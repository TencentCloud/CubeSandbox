// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package jfsdiff

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// reflinkDir returns a directory on a reflink-capable filesystem (XFS with
// reflink=1 or Btrfs). Set JFSDIFF_REFLINK_DIR to run these tests.
func reflinkDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("JFSDIFF_REFLINK_DIR")
	if dir == "" {
		t.Skip("JFSDIFF_REFLINK_DIR not set (needs XFS reflink=1 or Btrfs)")
	}
	d, err := os.MkdirTemp(dir, "jfsdiff-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func ficlone(t *testing.T, src, dst string) {
	t.Helper()
	s, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := unix.IoctlFileClone(int(d.Fd()), int(s.Fd())); err != nil {
		t.Fatalf("FICLONE %s -> %s: %v", src, dst, err)
	}
}

func sum(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeAt(t *testing.T, p string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func punch(t *testing.T, p string, off, n int64) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := punchHole(f, off, n); err != nil {
		t.Fatal(err)
	}
}

// roundTrip publishes pkg relative to replica and rebuilds it on a fresh clone
// of replica2 (a separately copied replica, as on another node).
func roundTrip(t *testing.T, dir, pkg, replica, replica2 string) *Diff {
	t.Helper()
	d, err := Compute(pkg, replica)
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dir, "rootfs.diff")
	blob, err := os.Create(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	segs, n, _, err := WriteBlob(pkg, d, blob)
	if err != nil {
		t.Fatal(err)
	}
	blob.Close()
	m := &Manifest{Version: ManifestVersion, SizeBytes: d.SizeBytes, Blob: "rootfs.diff", BlobBytes: n, Segments: segs, Holes: d.Holes}
	raw, _ := json.Marshal(m)
	mp := filepath.Join(dir, "rootfs.diff.json")
	if err := WriteFileAtomic(mp, raw); err != nil {
		t.Fatal(err)
	}
	m2, err := ReadManifest(mp)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "imported")
	ficlone(t, replica2, out)
	if _, err := Apply(out, blobPath, m2); err != nil {
		t.Fatal(err)
	}
	if sum(t, out) != sum(t, pkg) {
		t.Fatalf("imported content differs from package")
	}
	return d
}

func TestRoundTripScenarios(t *testing.T) {
	dir := reflinkDir(t)
	const size = 64 << 20
	rnd := rand.New(rand.NewSource(7))
	// A template with sparse data and an all-zero data region.
	tpl := filepath.Join(dir, "template")
	f, _ := os.Create(tpl)
	f.Truncate(size)
	data := make([]byte, 4<<20)
	rnd.Read(data)
	f.WriteAt(data, 0)
	f.WriteAt(make([]byte, 2<<20), 8<<20) // allocated zeros
	f.WriteAt(data[:1<<20], 20<<20)
	f.Sync()
	f.Close()

	replica := filepath.Join(dir, "replica")
	if _, w, err := ZeroSkipCopy(tpl, replica); err != nil || w != 5<<20 {
		t.Fatalf("ZeroSkipCopy wrote %d err %v", w, err)
	}
	if sum(t, tpl) != sum(t, replica) {
		t.Fatal("replica content differs from template")
	}
	replica2 := filepath.Join(dir, "replica2")
	if _, _, err := ZeroSkipCopy(tpl, replica2); err != nil {
		t.Fatal(err)
	}

	blk := make([]byte, 4096)
	t.Run("random writes, post-pause writes do not leak", func(t *testing.T) {
		sb := filepath.Join(dir, "sb1")
		pkg := filepath.Join(dir, "pkg1")
		ficlone(t, replica, sb)
		for i := 0; i < 500; i++ {
			rnd.Read(blk)
			writeAt(t, sb, int64(rnd.Intn(size/4096))*4096, blk)
		}
		ficlone(t, sb, pkg)
		for i := 0; i < 200; i++ {
			rnd.Read(blk)
			writeAt(t, sb, int64(rnd.Intn(size/4096))*4096, blk)
		}
		sub := filepath.Join(dir, "rt1")
		os.Mkdir(sub, 0o755)
		d := roundTrip(t, sub, pkg, replica, replica2)
		if d.DataBytes() == 0 || d.DataBytes() > 500*4096 {
			t.Fatalf("unexpected diff size %d", d.DataBytes())
		}
	})
	t.Run("discard, identical rewrite, zero-region write, growth", func(t *testing.T) {
		sb := filepath.Join(dir, "sb2")
		pkg := filepath.Join(dir, "pkg2")
		ficlone(t, replica, sb)
		punch(t, sb, 1<<20, 1<<20)
		writeAt(t, sb, 2<<20, data[2<<20:(2<<20)+4096])
		writeAt(t, sb, 9<<20, []byte("x"))
		os.Truncate(sb, size+(4<<20))
		writeAt(t, sb, size+(1<<20), data[:4096])
		ficlone(t, sb, pkg)
		sub := filepath.Join(dir, "rt2")
		os.Mkdir(sub, 0o755)
		d := roundTrip(t, sub, pkg, replica, replica2)
		if len(d.Holes) == 0 {
			t.Fatal("expected a hole range for the discarded template data")
		}
	})
	t.Run("two generations", func(t *testing.T) {
		sb := filepath.Join(dir, "sb3")
		p1 := filepath.Join(dir, "pkg3a")
		sb2 := filepath.Join(dir, "sb3b")
		p2 := filepath.Join(dir, "pkg3b")
		ficlone(t, replica, sb)
		for i := 0; i < 300; i++ {
			rnd.Read(blk)
			writeAt(t, sb, int64(rnd.Intn(size/4096))*4096, blk)
		}
		ficlone(t, sb, p1)
		os.Remove(sb)
		ficlone(t, p1, sb2)
		for i := 0; i < 300; i++ {
			rnd.Read(blk)
			writeAt(t, sb2, int64(rnd.Intn(size/4096))*4096, blk)
		}
		ficlone(t, sb2, p2)
		sub := filepath.Join(dir, "rt3")
		os.Mkdir(sub, 0o755)
		roundTrip(t, sub, p2, replica, replica2)
	})
	t.Run("large single write over 2GiB-safe loop", func(t *testing.T) {
		sb := filepath.Join(dir, "sb4")
		pkg := filepath.Join(dir, "pkg4")
		ficlone(t, replica, sb)
		big := bytes.Repeat(data[:1<<20], 24)
		writeAt(t, sb, 32<<20, big)
		ficlone(t, sb, pkg)
		sub := filepath.Join(dir, "rt4")
		os.Mkdir(sub, 0o755)
		roundTrip(t, sub, pkg, replica, replica2)
	})
}

func TestWriteBlobPreferringLiveSource(t *testing.T) {
	dir := reflinkDir(t)
	const size = 64 << 20
	rnd := rand.New(rand.NewSource(11))
	tpl := filepath.Join(dir, "template")
	f, _ := os.Create(tpl)
	f.Truncate(size)
	data := make([]byte, 4<<20)
	rnd.Read(data)
	f.WriteAt(data, 0)
	f.Sync()
	f.Close()
	replica := filepath.Join(dir, "replica")
	replica2 := filepath.Join(dir, "replica2")
	ZeroSkipCopy(tpl, replica)
	ZeroSkipCopy(tpl, replica2)

	sb := filepath.Join(dir, "sb")
	pkg := filepath.Join(dir, "pkg")
	ficlone(t, replica, sb)
	big := make([]byte, 16<<20)
	rnd.Read(big)
	writeAt(t, sb, 8<<20, big) // 16 MiB the guest wrote before the pause
	blk := make([]byte, 4096)
	for i := 0; i < 200; i++ {
		rnd.Read(blk)
		writeAt(t, sb, int64(rnd.Intn(size/4096))*4096, blk)
	}
	ficlone(t, sb, pkg)
	// The sandbox keeps running (snapshot case): rewrite part of what the
	// package captured and some new places, then it is destroyed.
	rnd.Read(blk)
	writeAt(t, sb, 9<<20, bytes.Repeat(blk, 64)) // 256 KiB inside the captured 16 MiB
	writeAt(t, sb, 40<<20, blk)
	live, err := os.Open(sb)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	os.Remove(sb)

	d, err := Compute(pkg, replica)
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dir, "blob")
	blob, _ := os.Create(blobPath)
	segs, n, _, fromAlt, err := WriteBlobPreferring(pkg, live, d, blob)
	blob.Close()
	if err != nil {
		t.Fatal(err)
	}
	if fromAlt == 0 || fromAlt >= n {
		t.Fatalf("fromAlt=%d of %d: expected most but not all bytes from the live source", fromAlt, n)
	}
	m := &Manifest{Version: ManifestVersion, SizeBytes: d.SizeBytes, BlobBytes: n, Segments: segs, Holes: d.Holes}
	out := filepath.Join(dir, "out")
	ficlone(t, replica2, out)
	if _, err := Apply(out, blobPath, m); err != nil {
		t.Fatal(err)
	}
	if sum(t, out) != sum(t, pkg) {
		t.Fatal("blob built from the live source does not reproduce the package")
	}
}

func TestComputeWithoutBaseRebuildsFromEmpty(t *testing.T) {
	dir := reflinkDir(t)
	src := filepath.Join(dir, "src")
	f, _ := os.Create(src)
	f.Truncate(16 << 20)
	f.WriteAt([]byte("hello"), 5<<20)
	f.Sync()
	f.Close()
	d, err := Compute(src, "")
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dir, "blob")
	blob, _ := os.Create(blobPath)
	segs, n, _, err := WriteBlob(src, d, blob)
	blob.Close()
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, nil, 0o644)
	m := &Manifest{Version: ManifestVersion, SizeBytes: d.SizeBytes, BlobBytes: n, Segments: segs}
	if _, err := Apply(empty, blobPath, m); err != nil {
		t.Fatal(err)
	}
	if sum(t, empty) != sum(t, src) {
		t.Fatal("rebuilt content differs")
	}
}
