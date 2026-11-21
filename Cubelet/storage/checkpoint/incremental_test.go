// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const MiB = int64(1 << 20)

func writeWindows(t *testing.T, path string, size int64, windows map[int64][]byte) {
	t.Helper()
	writeFile(t, path, size, windows)
}

// patchFile changes bytes of an existing file in place.
func patchFile(t *testing.T, path string, writes map[int64][]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for off, data := range writes {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
}

func incVolumes(dir string) []IncrementalVolume {
	return []IncrementalVolume{
		{Kind: KindRootfs, Name: "tpl-snap-x-rootfs", Path: filepath.Join(dir, "rootfs"), ChunkSize: 16 * MiB},
		{Kind: KindMemory, Name: "tpl-snap-x-memory", Path: filepath.Join(dir, "memory"), ChunkSize: MiB},
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

func TestIncrementalExportUploadsOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestClient(t)
	dir := t.TempDir()
	mem := map[int64][]byte{}
	for i := int64(0); i < 6; i++ {
		mem[i*MiB] = randBytes(int(MiB))
	}
	writeWindows(t, filepath.Join(dir, "memory"), 8*MiB, mem)
	writeWindows(t, filepath.Join(dir, "rootfs"), 32*MiB, map[int64][]byte{0: randBytes(4096), 17 * MiB: randBytes(4096)})
	opt := IncrementalOptions{SandboxID: "sb-1", Generation: 1}

	_, m1, st1, err := ExportIncremental(ctx, c, "snap-a", "s1", incVolumes(dir), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if st1.UploadedObjects != 8 {
		t.Fatalf("first export uploaded %d objects, want 6 memory + 2 rootfs windows", st1.UploadedObjects)
	}
	if tm := st1.Timing; tm.Workers <= 0 || tm.Throttled429 != 0 || tm.Retries != 0 {
		t.Fatalf("timing %+v: want the worker count and no throttling", tm)
	}

	// The guest changes one memory page: one 1 MiB window is new.
	patchFile(t, filepath.Join(dir, "memory"), map[int64][]byte{2*MiB + 4096: randBytes(4096)})
	opt.Known, opt.Generation = m1.ReusableChunks(c.Prefix()), 2
	cand2, m2, st2, err := ExportIncremental(ctx, c, "snap-b", "s1", incVolumes(dir), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if st2.UploadedObjects != 1 || st2.UploadedBytes != MiB {
		t.Fatalf("second export uploaded %d objects / %d bytes, want one 1 MiB window", st2.UploadedObjects, st2.UploadedBytes)
	}
	if st2.DataBytes != m1.DataBytes() || m2.DataBytes() != m1.DataBytes() {
		t.Fatalf("a checkpoint must still describe every byte: %d vs %d", m2.DataBytes(), m1.DataBytes())
	}

	// The second checkpoint restores on its own, from chunks both sessions
	// uploaded.
	_, loaded, err := Load(ctx, c, "snap-b", cand2.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Materialize(ctx, c, loaded, 2, nil, func(f *File) (string, error) {
		return filepath.Join(out, f.Kind), nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rootfs", "memory"} {
		if !sameFile(t, filepath.Join(dir, k), filepath.Join(out, k)) {
			t.Fatalf("%s differs after restore", k)
		}
	}
}

func TestTemplateBaseIsSharedBySandboxes(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	dir := t.TempDir()
	tpl := filepath.Join(dir, "template-memory")
	windows := map[int64][]byte{}
	for i := int64(0); i < 5; i++ {
		windows[i*MiB] = randBytes(int(MiB))
	}
	writeWindows(t, tpl, 8*MiB, windows)
	base, err := ExportTemplateBase(ctx, c, "tpl-a", tpl, MiB, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Chunks) != 5 {
		t.Fatalf("template base has %d chunks", len(base.Chunks))
	}
	// Another node with the same template memory commits the same base.
	if _, err := ExportTemplateBase(ctx, c, "tpl-a", tpl, MiB, 2); err != nil {
		t.Fatalf("an identical template base from another node: %v", err)
	}

	// A sandbox started from the template touched one window.
	raw, _ := os.ReadFile(tpl)
	_ = os.WriteFile(filepath.Join(dir, "memory"), raw, 0o600)
	patchFile(t, filepath.Join(dir, "memory"), map[int64][]byte{3 * MiB: randBytes(4096)})
	writeWindows(t, filepath.Join(dir, "rootfs"), 32*MiB, nil)
	known, err := KnownChunks(ctx, c, TemplatePrefix(c.Prefix(), "tpl-a")+"c/")
	if err != nil {
		t.Fatal(err)
	}
	puts := fake.puts
	_, m, st, err := ExportIncremental(ctx, c, "snap-t", "s1", incVolumes(dir), nil,
		IncrementalOptions{SandboxID: "sb-2", TemplateID: "tpl-a", Known: known})
	if err != nil {
		t.Fatal(err)
	}
	if st.UploadedObjects != 1 {
		t.Fatalf("first pause uploaded %d objects, want only the window it changed", st.UploadedObjects)
	}
	inTemplate := 0
	for _, ch := range m.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks {
		if strings.HasPrefix(ch.Key, TemplatePrefix(c.Prefix(), "tpl-a")) {
			inTemplate++
		}
	}
	if inTemplate != 4 {
		t.Fatalf("%d memory chunks reference the template base, want 4", inTemplate)
	}
	if fake.puts-puts != 3 { // one chunk, the manifest, the candidate
		t.Fatalf("%d PUTs", fake.puts-puts)
	}

	// A template base with different bytes is never replaced.
	patchFile(t, tpl, map[int64][]byte{0: randBytes(int(MiB))})
	if _, err := ExportTemplateBase(ctx, c, "tpl-a", tpl, MiB, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("a different template base: want ErrConflict, got %v", err)
	}
}

func TestIncrementalExportRepairsChunksTheStoreLost(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	dir := t.TempDir()
	writeWindows(t, filepath.Join(dir, "memory"), 4*MiB, map[int64][]byte{0: randBytes(int(MiB)), MiB: randBytes(int(MiB))})
	writeWindows(t, filepath.Join(dir, "rootfs"), 16*MiB, nil)
	opt := IncrementalOptions{SandboxID: "sb-3", Generation: 1}
	_, m1, _, err := ExportIncremental(ctx, c, "snap-r1", "s1", incVolumes(dir), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	// The store lost a chunk the next export would reference.
	lostChunk := m1.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks[0]
	fake.mu.Lock()
	delete(fake.objects, lostChunk.Key)
	fake.mu.Unlock()
	opt.Known, opt.Generation = m1.ReusableChunks(c.Prefix()), 2
	_, m2, st, err := ExportIncremental(ctx, c, "snap-r2", "s1", incVolumes(dir), nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if st.RepairedObjects != 1 {
		t.Fatalf("repaired %d objects", st.RepairedObjects)
	}
	if err := VerifyStored(ctx, c, m2); err != nil {
		t.Fatalf("committed short: %v", err)
	}
	// The same bytes go into the new generation and the manifest names them
	// there; the old key, which a sweep of generation 1 may be deleting, is
	// never written again.
	repaired := m2.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks[0]
	if want := LineageChunkKey(c.Prefix(), "sb-3", 2, lostChunk.SHA256); repaired.Key != want || repaired.SHA256 != lostChunk.SHA256 {
		t.Fatalf("repaired chunk is named %s, want %s", repaired.Key, want)
	}
	fake.mu.Lock()
	_, rewritten := fake.objects[lostChunk.Key]
	fake.mu.Unlock()
	if rewritten {
		t.Fatalf("the lost key %s of an older generation was written again", lostChunk.Key)
	}
	if kept := m2.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks[1]; kept.Key != m1.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks[1].Key {
		t.Fatalf("a chunk the store still holds was not reused: %s", kept.Key)
	}
}

func TestVerifyStoredFindsAMissingChunk(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	dir := t.TempDir()
	writeWindows(t, filepath.Join(dir, "memory"), 2*MiB, map[int64][]byte{0: randBytes(4096)})
	writeWindows(t, filepath.Join(dir, "rootfs"), 16*MiB, nil)
	_, m, _, err := ExportIncremental(ctx, c, "snap-v", "s1", incVolumes(dir), nil, IncrementalOptions{SandboxID: "sb-4"})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyStored(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	delete(fake.objects, m.Files[1].Chunks[0].Key)
	fake.mu.Unlock()
	if err := VerifyStored(ctx, c, m); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestManifestV2RefusesAnotherSandboxesChunks(t *testing.T) {
	m := &Manifest{SchemaVersion: SchemaVersionIncremental, SnapshotID: "snap-q", Session: "s1", ChunkSize: DefaultChunkSize,
		SandboxID: "sb-1", Generation: 5, Files: []File{{Role: RoleVolume, Name: "v", Size: MiB, Kind: KindMemory, ChunkSize: MiB,
			Chunks: []Chunk{{Offset: 0, Length: MiB, SHA256: strings.Repeat("a", 64), Key: LineageChunkKey("ckpt", "sb-2", 5, strings.Repeat("a", 64))}}}}}
	if err := m.Validate("ckpt", "snap-q"); err == nil {
		t.Fatal("a chunk of another sandbox's lineage was accepted")
	}
	m.Files[0].Chunks[0].Key = TemplateChunkKey("ckpt", "tpl-x", strings.Repeat("a", 64))
	if err := m.Validate("ckpt", "snap-q"); err == nil {
		t.Fatal("a template chunk without a template id was accepted")
	}
	m.TemplateID = "tpl-x"
	if err := m.Validate("ckpt", "snap-q"); err != nil {
		t.Fatalf("the template's own chunk was refused: %v", err)
	}
}
