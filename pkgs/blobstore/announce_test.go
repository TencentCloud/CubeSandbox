// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

import (
	"os"
	"testing"
)

func TestAnnounceBackendMismatch(t *testing.T) {
	root := t.TempDir()
	if err := AnnounceBackend(root, "fs"); err != nil {
		t.Fatalf("first announce: %v", err)
	}
	if err := AnnounceBackend(root, "fs"); err != nil {
		t.Fatalf("same backend: %v", err)
	}
	if err := AnnounceBackend(root, "s3"); err == nil {
		t.Fatal("expected mismatch error")
	}
	got, err := os.ReadFile(backendMarkerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fs\n" {
		t.Fatalf("marker = %q", got)
	}
}

func TestAnnounceBackendEmptyRoot(t *testing.T) {
	if err := AnnounceBackend("", "fs"); err != nil {
		t.Fatalf("empty root should be no-op: %v", err)
	}
}

func TestAnnounceIfFSS3DoesNotWriteMarker(t *testing.T) {
	root := t.TempDir()
	if err := AnnounceIfFS(root, "s3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backendMarkerPath(root)); !os.IsNotExist(err) {
		t.Fatal("s3 must not write a backend marker")
	}
}

func TestAnnounceBackendS3MarkerAllowsFS(t *testing.T) {
	root := t.TempDir()
	if err := AnnounceBackend(root, "s3"); err != nil {
		t.Fatal(err)
	}
	if err := AnnounceBackend(root, "fs"); err != nil {
		t.Fatalf("s3 marker must not block fs: %v", err)
	}
	got, err := os.ReadFile(backendMarkerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fs\n" {
		t.Fatalf("marker = %q", got)
	}
}
