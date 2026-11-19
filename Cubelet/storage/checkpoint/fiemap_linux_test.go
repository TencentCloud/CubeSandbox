// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// reflinkCopy clones src to dst, or skips the test where the filesystem
// cannot share blocks.
func reflinkCopy(t *testing.T, src, dst string) {
	t.Helper()
	s, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := unix.IoctlFileClone(int(d.Fd()), int(s.Fd())); err != nil {
		t.Skipf("reflink is not available here (%v); run on XFS with reflink=1", err)
	}
}

func TestBaselineWindowsAreReusedWithoutReading(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestClient(t)
	dir := t.TempDir()
	mem := map[int64][]byte{}
	for i := int64(0); i < 8; i++ {
		mem[i*MiB] = randBytes(int(MiB))
	}
	writeWindows(t, filepath.Join(dir, "memory"), 8*MiB, mem)
	writeWindows(t, filepath.Join(dir, "rootfs"), 32*MiB, map[int64][]byte{0: randBytes(int(MiB))})
	opt := IncrementalOptions{SandboxID: "sb-f", Generation: 1}
	_, m1, _, err := ExportIncremental(ctx, c, "snap-f1", "s1", incVolumes(dir), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	// The baseline shares every block with the exported files; the next
	// package is cloned from them and the guest rewrites one window.
	base := t.TempDir()
	for _, k := range []string{"rootfs", "memory"} {
		reflinkCopy(t, filepath.Join(dir, k), filepath.Join(base, k))
	}
	patchFile(t, filepath.Join(dir, "memory"), map[int64][]byte{5 * MiB: randBytes(4096)})
	vols := incVolumes(dir)
	for i := range vols {
		vols[i].BaselinePath = filepath.Join(base, vols[i].Kind)
		vols[i].BaseChunks = m1.FindFile(RoleVolume, vols[i].Name).Chunks
	}
	opt.Known, opt.Generation = m1.ReusableChunks(c.Prefix()), 2
	_, m2, st, err := ExportIncremental(ctx, c, "snap-f2", "s1", vols, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadBytes != MiB {
		t.Fatalf("read %d bytes, want only the rewritten 1 MiB window", st.ReadBytes)
	}
	if st.UploadedObjects != 1 || st.ReusedBytes != m2.DataBytes()-MiB {
		t.Fatalf("uploaded %d, reused %d of %d", st.UploadedObjects, st.ReusedBytes, m2.DataBytes())
	}

	// A baseline that is not the same size is not trusted: every window
	// with data is read again.
	if err := os.Truncate(filepath.Join(base, "memory"), 4*MiB); err != nil {
		t.Fatal(err)
	}
	opt.Known, opt.Generation = m2.ReusableChunks(c.Prefix()), 3
	_, _, st, err = ExportIncremental(ctx, c, "snap-f3", "s1", vols, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadBytes < 8*MiB || st.UploadedObjects != 0 {
		t.Fatalf("read %d bytes and uploaded %d objects without a usable baseline", st.ReadBytes, st.UploadedObjects)
	}
}
