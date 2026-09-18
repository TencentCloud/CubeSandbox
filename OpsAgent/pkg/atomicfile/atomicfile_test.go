// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFirstWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.yaml")
	if err := Replace(path, "a: 1\n"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "a: 1\n" {
		t.Fatalf("content = %q err=%v", data, err)
	}
	// First-ever write uses the default 0644.
	if info, _ := os.Stat(path); info.Mode().Perm() != 0644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestReplaceKeepsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, "new\n"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600 preserved", info.Mode().Perm())
	}
}

func TestBackupMissingSourceIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	bak, err := Backup(path, 5)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if bak != "" {
		t.Fatalf("missing source must not produce a backup, got %q", bak)
	}
}

func TestBackupPrunesRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.yaml")
	if err := os.WriteFile(path, []byte("v0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Write several versions; each Backup keeps at most keep=2.
	for i := 1; i <= 5; i++ {
		if _, err := Backup(path, 2); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	baks := ListBackups(path)
	if len(baks) != 2 {
		t.Fatalf("backups = %d, want 2 after rotation", len(baks))
	}
}

func TestBackupWritesReadableCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.yaml")
	if err := os.WriteFile(path, []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bak, err := Backup(path, 5)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	data, err := os.ReadFile(bak)
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("backup content = %q err=%v", data, err)
	}
}
