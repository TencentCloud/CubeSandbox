// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectSnapshotPathsChecksMetaDirAndSnapshotStateDir(t *testing.T) {
	metaDir := t.TempDir()
	stateDir := snapshotStateDir(metaDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) failed: %v", stateDir, err)
	}

	status, err := inspectSnapshotPaths(metaDir)
	if err != nil {
		t.Fatalf("inspectSnapshotPaths returned error: %v", err)
	}
	if !status.metaDirExists {
		t.Fatalf("metaDirExists = false, want true")
	}
	if !status.snapshotStateExists {
		t.Fatalf("snapshotStateExists = false, want true")
	}
	if status.snapshotStatePath != filepath.Join(metaDir, "snapshot") {
		t.Fatalf("snapshotStatePath = %q, want %q", status.snapshotStatePath, filepath.Join(metaDir, "snapshot"))
	}
	if status.errorMessage != "" {
		t.Fatalf("errorMessage = %q, want empty", status.errorMessage)
	}
}

func TestInspectSnapshotPathsReportsMissingSnapshotStateDir(t *testing.T) {
	metaDir := t.TempDir()

	status, err := inspectSnapshotPaths(metaDir)
	if err != nil {
		t.Fatalf("inspectSnapshotPaths returned error: %v", err)
	}
	if !status.metaDirExists {
		t.Fatalf("metaDirExists = false, want true")
	}
	if status.snapshotStateExists {
		t.Fatalf("snapshotStateExists = true, want false")
	}
}

func TestInspectSnapshotPathsRejectsRelativeMetaDir(t *testing.T) {
	_, err := inspectSnapshotPaths("relative/meta")
	if err == nil {
		t.Fatal("expected inspectSnapshotPaths to reject relative path")
	}
}
