// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePackageMetadata(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, snapshotPackageMetadataFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readPackageMetadata(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, snapshotPackageMetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("metadata is not a JSON object: %v", err)
	}
	return fields
}

// A sandbox that was itself cold-booted captures the layer of the sandbox that
// created it, while its own container id is new. Recording the name is the only
// way the next cold boot finds the files.
func TestRecordWritableLayerKeepsTheRestOfTheDescriptor(t *testing.T) {
	dir := writePackageMetadata(t, `{"kernel_version":"v1","image_version":"v2",`+
		`"app_snapshot_container_id":"sandbox-x","vm_res":{"cpu":2,"memory":1073741824}}`)

	if err := recordWritableLayer(dir, " disk/tpl-1_0 "); err != nil {
		t.Fatalf("recordWritableLayer: %v", err)
	}

	fields := readPackageMetadata(t, dir)
	if got := string(fields[snapshotPackageWritableLayerKey]); got != `"disk/tpl-1_0"` {
		t.Fatalf("writable layer = %s, want the trimmed name", got)
	}
	for _, key := range []string{"kernel_version", "image_version", "app_snapshot_container_id", "vm_res"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("%q was dropped from the descriptor: %v", key, fields)
		}
	}
	// The VM shape must survive byte for byte: it is the guest's device layout.
	var vmRes struct {
		CPU    int   `json:"cpu"`
		Memory int64 `json:"memory"`
	}
	if err := json.Unmarshal(fields["vm_res"], &vmRes); err != nil {
		t.Fatal(err)
	}
	if vmRes.CPU != 2 || vmRes.Memory != 1073741824 {
		t.Fatalf("vm_res = %+v", vmRes)
	}
}

// An ordinary sandbox mounts "disk/<guest container id>", which is already the
// value cold boot derives, so the descriptor must not gain a redundant field.
func TestRecordWritableLayerSkipsOrdinarySandboxes(t *testing.T) {
	body := `{"app_snapshot_container_id":"tpl-1_0"}`
	dir := writePackageMetadata(t, body)

	if err := recordWritableLayer(dir, ""); err != nil {
		t.Fatalf("recordWritableLayer: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, snapshotPackageMetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("descriptor rewritten for an ordinary sandbox: %s", raw)
	}
}

func TestRecordWritableLayerRejectsAMissingDescriptor(t *testing.T) {
	if err := recordWritableLayer(t.TempDir(), "disk/tpl-1_0"); err == nil {
		t.Fatal("expected an error: the package has no descriptor to update")
	}
}

// A failed edit must not publish a half-written descriptor, and must not leave
// a staging file behind: the JFS uploader packages every regular file in the
// metadata directory.
func TestMutateSnapshotPackageMetadataKeepsTheFileOnFailure(t *testing.T) {
	const body = `{"vm_res":{"cpu":2}}`

	t.Run("mutate_error", func(t *testing.T) {
		dir := writePackageMetadata(t, body)

		err := mutateSnapshotPackageMetadata(dir, "snapshot metadata", func(map[string]json.RawMessage) error {
			return errors.New("boom")
		})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the caller's failure", err)
		}
		requireDescriptorUnchanged(t, dir, body)
	})

	t.Run("write_error", func(t *testing.T) {
		dir := writePackageMetadata(t, body)
		// A directory at the staging path makes os.WriteFile fail with EISDIR,
		// which is the same shape as ENOSPC/EDQUOT: the file was not published.
		if err := os.Mkdir(filepath.Join(dir, snapshotPackageMetadataFile+".tmp"), 0o755); err != nil {
			t.Fatal(err)
		}

		err := mutateSnapshotPackageMetadata(dir, "snapshot metadata", func(fields map[string]json.RawMessage) error {
			fields["rootfs_writable_layer"] = json.RawMessage(`"disk/x"`)
			return nil
		})
		if err == nil {
			t.Fatal("expected the write to fail")
		}
		requireDescriptorUnchanged(t, dir, body)
	})
}

// requireDescriptorUnchanged asserts the descriptor still has the original
// bytes and that no staging path survived.
func requireDescriptorUnchanged(t *testing.T, dir, body string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, snapshotPackageMetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("descriptor changed on failure: %s", raw)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("metadata directory has %d entries, want 1: %v", len(entries), entries)
	}
}
