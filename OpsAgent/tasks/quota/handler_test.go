// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/upstream"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/pkg/atomicfile"
)

// fakePuller stubs the CubeOps pull endpoint.
type fakePuller struct {
	resp *upstream.SpecResponse
	err  error
}

func (f *fakePuller) PullSpec(ctx context.Context, nodeID string) (*upstream.SpecResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func newTestDomain(t *testing.T) (*Domain, string) {
	t.Helper()
	dir := t.TempDir()
	confPath := filepath.Join(dir, "conf.yaml")
	if err := os.WriteFile(confPath, []byte(sampleConf), 0644); err != nil {
		t.Fatal(err)
	}
	d := New("node-a", confPath, 3, &fakePuller{resp: &upstream.SpecResponse{}})
	// Fixed host capacity so the overcommit guards are deterministic in tests
	// (16 cores, ~39GB), independent of the machine running the suite.
	d.physicalFn = func() (PhysicalTotals, error) {
		return PhysicalTotals{CpuTotal: 16, MemMBTotal: 39799}, nil
	}
	return d, confPath
}

func postPush(t *testing.T, d *Domain, body string) pushResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/quota", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	for pattern, h := range d.Routes() {
		if pattern == "POST /api/v1/config/quota" {
			h(rec, req)
			var out pushResponse
			if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			return out
		}
	}
	t.Fatal("push route not registered")
	return pushResponse{}
}

func TestPushWritesFileAndBacksUp(t *testing.T) {
	d, confPath := newTestDomain(t)
	resp := postPush(t, d, `{"request_id":"r1","revision":3,"spec":{"mcpu_limit":64000,"mem_limit":"64Gi"},"physical":{"cpu_total":16,"mem_mb_total":39799}}`)
	if !resp.Applied || resp.Error != "" {
		t.Fatalf("push failed: %+v", resp)
	}

	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mcpu_limit: 64000")) {
		t.Fatalf("file not updated:\n%s", data)
	}
	baks := atomicfile.ListBackups(confPath)
	if len(baks) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(baks))
	}
}

func TestPushSameSpecIsNoop(t *testing.T) {
	d, confPath := newTestDomain(t)
	body := `{"request_id":"r1","revision":1,"spec":{"mcpu_limit":64000},"physical":{"cpu_total":16}}`
	if resp := postPush(t, d, body); !resp.Applied || resp.Noop {
		t.Fatalf("first push should write: %+v", resp)
	}
	before, _ := os.ReadFile(confPath)
	resp := postPush(t, d, body)
	if !resp.Applied || !resp.Noop {
		t.Fatalf("second push should noop: %+v", resp)
	}
	after, _ := os.ReadFile(confPath)
	if !bytes.Equal(before, after) {
		t.Fatal("noop push must not touch the file")
	}
	if baks := atomicfile.ListBackups(confPath); len(baks) != 1 {
		t.Fatalf("noop push must not add backups, got %d", len(baks))
	}
}

func TestPushRejectsInvalidSpec(t *testing.T) {
	d, confPath := newTestDomain(t)
	before, _ := os.ReadFile(confPath)
	resp := postPush(t, d, `{"request_id":"r2","revision":1,"spec":{"mcpu_limit":999999999},"physical":{"cpu_total":16}}`)
	if resp.Applied || resp.Error == "" {
		t.Fatalf("invalid spec must be rejected: %+v", resp)
	}
	after, _ := os.ReadFile(confPath)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected push must not touch the file")
	}
}

func TestPushMissingSpec(t *testing.T) {
	d, _ := newTestDomain(t)
	resp := postPush(t, d, `{"request_id":"r3","revision":1}`)
	if resp.Applied || resp.Error == "" {
		t.Fatalf("missing spec must be rejected: %+v", resp)
	}
}

func TestReconcileAppliesAndSkips(t *testing.T) {
	d, confPath := newTestDomain(t)
	fp := &fakePuller{}
	d.puller = fp

	fp.resp = &upstream.SpecResponse{Managed: true, Revision: 5, Spec: &upstream.Spec{MCpuLimit: 32000}}
	d.Reconcile(context.Background())
	data, _ := os.ReadFile(confPath)
	if !bytes.Contains(data, []byte("mcpu_limit: 32000")) {
		t.Fatalf("reconcile did not apply:\n%s", data)
	}

	// Same revision: a hand-edited file is still healed (drift correction
	// must not depend on a revision bump).
	handEdited := strings.Replace(string(data), "mcpu_limit: 32000", "mcpu_limit: 1", 1)
	if err := os.WriteFile(confPath, []byte(handEdited), 0644); err != nil {
		t.Fatal(err)
	}
	d.Reconcile(context.Background())
	after, _ := os.ReadFile(confPath)
	if !bytes.Contains(after, []byte("mcpu_limit: 32000")) {
		t.Fatalf("same-revision reconcile must heal a hand-edited file:\n%s", after)
	}

	// Same revision, file already matching: no rewrite and no extra backup.
	baks := atomicfile.ListBackups(confPath)
	d.Reconcile(context.Background())
	after, _ = os.ReadFile(confPath)
	if !bytes.Contains(after, []byte("mcpu_limit: 32000")) {
		t.Fatalf("matching reconcile must keep the file:\n%s", after)
	}
	if got := atomicfile.ListBackups(confPath); len(got) != len(baks) {
		t.Fatalf("matching reconcile must not add backups, got %d want %d", len(got), len(baks))
	}

	// New revision: heals the manual edit.
	fp.resp = &upstream.SpecResponse{Managed: true, Revision: 6, Spec: &upstream.Spec{MCpuLimit: 48000}}
	d.Reconcile(context.Background())
	data, _ = os.ReadFile(confPath)
	if !bytes.Contains(data, []byte("mcpu_limit: 48000")) {
		t.Fatalf("new revision did not heal the file:\n%s", data)
	}

	// A structurally broken file must be rejected, not overwritten: the agent
	// cannot merge into a document without the host.quota anchor.
	if err := os.WriteFile(confPath, []byte("not: a valid dynamicconf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fp.resp = &upstream.SpecResponse{Managed: true, Revision: 7, Spec: &upstream.Spec{MCpuLimit: 32000}}
	d.Reconcile(context.Background())
	after, _ = os.ReadFile(confPath)
	if string(after) != "not: a valid dynamicconf\n" {
		t.Fatal("reconcile on a broken file must surface an error instead of overwriting")
	}
}

func TestReconcileUnmanagedLeavesFileAlone(t *testing.T) {
	d, confPath := newTestDomain(t)
	d.puller = &fakePuller{resp: &upstream.SpecResponse{Managed: false}}
	before, _ := os.ReadFile(confPath)
	d.Reconcile(context.Background())
	after, _ := os.ReadFile(confPath)
	if !bytes.Equal(before, after) {
		t.Fatal("unmanaged reconcile must not touch the file")
	}
}

func TestApplyConcurrent(t *testing.T) {
	d, confPath := newTestDomain(t)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := &Spec{MCpuLimit: int64(1000 + i)}
			if _, err := d.apply(spec, PhysicalTotals{CpuTotal: 16}); err != nil {
				t.Errorf("apply %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	// The file must end up matching one of the concurrent specs (no torn write).
	var ok bool
	for i := 0; i < n; i++ {
		if matched, _ := Matches(string(data), &Spec{MCpuLimit: int64(1000 + i)}); matched {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("file does not match any concurrent spec:\n%s", data)
	}
}
