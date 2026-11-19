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
)

func pathIn(dir string) func(f *File) (string, error) {
	return func(f *File) (string, error) { return filepath.Join(dir, f.Name), nil }
}

func TestMaterializeFromALocalBaseFetchesOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestClient(t)
	src := t.TempDir()
	mem := map[int64][]byte{}
	for i := int64(0); i < 8; i++ {
		mem[i*MiB] = randBytes(int(MiB))
	}
	writeWindows(t, filepath.Join(src, "memory"), 8*MiB, mem)
	// The rootfs window at 32 MiB holds nothing, so no checkpoint names it.
	writeWindows(t, filepath.Join(src, "rootfs"), 48*MiB, map[int64][]byte{0: randBytes(4096), 17 * MiB: randBytes(4096)})
	opt := IncrementalOptions{SandboxID: "sb-m", Generation: 1}
	_, m1, _, err := ExportIncremental(ctx, c, "snap-m1", "s1", incVolumes(src), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	// This node imported the first checkpoint and kept it as its base.
	base := t.TempDir()
	if err := Materialize(ctx, c, m1, 2, nil, pathIn(base)); err != nil {
		t.Fatal(err)
	}
	reflinkCopy(t, filepath.Join(base, "tpl-snap-x-memory"), filepath.Join(t.TempDir(), "probe"))

	// The guest rewrites one memory window and frees another, which the
	// next checkpoint no longer names.
	patchFile(t, filepath.Join(src, "memory"), map[int64][]byte{3*MiB + 4096: randBytes(4096)})
	h, err := os.OpenFile(filepath.Join(src, "memory"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := punchHole(h, 6*MiB, MiB); err != nil {
		t.Fatal(err)
	}
	h.Close()
	opt.Known, opt.Generation = m1.ReusableChunks(c.Prefix()), 2
	_, m2, _, err := ExportIncremental(ctx, c, "snap-m2", "s1", incVolumes(src), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	// Where the base sat when this node saved it.
	saved := map[string][]Extent{}
	for _, v := range incVolumes(src) {
		if saved[v.Name], err = ExtentMap(filepath.Join(base, v.Name)); err != nil {
			t.Fatal(err)
		}
	}
	trust := false
	baseFor := func(f *File) *LocalBase {
		return &LocalBase{Path: filepath.Join(base, f.Name), Chunks: m1.FindFile(RoleVolume, f.Name).Chunks,
			Extents: saved[f.Name], TrustShared: trust}
	}
	check := func(out string) {
		t.Helper()
		for _, v := range incVolumes(src) {
			if !sameFile(t, v.Path, filepath.Join(out, v.Name)) {
				t.Fatalf("%s differs from what the guest left", v.Name)
			}
		}
	}

	out := t.TempDir()
	st, err := MaterializeFrom(ctx, c, m2, 2, nil, pathIn(out), baseFor)
	if err != nil {
		t.Fatal(err)
	}
	check(out)
	if st.BasedFiles != 2 || st.FetchedObjects != 1 || st.FetchedBytes != MiB || st.RepairedBytes != 0 ||
		st.ReusedBytes != m2.DataBytes()-MiB || st.TrustedBytes != 0 {
		t.Fatalf("stats %+v: want only the rewritten window fetched", st)
	}

	// Trusting FIEMAP: a base that has not moved since it was saved is
	// taken without a readback, and the result is the same.
	trust = true
	out = t.TempDir()
	st, err = MaterializeFrom(ctx, c, m2, 2, nil, pathIn(out), baseFor)
	if err != nil {
		t.Fatal(err)
	}
	check(out)
	if st.FetchedObjects != 1 || st.TrustedBytes != st.ReusedBytes || st.ReusedBytes != m2.DataBytes()-MiB {
		t.Fatalf("stats %+v: want every reused window trusted", st)
	}
	if st.Timing.Workers != 2 || st.Timing.Throttled429 != 0 || st.Timing.Retries != 0 {
		t.Fatalf("timing %+v: want 2 workers and no throttling", st.Timing)
	}

	// A damaged base costs a download, never a wrong file: a reused window
	// that no longer matches is fetched, and data the base holds where the
	// checkpoint names nothing is cleared.
	patchFile(t, filepath.Join(base, "tpl-snap-x-memory"), map[int64][]byte{MiB: randBytes(4096)})
	patchFile(t, filepath.Join(base, "tpl-snap-x-rootfs"), map[int64][]byte{40 * MiB: randBytes(4096)})
	// The base shares its blocks with the clones above, so the writes moved
	// them: FIEMAP trust is off and the readback catches the damage.
	for _, trust = range []bool{false, true} {
		out = t.TempDir()
		st, err = MaterializeFrom(ctx, c, m2, 2, nil, pathIn(out), baseFor)
		if err != nil {
			t.Fatal(err)
		}
		check(out)
		if st.RepairedBytes != MiB || st.FetchedObjects != 2 || st.TrustedBytes != 0 {
			t.Fatalf("trust=%v stats %+v: want the damaged window repaired from the store", trust, st)
		}
	}
}
