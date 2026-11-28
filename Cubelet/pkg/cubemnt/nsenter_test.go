// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux && !gccgo

package cubemnt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnterNamespacePathSkipsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mnt")
	if rc := enterNamespacePath(missing); rc != 0 {
		t.Fatalf("enterNamespacePath(%q) = %d, want 0 for ENOENT", missing, rc)
	}
}

func TestEnterNamespacePathSkipsRegularNonNSFSFile(t *testing.T) {
	placeholder := filepath.Join(t.TempDir(), "mnt")
	if err := os.WriteFile(placeholder, nil, 0600); err != nil {
		t.Fatalf("create placeholder file: %v", err)
	}
	if rc := enterNamespacePath(placeholder); rc != 0 {
		t.Fatalf("enterNamespacePath(%q) = %d, want 0 for non-nsfs file", placeholder, rc)
	}
}

func TestEnterNamespacePathFailsOnNonENOENTOpenError(t *testing.T) {
	// Traversing a regular file returns ENOTDIR regardless of uid.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}
	invalid := filepath.Join(blocker, "mnt")
	if rc := enterNamespacePath(invalid); rc != -1 {
		t.Fatalf("enterNamespacePath(%q) = %d, want -1 for ENOTDIR", invalid, rc)
	}
}
