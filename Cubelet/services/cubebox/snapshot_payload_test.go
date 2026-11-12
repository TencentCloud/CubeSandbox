// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func TestParseCommitSnapshotMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    snapshotmode.Mode
		wantErr bool
	}{
		{raw: "", want: snapshotmode.Full},
		{raw: "full", want: snapshotmode.Full},
		{raw: "fs-only", want: snapshotmode.FsOnly},
		{raw: " FS-ONLY ", want: snapshotmode.FsOnly},
		{raw: "filesystem", want: snapshotmode.FsOnly},
		{raw: "incremental", wantErr: true},
		{raw: "filesystem-only", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseCommitSnapshotMode(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q: expected error", tc.raw)
			}
			if !strings.Contains(err.Error(), snapshotmode.ErrSnapshotModeInvalid) {
				t.Fatalf("%q: error %q must carry the stable identifier", tc.raw, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("%q: mode = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestValidateCommitPayload(t *testing.T) {
	memory := &storage.CowSnapshotObject{Name: "tpl-1-memory"}
	cases := []struct {
		name    string
		payload snapshotPayload
		mode    snapshotmode.Mode
		wantErr bool
	}{
		{name: "full with memory", payload: snapshotPayload{memory: memory}, mode: snapshotmode.Full},
		{name: "full without memory", payload: snapshotPayload{}, mode: snapshotmode.Full, wantErr: true},
		{name: "fs-only without memory", payload: snapshotPayload{}, mode: snapshotmode.FsOnly},
		{name: "fs-only with memory", payload: snapshotPayload{memory: memory}, mode: snapshotmode.FsOnly, wantErr: true},
	}
	for _, tc := range cases {
		err := validateCommitPayload(tc.payload, tc.mode)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestFsOnlySnapshotCaptureHasNoMemoryVolume(t *testing.T) {
	raw, err := buildFrozenSnapshotConfig(
		"snap-1", "/snapshots/snap-1", "/dev/cube-memory", "full", "/dev/cube-rootfs", snapshotmode.FsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["memory_vol_url"]; ok {
		t.Fatalf("fs-only payload must not carry a memory volume: %s", raw)
	}
	if payload["snapshot_mode"] != "fs-only" {
		t.Fatalf("snapshot_mode = %v, want fs-only", payload["snapshot_mode"])
	}
	if payload["rootfs_host_path"] != "/dev/cube-rootfs" {
		t.Fatalf("rootfs_host_path = %v", payload["rootfs_host_path"])
	}
}

func TestFullSnapshotCaptureKeepsLegacyPayloadShape(t *testing.T) {
	raw, err := buildFrozenSnapshotConfig(
		"snap-1", "/snapshots/snap-1", "/dev/cube-memory", "full", "/dev/cube-rootfs", snapshotmode.Full)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["memory_vol_url"] != "file:///dev/cube-memory" {
		t.Fatalf("memory_vol_url = %v", payload["memory_vol_url"])
	}
	// Older shims must keep seeing the exact same keys they always did.
	if _, ok := payload["snapshot_mode"]; ok {
		t.Fatalf("full payload must stay byte-compatible: %s", raw)
	}
	if _, ok := payload["rootfs_host_path"]; ok {
		t.Fatalf("full payload must stay byte-compatible: %s", raw)
	}
}

func TestFilesystemOnlyPrecondition(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("snapshot failed: " + snapshotmode.ErrFilesystemOnlyFreezeUnsupported + ": no agent"), true},
		{errors.New("snapshot failed: " + snapshotmode.ErrFilesystemOnlyRootfsUnsupported + ": no disk"), true},
		{errors.New("snapshot failed: " + snapshotmode.ErrFilesystemOnlyFreezeFailed + ": ioctl"), false},
		{errors.New("snapshot failed: " + snapshotmode.ErrFilesystemOnlyThawFailed + ": ioctl"), false},
	}
	for _, tc := range cases {
		if got := filesystemOnlyPrecondition(tc.err); got != tc.want {
			t.Fatalf("filesystemOnlyPrecondition(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
