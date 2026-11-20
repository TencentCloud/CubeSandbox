// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMatchMountRequiresWritableVolume(t *testing.T) {
	const target = "/volumes/nfs"
	const source = "server:/export/session-1"
	for _, tt := range []struct {
		name, options, superOptions, source string
		allow                               bool
	}{
		{"read-only bind", "ro,nosuid,nodev", "rw,vers=3", source, false},
		{"read-only filesystem", "rw,nosuid,nodev", "ro,vers=3", source, false},
		{"read-write", "rw,nosuid,nodev", "rw,vers=3", source, true},
		{"different export", "rw,nosuid,nodev", "rw,vers=3", "server:/other", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			line := "12 1 0:20 / " + target + " " + tt.options + " - nfs " + tt.source + " " + tt.superOptions + "\n"
			found, err := matchMount(line, target, source)
			if tt.allow != (found && err == nil) || !tt.allow && err == nil {
				t.Fatalf("unexpected mount check: found=%v err=%v", found, err)
			}
		})
	}
}

func TestVolumeSourceUsesVolumeDirectory(t *testing.T) {
	const id = "session-1"
	source, err := volumeSource("server:/exports/volumes", id)
	if err != nil || source != "server:/exports/volumes/session-1" {
		t.Fatalf("unexpected source: %q, %v", source, err)
	}
	if _, err := run(t.Context(), options{op: "attach", id: "../other"}); err == nil || !strings.Contains(err.Error(), "invalid NFS volume ID") {
		t.Fatalf("path-like volume ID was not rejected: %v", err)
	}
}

func TestAttachmentPathsPreserveConfiguredBase(t *testing.T) {
	parent := t.TempDir()
	realBase := filepath.Join(parent, "real")
	if err := os.Mkdir(realBase, 0755); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(parent, "configured")
	if err := os.Symlink(realBase, configured); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create a directory symlink: %v", err)
		}
		t.Fatal(err)
	}
	base, target, hostPath, err := attachmentPaths(configured, "test", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	wantBase, err := filepath.EvalSymlinks(realBase)
	if err != nil || base != wantBase {
		t.Fatalf("resolved base = %q, want %q, err = %v", base, wantBase, err)
	}
	relative, err := filepath.Rel(configured, hostPath)
	if err != nil || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.Join(base, relative) != filepath.Join(target, dataDirName) {
		t.Fatalf("host_path %q must use configured base %q and resolve into %q", hostPath, configured, target)
	}
}

func TestVolumeRegistrationRetainsClaimAndData(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBE_NFS_CONTROL_ROOT", root)
	const id = "session-1"
	dir := filepath.Join(root, id)
	create := options{op: "create", id: id}
	if _, err := run(t.Context(), create); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "explicit name") || strings.Contains(err.Error(), root) {
		t.Fatalf("Create must require a provisioned directory, got %v", err)
	}
	data := filepath.Join(dir, dataDirName)
	if err := os.MkdirAll(data, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(data, "file.txt")
	if err := os.WriteFile(file, []byte("existing data"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := run(t.Context(), create)
	if err != nil || !bindingPattern.MatchString(first.PrivateData) {
		t.Fatalf("missing registration binding: %+v, %v", first, err)
	}
	if err := validateBinding(dir, first.PrivateData); err != nil {
		t.Fatal(err)
	}
	other := first.PrivateData[:31] + "0"
	if other == first.PrivateData {
		other = first.PrivateData[:31] + "1"
	}
	if err := validateBinding(dir, other); err == nil {
		t.Fatal("a different registration was allowed to attach the directory")
	}
	if _, err := run(t.Context(), options{op: "destroy", id: id, privateData: first.PrivateData}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t.Context(), create); err == nil {
		t.Fatal("the directory could be claimed again after Destroy")
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "existing data" {
		t.Fatalf("retained data was modified: %q, %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(data, bindingFile)); !os.IsNotExist(err) {
		t.Fatalf("binding must stay outside the data directory, got %v", err)
	}
}
