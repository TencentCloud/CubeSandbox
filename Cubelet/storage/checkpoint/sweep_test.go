// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func clientFor(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(Config{Endpoint: url, Bucket: "bkt", AccessKey: "ak", SecretKey: "sk", PathStyle: true, Prefix: "ckpt"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeS3) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

// sweepManifest is a checkpoint of sandbox sb at generation gen that names
// keys, one 1 MiB memory window each.
func sweepManifest(sb string, gen int64, keys ...string) *Manifest {
	f := File{Role: RoleVolume, Name: "mem", Kind: KindMemory, ChunkSize: MiB, Size: int64(len(keys)+1) * MiB, Chunks: []Chunk{}}
	for i, k := range keys {
		f.Chunks = append(f.Chunks, Chunk{Offset: int64(i) * MiB, Length: MiB, SHA256: k[strings.LastIndex(k, "/")+1:], Key: k})
	}
	return &Manifest{SchemaVersion: SchemaVersionIncremental, SnapshotID: "snap-k", Session: "s1", ChunkSize: DefaultChunkSize,
		SandboxID: sb, Generation: gen, Files: []File{f}}
}

func sum64(c byte) string { return strings.Repeat(string(c), 64) }

func TestSweepDeletesOnlyWhatItsCheckpointSupersededUpToItsGeneration(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	lin := LineagePrefix(c.Prefix(), "sb-s")
	keep1 := LineageChunkKey(c.Prefix(), "sb-s", 1, sum64('a'))
	keep2 := LineageChunkKey(c.Prefix(), "sb-s", 2, sum64('b'))
	tpl := TemplateChunkKey(c.Prefix(), "tpl-s", sum64('c'))
	doomed := []string{
		lin + "c/" + sum64('d'), // from before generations
		LineageChunkKey(c.Prefix(), "sb-s", 1, sum64('e')),
		LineageChunkKey(c.Prefix(), "sb-s", 2, sum64('f')),
	}
	spared := []string{
		keep1, keep2, tpl,
		LineageChunkKey(c.Prefix(), "sb-s", 3, sum64('a')), // a newer export's upload
		lin + "notes.json", // not a chunk
		LineageChunkKey(c.Prefix(), "sb-t", 1, sum64('e')), // another sandbox
	}
	for _, k := range append(append([]string{}, doomed...), spared...) {
		fake.put(k, []byte("x"))
	}
	st, err := SweepLineage(ctx, c, sweepManifest("sb-s", 2, keep1, keep2, tpl), SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range doomed {
		if fake.has(k) {
			t.Errorf("superseded %s survived", k)
		}
	}
	for _, k := range spared {
		if !fake.has(k) {
			t.Errorf("%s was deleted", k)
		}
	}
	if st.Deleted != 3 || st.Candidates != 3 || st.Failed != 0 || !st.Batched || st.Cancelled || st.Listed != 7 {
		t.Fatalf("stats %+v", st)
	}
	if fake.batchDeletes != 1 || fake.singleDeletes != 0 || st.Requests != 2 {
		t.Fatalf("%d batch and %d single deletes, %d requests; want one batch after one listing", fake.batchDeletes, fake.singleDeletes, st.Requests)
	}
	if _, err := SweepLineage(ctx, c, sweepManifest("sb-s", 0, keep1), SweepOptions{}); err == nil {
		t.Fatal("a checkpoint from before generations cannot bound a sweep")
	}
}

func TestSweepBatchesAtMostBatchSizeKeys(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	for i := 0; i < 25; i++ {
		fake.put(LineageChunkKey(c.Prefix(), "sb-b", 1, digest([]byte{byte(i)})), []byte("x"))
	}
	st, err := SweepLineage(ctx, c, sweepManifest("sb-b", 1), SweepOptions{BatchSize: 10, Workers: 2})
	if err != nil || st.Deleted != 25 || fake.batchDeletes != 3 {
		t.Fatalf("deleted %d in %d batches (%v), want 25 in 3", st.Deleted, fake.batchDeletes, err)
	}
}

func TestSweepFallsBackToSingleDeletes(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.batchDelete = "reject"
	for i := 0; i < 6; i++ {
		fake.put(LineageChunkKey(c.Prefix(), "sb-u", 1, digest([]byte{byte(i)})), []byte("x"))
	}
	st, err := SweepLineage(ctx, c, sweepManifest("sb-u", 1), SweepOptions{})
	if err != nil || st.Deleted != 6 || st.Batched {
		t.Fatalf("stats %+v (%v): want six single deletes", st, err)
	}
	if c.BatchDeleteSupported() || fake.singleDeletes != 6 {
		t.Fatalf("batch supported=%v, %d single deletes", c.BatchDeleteSupported(), fake.singleDeletes)
	}
	// The store is not asked for DeleteObjects again.
	fake.put(LineageChunkKey(c.Prefix(), "sb-u", 1, sum64('9')), []byte("x"))
	if st, err := SweepLineage(ctx, c, sweepManifest("sb-u", 1), SweepOptions{}); err != nil || st.Deleted != 1 || st.Requests != 2 {
		t.Fatalf("second sweep %+v (%v)", st, err)
	}
}

func TestSweepReportsKeysTheStoreDidNotDelete(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	refused := LineageChunkKey(c.Prefix(), "sb-e", 1, sum64('1'))
	gone := LineageChunkKey(c.Prefix(), "sb-e", 1, sum64('2'))
	fine := LineageChunkKey(c.Prefix(), "sb-e", 1, sum64('3'))
	fake.deleteErrors = map[string]string{refused: "AccessDenied", gone: "NoSuchKey"}
	for _, k := range []string{refused, gone, fine} {
		fake.put(k, []byte("x"))
	}
	st, err := SweepLineage(ctx, c, sweepManifest("sb-e", 1), SweepOptions{})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want the refused key reported, got %v", err)
	}
	if st.Failed != 1 || st.Deleted != 2 || fake.has(fine) {
		t.Fatalf("stats %+v: a missing key counts as deleted, a refused one as failed", st)
	}

	// One by one, a refused delete is counted the same way.
	c2, fake2 := newTestClient(t)
	fake2.batchDelete = "reject"
	refused2 := LineageChunkKey(c2.Prefix(), "sb-e", 1, sum64('1'))
	fake2.deleteErrors = map[string]string{refused2: "AccessDenied"}
	fake2.put(refused2, []byte("x"))
	fake2.put(LineageChunkKey(c2.Prefix(), "sb-e", 1, sum64('4')), []byte("x"))
	st, err = SweepLineage(ctx, c2, sweepManifest("sb-e", 1), SweepOptions{})
	if err == nil || st.Failed != 1 || st.Deleted != 1 {
		t.Fatalf("single deletes: stats %+v err %v", st, err)
	}
}

func TestSweepRetriesThrottledBatchesAndCountsThem(t *testing.T) {
	fastThrottleBackoff(t)
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.throttlePosts = 3
	k := LineageChunkKey(c.Prefix(), "sb-r", 1, sum64('1'))
	fake.put(k, []byte("x"))
	before := Throttles()
	st, err := SweepLineage(ctx, c, sweepManifest("sb-r", 1), SweepOptions{})
	if err != nil || st.Deleted != 1 || fake.has(k) {
		t.Fatalf("stats %+v (%v)", st, err)
	}
	if st.Throttled429 != 3 || st.ThrottleBackoffMS < 0 || st.Requests != 5 {
		t.Fatalf("stats %+v: want three 429s over five requests", st)
	}
	if got := Throttles().Sub(before).Status429; got != 3 {
		t.Fatalf("the process saw %d 429s", got)
	}
}

func TestSweepIsPacedByItsRequestRate(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.batchDelete = "reject"
	c.noBatchDelete.Store(true)
	for i := 0; i < 10; i++ {
		fake.put(LineageChunkKey(c.Prefix(), "sb-p", 1, digest([]byte{byte(i)})), []byte("x"))
	}
	start := time.Now()
	st, err := SweepLineage(ctx, c, sweepManifest("sb-p", 1), SweepOptions{RequestsPerSecond: 20, Workers: 4})
	if err != nil || st.Deleted != 10 || st.Requests != 11 {
		t.Fatalf("stats %+v (%v)", st, err)
	}
	// Eleven requests at 20/s: the first goes at once, the rest 50 ms apart.
	if d := time.Since(start); d < 450*time.Millisecond {
		t.Fatalf("eleven requests took %s, faster than 20/s", d)
	}
}

// interleave holds the first delete the store receives until release.
type interleave struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func holdFirstDelete(fake *fakeS3) *interleave {
	h := &interleave{reached: make(chan struct{}), release: make(chan struct{})}
	fake.mu.Lock()
	fake.beforeDelete = func() {
		first := false
		h.once.Do(func() { first = true })
		if first {
			close(h.reached)
			<-h.release
		}
	}
	fake.mu.Unlock()
	return h
}

// supersededChain exports, from one node, checkpoint N-1 that names x in
// memory window 0 and checkpoint N that replaced it with y. It returns N,
// the key N-1 held x at, and the volumes, whose window 0 holds y.
func supersededChain(t *testing.T, c *Client, dir, sb string, x []byte) (*Manifest, string, []IncrementalVolume) {
	t.Helper()
	ctx := context.Background()
	writeWindows(t, filepath.Join(dir, "memory"), 4*MiB, map[int64][]byte{0: x, MiB: randBytes(int(MiB))})
	writeWindows(t, filepath.Join(dir, "rootfs"), 16*MiB, nil)
	vols := incVolumes(dir)
	_, prev, _, err := ExportIncremental(ctx, c, "snap-n1", "s1", vols, nil, IncrementalOptions{SandboxID: sb, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	patchFile(t, filepath.Join(dir, "memory"), map[int64][]byte{0: randBytes(int(MiB))})
	_, n, _, err := ExportIncremental(ctx, c, "snap-n2", "s1", vols, nil,
		IncrementalOptions{SandboxID: sb, Generation: 2, Known: prev.ReusableChunks(c.Prefix())})
	if err != nil {
		t.Fatal(err)
	}
	xKey := prev.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks[0].Key
	if xKey != LineageChunkKey(c.Prefix(), sb, 1, digest(x)) {
		t.Fatalf("x is at %s", xKey)
	}
	return n, xKey, vols
}

func TestCrossNodeSweepNeverDeletesWhatTheNextCheckpointNames(t *testing.T) {
	ctx := context.Background()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	nodeA, nodeB := clientFor(t, srv.URL), clientFor(t, srv.URL)
	dir := t.TempDir()
	x := randBytes(int(MiB))
	n, xKey, vols := supersededChain(t, nodeA, dir, "sb-x", x)

	// Node A sweeps after N. It has listed the lineage and is about to
	// delete x, which N no longer names, when ...
	hold := holdFirstDelete(fake)
	type result struct {
		st  *SweepStats
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, err := SweepLineage(ctx, nodeA, n, SweepOptions{})
		done <- result{st, err}
	}()
	<-hold.reached
	// ... node B, where the sandbox resumed from N, pauses it again with x
	// back in window 0, building N+1 from N.
	patchFile(t, filepath.Join(dir, "memory"), map[int64][]byte{0: x})
	_, next, _, err := ExportIncremental(ctx, nodeB, "snap-n3", "s1", vols, nil,
		IncrementalOptions{SandboxID: "sb-x", Generation: n.Generation + 1, Known: n.ReusableChunks(nodeB.Prefix())})
	if err != nil {
		t.Fatal(err)
	}
	close(hold.release)
	res := <-done
	if res.err != nil || res.st.Deleted == 0 {
		t.Fatalf("sweep: %+v %v", res.st, res.err)
	}
	if fake.has(xKey) {
		t.Fatalf("the sweep left %s, which N superseded", xKey)
	}
	if err := VerifyStored(ctx, nodeB, next); err != nil {
		t.Fatalf("N+1 lost a chunk to the sweep after N: %v", err)
	}
	if got := next.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks[0].Key; got != LineageChunkKey(nodeB.Prefix(), "sb-x", 3, digest(x)) {
		t.Fatalf("N+1 names x at %s, not in its own generation", got)
	}
}

func TestCancelledSweepNeverDeletesWhatANewerExportNames(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	dir := t.TempDir()
	x := randBytes(int(MiB))
	n, xKey, vols := supersededChain(t, c, dir, "sb-c", x)

	hold := holdFirstDelete(fake)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan *SweepStats, 1)
	go func() {
		st, _ := SweepLineage(sctx, c, n, SweepOptions{})
		done <- st
	}()
	<-hold.reached
	// The next export of the same sandbox starts while the sweep's batch,
	// with x in it, is at the store; it cancels the sweep ...
	patchFile(t, filepath.Join(dir, "memory"), map[int64][]byte{0: x})
	cancel()
	st := <-done
	if !st.Cancelled {
		t.Fatalf("stats %+v: want a cancelled sweep", st)
	}
	_, next, _, err := ExportIncremental(ctx, c, "snap-n3", "s1", vols, nil,
		IncrementalOptions{SandboxID: "sb-c", Generation: n.Generation + 1, Known: n.ReusableChunks(c.Prefix())})
	if err != nil {
		t.Fatal(err)
	}
	// ... but the store still carries out the batch it already received.
	close(hold.release)
	deadline := time.Now().Add(5 * time.Second)
	for fake.has(xKey) {
		if time.Now().After(deadline) {
			t.Fatal("the in-flight batch never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := VerifyStored(ctx, c, next); err != nil {
		t.Fatalf("the cancelled sweep deleted a chunk N+1 names: %v", err)
	}
}

func TestExportReusesOnlyOlderGenerationsAndItsTemplate(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	dir := t.TempDir()
	w := [][]byte{randBytes(int(MiB)), randBytes(int(MiB)), randBytes(int(MiB)), randBytes(int(MiB)), randBytes(int(MiB))}
	writeWindows(t, filepath.Join(dir, "memory"), 8*MiB, map[int64][]byte{0: w[0], MiB: w[1], 2 * MiB: w[2], 3 * MiB: w[3], 4 * MiB: w[4]})
	writeWindows(t, filepath.Join(dir, "rootfs"), 16*MiB, nil)
	p := c.Prefix()
	known := map[string]string{
		digest(w[0]): LineagePrefix(p, "sb-g") + "c/" + digest(w[0]), // before generations
		digest(w[1]): LineageChunkKey(p, "sb-g", 7, digest(w[1])),    // its own generation
		digest(w[2]): LineageChunkKey(p, "sb-g", 9, digest(w[2])),    // a newer one
		digest(w[3]): TemplateChunkKey(p, "tpl-other", digest(w[3])), // another template
		digest(w[4]): LineageChunkKey(p, "sb-g", 6, digest(w[4])),    // an older one: reused
	}
	for _, k := range known {
		fake.put(k, make([]byte, MiB))
	}
	_, m, st, err := ExportIncremental(ctx, c, "snap-g", "s1", incVolumes(dir), nil,
		IncrementalOptions{SandboxID: "sb-g", Generation: 7, Known: known})
	if err != nil {
		t.Fatal(err)
	}
	if st.UploadedObjects != 4 {
		t.Fatalf("uploaded %d objects, want four: only the older generation's chunk is reused", st.UploadedObjects)
	}
	for i, ch := range m.FindFile(RoleVolume, "tpl-snap-x-memory").Chunks {
		want := LineageChunkKey(p, "sb-g", 7, ch.SHA256)
		if i == 4 {
			want = known[digest(w[4])]
		}
		if ch.Key != want {
			t.Errorf("window %d is named %s, want %s", i, ch.Key, want)
		}
	}
	if m.Generation != 7 {
		t.Fatalf("manifest generation %d", m.Generation)
	}
	reuse := m.ReusableChunks(p)
	if len(reuse) != 5 {
		t.Fatalf("a successor may reuse %d chunks, want all five", len(reuse))
	}
}

func TestManifestGenerationsBoundTheLineageKeys(t *testing.T) {
	sum := sum64('a')
	m := sweepManifest("sb-v", 5, LineageChunkKey("ckpt", "sb-v", 5, sum))
	m.SnapshotID = "snap-v"
	if err := m.Validate("ckpt", "snap-v"); err != nil {
		t.Fatalf("its own generation: %v", err)
	}
	m.Files[0].Chunks[0].Key = LineageChunkKey("ckpt", "sb-v", 4, sum)
	if err := m.Validate("ckpt", "snap-v"); err != nil {
		t.Fatalf("an older generation: %v", err)
	}
	for name, key := range map[string]string{
		"a newer generation": LineageChunkKey("ckpt", "sb-v", 6, sum),
		"a key from before":  LineagePrefix("ckpt", "sb-v") + "c/" + sum,
		"generation zero":    LineagePrefix("ckpt", "sb-v") + "g/0000000000000000000/c/" + sum,
		"a short generation": LineagePrefix("ckpt", "sb-v") + "g/5/c/" + sum,
		"another digest":     LineageChunkKey("ckpt", "sb-v", 5, sum64('b')),
		"another sandbox's":  LineageChunkKey("ckpt", "sb-w", 5, sum),
		"a nested key":       LineagePrefix("ckpt", "sb-v") + "g/0000000000000000005/c/x/" + sum,
	} {
		m.Files[0].Chunks[0].Key = key
		if err := m.Validate("ckpt", "snap-v"); err == nil {
			t.Errorf("%s was accepted: %s", name, key)
		}
	}
	// A manifest from before generations names only chunks from before.
	m.Generation = 0
	m.Files[0].Chunks[0].Key = LineagePrefix("ckpt", "sb-v") + "c/" + sum
	if err := m.Validate("ckpt", "snap-v"); err != nil {
		t.Fatalf("an old manifest: %v", err)
	}
	if len(m.ReusableChunks("ckpt")) != 0 {
		t.Fatal("chunks of an old manifest must not be reused")
	}
	m.Files[0].Chunks[0].Key = LineageChunkKey("ckpt", "sb-v", 1, sum)
	if err := m.Validate("ckpt", "snap-v"); err == nil {
		t.Fatal("an old manifest named a generation")
	}
}

func TestDeletePrefixBatchesLikeASweep(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	for i := 0; i < 25; i++ {
		fake.put(ChunkKey(c.Prefix(), "snap-r", "s1", digest([]byte{byte(i)})), []byte("x"))
	}
	fake.put(CandidateKey(c.Prefix(), "snap-keep"), []byte("x"))
	n, err := c.DeletePrefix(ctx, SnapshotPrefix(c.Prefix(), "snap-r"), SweepOptions{BatchSize: 10, Workers: 2})
	if err != nil || n != 25 || fake.batchDeletes != 3 || fake.singleDeletes != 0 {
		t.Fatalf("deleted %d in %d batches and %d single deletes (%v), want 25 in 3 batches", n, fake.batchDeletes, fake.singleDeletes, err)
	}
	if !fake.has(CandidateKey(c.Prefix(), "snap-keep")) || len(fake.objects) != 1 {
		t.Fatalf("objects left: %v", fake.objects)
	}
}

func TestDeletePrefixFallsBackToSingleDeletesAndIsPaced(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.batchDelete = "reject"
	for i := 0; i < 10; i++ {
		fake.put(ChunkKey(c.Prefix(), "snap-p", "s1", digest([]byte{byte(i)})), []byte("x"))
	}
	start := time.Now()
	n, err := c.DeletePrefix(ctx, SnapshotPrefix(c.Prefix(), "snap-p"), SweepOptions{RequestsPerSecond: 20, Workers: 4})
	if err != nil || n != 10 || fake.singleDeletes != 10 || c.BatchDeleteSupported() {
		t.Fatalf("deleted %d with %d single deletes (%v), batch supported=%v", n, fake.singleDeletes, err, c.BatchDeleteSupported())
	}
	// A listing, a refused DeleteObjects and ten deletes at 20/s: the first
	// goes at once, the rest 50 ms apart.
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Fatalf("twelve requests took %s, faster than 20/s", d)
	}
}

func TestDeletePrefixReportsKeysTheStoreDidNotDelete(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	refused := ChunkKey(c.Prefix(), "snap-f", "s1", sum64('a'))
	fake.put(refused, []byte("x"))
	fake.put(ChunkKey(c.Prefix(), "snap-f", "s1", sum64('b')), []byte("x"))
	fake.deleteErrors = map[string]string{refused: "AccessDenied"}
	n, err := c.DeletePrefix(ctx, SnapshotPrefix(c.Prefix(), "snap-f"), SweepOptions{})
	if err == nil || n != 1 || !fake.has(refused) {
		t.Fatalf("deleted %d (%v); want one deleted and the refused key reported", n, err)
	}
}
