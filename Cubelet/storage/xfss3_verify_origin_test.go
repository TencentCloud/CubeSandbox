// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// verifyTestCheckpoint commits a pause package of snap, exported by node
// origin, to a fake store and points the xfs-s3 client and node id hooks at
// it. Readback checks presence only.
func verifyTestCheckpoint(t *testing.T, snap, origin string) *sweepTestStore {
	t.Helper()
	store := &sweepTestStore{objects: map[string][]byte{}}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	client, err := checkpoint.NewClient(checkpoint.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
		PathStyle: true, Prefix: "ckpt", Verify: checkpoint.VerifyExistence})
	if err != nil {
		t.Fatal(err)
	}
	prevVal, prevNode := xfss3ClientVal, xfss3NodeID
	t.Cleanup(func() { xfss3ClientVal, xfss3NodeID = prevVal, prevNode })
	xfss3ClientVal = client

	home := t.TempDir()
	entry := &SnapshotCatalogEntry{SnapshotID: snap, Kind: CatalogKindPauseSnapshot,
		RootfsVol: cowTemplateRootfsName(snap), MemoryVol: cowTemplateMemoryName(snap)}
	catalog, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var files []checkpoint.SourceFile
	add := func(role, name string, data []byte) {
		p := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, checkpoint.SourceFile{Role: role, Name: name, Path: p})
	}
	add(checkpoint.RoleVolume, entry.RootfsVol, []byte("rootfs"))
	add(checkpoint.RoleVolume, entry.MemoryVol, []byte("memory"))
	for _, name := range []string{snapshotCatalogFileName, "metadata.json", "snapshot/config.json", "snapshot/state.json", "sandbox_spec.json"} {
		add(checkpoint.RoleMeta, name, []byte("{}"))
	}
	attrs := map[string]string{xfss3CatalogAttr: string(catalog), xfss3OriginAttr: origin}
	if _, _, err := checkpoint.Export(context.Background(), client, snap, "s1", files,
		checkpoint.ExportOptions{Attributes: attrs}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestXFSS3ReadbackKnowsTheOriginByNodeID(t *testing.T) {
	const snap = "snap-origin0000000000000000001"
	verifyTestCheckpoint(t, snap, "node-a")
	// Two nodes may share a hostname; only the node id names the origin.
	xfss3NodeID = func() (string, error) { return "node-a", nil }
	out := verifyXFSS3(context.Background(), snap, &checkpoint.Progress{})
	if out.state != cow.RemoteStatePending || !strings.HasPrefix(out.msg, "retry(isolation)") {
		t.Fatalf("the origin must not read its own checkpoint back: %+v", out)
	}

	xfss3NodeID = func() (string, error) { return "", errors.New("no identity") }
	out = verifyXFSS3(context.Background(), snap, &checkpoint.Progress{})
	if out.state != cow.RemoteStatePending || !strings.HasPrefix(out.msg, "retry(isolation)") {
		t.Fatalf("a node that cannot name itself cannot tell it is not the origin: %+v", out)
	}

	xfss3NodeID = func() (string, error) { return "node-b", nil }
	out = verifyXFSS3(context.Background(), snap, &checkpoint.Progress{})
	if out.state != cow.RemoteStateReady {
		t.Fatalf("another node reads it back: %+v", out)
	}
}

func TestXFSS3CorruptReadbackIsCheckedAgain(t *testing.T) {
	const snap = "snap-corrupt000000000000000001"
	store := verifyTestCheckpoint(t, snap, "node-a")
	xfss3NodeID = func() (string, error) { return "node-b", nil }
	store.mu.Lock()
	for k := range store.objects {
		if strings.Contains(k, "/c/") {
			delete(store.objects, k)
			break
		}
	}
	store.mu.Unlock()

	out := verifyXFSS3(context.Background(), snap, &checkpoint.Progress{})
	if out.state != cow.RemoteStateFailed || !out.recheck {
		t.Fatalf("a missing chunk is a corrupt verdict read from the store: %+v", out)
	}
	ver := &xfss3Verify{}
	ver.noteVerifyOutcome(out)
	if ver.state != cow.RemoteStatePending || !ver.rechecking || time.Until(ver.notBefore) < xfss3CorruptRecheck-time.Second {
		t.Fatalf("one corrupt readback must not fail the checkpoint: %+v", ver)
	}
	ver.noteVerifyOutcome(out)
	if ver.state != cow.RemoteStateFailed {
		t.Fatalf("a second corrupt readback fails it: %+v", ver)
	}
}

func TestXFSS3CorruptReadbackThatClearsIsForgotten(t *testing.T) {
	corrupt := xfss3VerifyOutcome{state: cow.RemoteStateFailed, msg: "chunk missing", recheck: true}
	ver := &xfss3Verify{}
	ver.noteVerifyOutcome(corrupt)
	ver.noteVerifyOutcome(xfss3VerifyOutcome{state: cow.RemoteStatePending, msg: "store unreachable", backoff: true})
	if ver.rechecking {
		t.Fatal("a readback that did not find corruption clears the recheck")
	}
	ver.noteVerifyOutcome(corrupt)
	if ver.state != cow.RemoteStatePending {
		t.Fatalf("corruption found again after a clean readback is checked again: %+v", ver)
	}

	// A defect the manifest itself shows needs no second look.
	ver = &xfss3Verify{}
	ver.noteVerifyOutcome(xfss3VerifyOutcome{state: cow.RemoteStateFailed, msg: "manifest carries no catalog"})
	if ver.state != cow.RemoteStateFailed {
		t.Fatalf("a manifest defect fails at once: %+v", ver)
	}
}

func TestXFSS3ExportStopsOnFilesNoCheckpointCanCarry(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("metadata path %q has an invalid component: %w", ".hidden", checkpoint.ErrUnexportable),
		fmt.Errorf("commit candidate: %w", checkpoint.ErrConflict),
	} {
		if !xfss3ExportCannotSucceed(err) {
			t.Errorf("%v must stop the export", err)
		}
	}
	for _, err := range []error{nil, errors.New("s3 PUT: 503"), fmt.Errorf("open: %w", os.ErrNotExist)} {
		if xfss3ExportCannotSucceed(err) {
			t.Errorf("%v must be retried", err)
		}
	}
}
