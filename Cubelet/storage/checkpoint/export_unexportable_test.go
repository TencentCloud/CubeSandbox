// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestExportMarksFilesItCannotCarry(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	vol := filepath.Join(dir, "rootfs")
	meta := filepath.Join(dir, "meta")
	writeFile(t, vol, minChunkSize, map[int64][]byte{0: []byte("data")})
	writeFile(t, meta, 16, map[int64][]byte{0: []byte("meta")})
	good := SourceFile{Role: RoleVolume, Name: "rootfs", Path: vol}

	for name, bad := range map[string]SourceFile{
		"hidden metadata file": {Role: RoleMeta, Name: ".hidden", Path: meta},
		"metadata path escape": {Role: RoleMeta, Name: "../meta", Path: meta},
		"volume name":          {Role: RoleVolume, Name: "root fs", Path: vol},
		"unknown role":         {Role: "exec", Name: "meta", Path: meta},
		"not a regular file":   {Role: RoleMeta, Name: "dir", Path: dir},
	} {
		_, _, err := Export(ctx, c, "snap-u", "s1", []SourceFile{good, bad}, ExportOptions{ChunkSize: minChunkSize})
		if !errors.Is(err, ErrUnexportable) {
			t.Errorf("full export, %s: %v", name, err)
		}
	}

	opt := IncrementalOptions{SandboxID: "sb-u"}
	vols := []IncrementalVolume{{Kind: "rootfs", Name: "rootfs", Path: vol, ChunkSize: minChunkSize}}
	_, _, _, err := ExportIncremental(ctx, c, "snap-u", "s1", vols,
		[]SourceFile{{Role: RoleMeta, Name: ".hidden", Path: meta}}, opt)
	if !errors.Is(err, ErrUnexportable) {
		t.Errorf("incremental export, hidden metadata file: %v", err)
	}
	vols[0].Name = "root fs"
	if _, _, _, err := ExportIncremental(ctx, c, "snap-u", "s1", vols, nil, opt); !errors.Is(err, ErrUnexportable) {
		t.Errorf("incremental export, volume name: %v", err)
	}
}
