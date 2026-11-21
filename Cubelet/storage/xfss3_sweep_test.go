// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

// sweepTestStore is a path-style bucket "b" with what an export needs.
type sweepTestStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	firstPut time.Time
}

func (s *sweepTestStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/b"), "/")
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodPut:
		if _, ok := s.objects[key]; ok && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if s.firstPut.IsZero() {
			s.firstPut = time.Now()
		}
		s.objects[key] = body
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		type content struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		res := struct {
			XMLName  xml.Name  `xml:"ListBucketResult"`
			Contents []content `xml:"Contents"`
		}{}
		var keys []string
		for k := range s.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, content{Key: k, Size: len(s.objects[k])})
		}
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		data, ok := s.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func (s *sweepTestStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

// sweepTestEnv points the xfs-s3 hooks at a fake store and a temporary
// baseline root, and stubs the lineage sweep with sweep.
func sweepTestEnv(t *testing.T, sweep func(ctx context.Context, c *checkpoint.Client, keep *checkpoint.Manifest, opt checkpoint.SweepOptions) (*checkpoint.SweepStats, error)) (*checkpoint.Client, *sweepTestStore) {
	t.Helper()
	store := &sweepTestStore{objects: map[string][]byte{}}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	client, err := checkpoint.NewClient(checkpoint.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
		PathStyle: true, Prefix: "ckpt", Incremental: true, MemoryChunkMiB: 1, RootfsChunkMiB: 16})
	if err != nil {
		t.Fatal(err)
	}
	baseRoot := t.TempDir()
	prevRoot, prevSweep, prevWait := xfss3BaseRoot, xfss3SweepLineage, xfss3SweepStopWait
	t.Cleanup(func() { xfss3BaseRoot, xfss3SweepLineage, xfss3SweepStopWait = prevRoot, prevSweep, prevWait })
	xfss3BaseRoot = func() string { return baseRoot }
	xfss3SweepLineage = sweep
	xfss3SweepStopWait = 10 * time.Second
	return client, store
}

func noSweep(context.Context, *checkpoint.Client, *checkpoint.Manifest, checkpoint.SweepOptions) (*checkpoint.SweepStats, error) {
	return &checkpoint.SweepStats{}, nil
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

const testMiB = 1 << 20

// sweepTestPackage is a package home with a 4 MiB memory file (two 1 MiB
// windows of data) and an empty 16 MiB rootfs.
func sweepTestPackage(t *testing.T, snapshotID string, memory []byte) (*SnapshotCatalogEntry, []checkpoint.SourceFile) {
	t.Helper()
	home := t.TempDir()
	entry := &SnapshotCatalogEntry{SnapshotID: snapshotID, SnapshotPath: home,
		RootfsVol: "tpl-" + snapshotID + "-rootfs", MemoryVol: "tpl-" + snapshotID + "-memory"}
	mem := filepath.Join(home, "memory")
	if err := os.WriteFile(mem, memory, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(mem, 4*testMiB); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(home, "rootfs")
	if err := os.WriteFile(rootfs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(rootfs, 16*testMiB); err != nil {
		t.Fatal(err)
	}
	return entry, []checkpoint.SourceFile{
		{Role: checkpoint.RoleVolume, Name: entry.RootfsVol, Path: rootfs},
		{Role: checkpoint.RoleVolume, Name: entry.MemoryVol, Path: mem},
	}
}

// writeTestBaseline makes m, exported from files, the sandbox's baseline as
// snapshotID, with plain copies standing in for the reflinks.
func writeTestBaseline(t *testing.T, sandboxID, snapshotID string, m *checkpoint.Manifest, files []checkpoint.SourceFile) string {
	t.Helper()
	dir := filepath.Join(xfss3BaseRoot(), sandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		kind := checkpoint.KindRootfs
		if strings.HasSuffix(f.Name, "-memory") {
			kind = checkpoint.KindMemory
		}
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, kind), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(m)
	sum := sha256.Sum256(raw)
	rec, _ := json.Marshal(&xfss3BaseRecord{SnapshotID: snapshotID, ManifestSHA256: hex.EncodeToString(sum[:])})
	_ = os.WriteFile(filepath.Join(dir, xfss3BaseManifestFile), raw, 0o600)
	_ = os.WriteFile(filepath.Join(dir, xfss3BaseRecordFile), rec, 0o600)
	return hex.EncodeToString(sum[:])
}

// exportFirst commits snap-1 of sandboxID at generation 1 straight through
// the checkpoint package, as the export before the one under test.
func exportFirst(t *testing.T, client *checkpoint.Client, sandboxID string, files []checkpoint.SourceFile, entry *SnapshotCatalogEntry) *checkpoint.Manifest {
	t.Helper()
	vols := []checkpoint.IncrementalVolume{
		{Kind: checkpoint.KindRootfs, Name: entry.RootfsVol, Path: files[0].Path, ChunkSize: 16 * testMiB},
		{Kind: checkpoint.KindMemory, Name: entry.MemoryVol, Path: files[1].Path, ChunkSize: testMiB},
	}
	_, m, _, err := checkpoint.ExportIncremental(context.Background(), client, "snap-1", "s-1", vols, nil,
		checkpoint.IncrementalOptions{SandboxID: sandboxID, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func memoryChunks(m *checkpoint.Manifest) []checkpoint.Chunk {
	for _, f := range m.Files {
		if f.Kind == checkpoint.KindMemory {
			return f.Chunks
		}
	}
	return nil
}

func TestXFSS3ExportReusesLineageChunksOnlyThroughTheCheckpointItWasRestoredFrom(t *testing.T) {
	ctx := context.Background()
	x := randomBytes(t, 2*testMiB)
	for _, tc := range []struct {
		name     string
		restored *xfss3RestoredFrom
		reuse    bool
	}{
		{"restored from the baseline", &xfss3RestoredFrom{SnapshotID: "snap-1"}, true},
		{"a stale baseline", &xfss3RestoredFrom{SnapshotID: "snap-0"}, false},
		{"no record of the restore", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := sweepTestEnv(t, noSweep)
			entry1, files1 := sweepTestPackage(t, "snap-1", x)
			m1 := exportFirst(t, client, "sb-1", files1, entry1)
			sha1 := writeTestBaseline(t, "sb-1", "snap-1", m1, files1)
			if tc.restored != nil && tc.reuse {
				tc.restored.ManifestSHA256 = sha1
			}
			// The next package holds the same memory.
			entry2, files2 := sweepTestPackage(t, "snap-2", x)
			job := &xfss3Job{SnapshotID: "snap-2", Session: "s-2", SandboxID: "sb-1", Generation: 2, RestoredFrom: tc.restored}
			_, m2, err := exportXFSS3Incremental(ctx, client, entry2, job, files2, nil, time.Now(), &checkpoint.Progress{})
			if err != nil {
				t.Fatal(err)
			}
			job.sweep.release()
			<-job.sweep.done
			old, got := memoryChunks(m1), memoryChunks(m2)
			if len(got) != 2 || len(old) != 2 {
				t.Fatalf("memory chunks %d and %d", len(old), len(got))
			}
			for i := range got {
				reused := got[i].Key == old[i].Key
				if reused != tc.reuse {
					t.Errorf("window %d named %s (first checkpoint %s): reuse=%v, want %v", i, got[i].Key, old[i].Key, reused, tc.reuse)
				}
				if !tc.reuse && got[i].Key != checkpoint.LineageChunkKey(client.Prefix(), "sb-1", 2, got[i].SHA256) {
					t.Errorf("window %d uploaded to %s, not into generation 2", i, got[i].Key)
				}
			}
			if wantBase := map[bool]string{true: "lineage", false: "none"}[tc.reuse]; job.Base != wantBase {
				t.Errorf("base %q, want %q", job.Base, wantBase)
			}
			if err := checkpoint.VerifyStored(ctx, client, m2); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestXFSS3BaselineIdentity(t *testing.T) {
	m := &checkpoint.Manifest{Generation: 3}
	bl := &xfss3Baseline{SnapshotID: "snap-1", ManifestSHA256: "aa", Manifest: m}
	for _, tc := range []struct {
		name string
		bl   *xfss3Baseline
		rf   *xfss3RestoredFrom
		ok   bool
		why  bool
	}{
		{"first checkpoint", nil, nil, false, false},
		{"matching pause", bl, &xfss3RestoredFrom{SnapshotID: "snap-1"}, true, false},
		{"matching import", bl, &xfss3RestoredFrom{SnapshotID: "snap-1", ManifestSHA256: "aa"}, true, false},
		{"another manifest of the snapshot", bl, &xfss3RestoredFrom{SnapshotID: "snap-1", ManifestSHA256: "bb"}, false, true},
		{"stale", bl, &xfss3RestoredFrom{SnapshotID: "snap-2"}, false, true},
		{"no baseline", nil, &xfss3RestoredFrom{SnapshotID: "snap-2"}, false, true},
		{"no record", bl, nil, false, true},
		{"before generations", &xfss3Baseline{SnapshotID: "snap-1", Manifest: &checkpoint.Manifest{}}, &xfss3RestoredFrom{SnapshotID: "snap-1"}, false, true},
	} {
		ok, why := xfss3BaselineUsable(tc.bl, tc.rf)
		if ok != tc.ok || (why != "") != tc.why {
			t.Errorf("%s: usable=%v why=%q", tc.name, ok, why)
		}
	}
	now := time.Unix(0, 1000)
	if g := xfss3NextGeneration(nil, now); g != 1000 {
		t.Errorf("first generation %d", g)
	}
	// Another node's clock ran ahead: the generation still grows.
	if g := xfss3NextGeneration(&xfss3RestoredFrom{Generation: 5000}, now); g != 5001 {
		t.Errorf("generation after 5000 is %d", g)
	}
}

func TestXFSS3RestoredFromRecord(t *testing.T) {
	_, _ = sweepTestEnv(t, noSweep)
	if loadXFSS3RestoredFrom("sb-r") != nil {
		t.Fatal("a record out of nothing")
	}
	if err := saveXFSS3RestoredFrom("sb-r", &xfss3RestoredFrom{SnapshotID: "snap-9", Generation: 9, Via: "import"}); err != nil {
		t.Fatal(err)
	}
	if rf := loadXFSS3RestoredFrom("sb-r"); rf == nil || rf.SnapshotID != "snap-9" || rf.Generation != 9 || rf.RecordedAt == "" {
		t.Fatalf("record %+v", rf)
	}
	if err := saveXFSS3RestoredFrom("../x", &xfss3RestoredFrom{SnapshotID: "snap-9"}); err == nil {
		t.Fatal("a sandbox id with a path in it was accepted")
	}
}

func TestXFSS3EnsureRepairUploadsIntoTheNewGeneration(t *testing.T) {
	ctx := context.Background()
	client, store := sweepTestEnv(t, noSweep)
	x := randomBytes(t, 2*testMiB)
	entry1, files1 := sweepTestPackage(t, "snap-1", x)
	m1 := exportFirst(t, client, "sb-e", files1, entry1)
	sha1 := writeTestBaseline(t, "sb-e", "snap-1", m1, files1)
	lost := memoryChunks(m1)[0]
	store.mu.Lock()
	delete(store.objects, lost.Key)
	store.mu.Unlock()

	entry2, files2 := sweepTestPackage(t, "snap-2", x)
	job := &xfss3Job{SnapshotID: "snap-2", Session: "s-2", SandboxID: "sb-e", Generation: 2,
		RestoredFrom: &xfss3RestoredFrom{SnapshotID: "snap-1", ManifestSHA256: sha1}}
	_, m2, err := exportXFSS3Incremental(ctx, client, entry2, job, files2, nil, time.Now(), &checkpoint.Progress{})
	if err != nil {
		t.Fatal(err)
	}
	job.sweep.release()
	<-job.sweep.done
	got := memoryChunks(m2)
	if want := checkpoint.LineageChunkKey(client.Prefix(), "sb-e", 2, lost.SHA256); got[0].Key != want {
		t.Fatalf("the lost chunk is named %s, want %s in the new generation", got[0].Key, want)
	}
	if store.has(lost.Key) {
		t.Fatalf("the old key %s was written again", lost.Key)
	}
	if got[1].Key != memoryChunks(m1)[1].Key {
		t.Fatalf("a stored chunk of the base was not reused: %s", got[1].Key)
	}
	if job.Stats.RepairedObjects != 1 {
		t.Fatalf("repaired %d", job.Stats.RepairedObjects)
	}
	if err := checkpoint.VerifyStored(ctx, client, m2); err != nil {
		t.Fatal(err)
	}
}

// blockingSweep stands in for the lineage sweep: it reports when it starts
// and runs until released or cancelled.
type blockingSweep struct {
	started   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	cancelled time.Time
}

func newBlockingSweep() *blockingSweep {
	return &blockingSweep{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingSweep) sweep(ctx context.Context, _ *checkpoint.Client, _ *checkpoint.Manifest, _ checkpoint.SweepOptions) (*checkpoint.SweepStats, error) {
	b.started <- struct{}{}
	select {
	case <-b.release:
		return &checkpoint.SweepStats{Listed: 3, Candidates: 2, Deleted: 2, Requests: 2, Batched: true, DurationMS: 7}, nil
	case <-ctx.Done():
		b.mu.Lock()
		b.cancelled = time.Now()
		b.mu.Unlock()
		return &checkpoint.SweepStats{Cancelled: true}, nil
	}
}

func readJob(t *testing.T, home string) *xfss3Job {
	t.Helper()
	job, err := readXFSS3Job(home)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestXFSS3SweepDoesNotDelayTheExport(t *testing.T) {
	b := newBlockingSweep()
	client, _ := sweepTestEnv(t, b.sweep)
	entry, _ := sweepTestPackage(t, "snap-a", randomBytes(t, testMiB))
	m := &checkpoint.Manifest{SchemaVersion: checkpoint.SchemaVersionIncremental, SandboxID: "sb-a", Generation: 4}
	prevOnce := xfss3ExportOnce
	t.Cleanup(func() { xfss3ExportOnce = prevOnce })
	xfss3ExportOnce = func(ctx context.Context, entry *SnapshotCatalogEntry, job *xfss3Job, _ *checkpoint.Progress) (*checkpoint.Candidate, *checkpoint.Manifest, error) {
		job.Mode = "incremental"
		job.sweep = newXFSS3Sweep(ctx, client, entry.SnapshotPath, job.Session, entry.SnapshotID, m)
		return &checkpoint.Candidate{ManifestSHA256: strings.Repeat("a", 64)}, m, nil
	}
	job := &xfss3Job{Role: xfss3RoleOrigin, SnapshotID: "snap-a", Session: "s-a", State: xfss3StateExporting, SandboxID: "sb-a"}
	if err := writeXFSS3Job(entry.SnapshotPath, job); err != nil {
		t.Fatal(err)
	}
	// The export returns, recorded exported, while the sweep is still running.
	runXFSS3Export(context.Background(), entry, job, &checkpoint.Progress{})
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep never started")
	}
	rec := readJob(t, entry.SnapshotPath)
	if rec.State != xfss3StateExported || rec.Sweep == nil || rec.Sweep.State != xfss3SweepPending {
		t.Fatalf("job %+v sweep %+v: want exported with the sweep pending", rec, rec.Sweep)
	}
	close(b.release)
	v, ok := xfss3Sweeps.Load("sb-a")
	if ok {
		<-v.(*xfss3Sweep).done
	}
	deadline := time.Now().Add(5 * time.Second)
	for rec = readJob(t, entry.SnapshotPath); rec.Sweep.State == xfss3SweepPending; rec = readJob(t, entry.SnapshotPath) {
		if time.Now().After(deadline) {
			t.Fatal("the sweep was never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec.State != xfss3StateExported || rec.Sweep.State != xfss3SweepDone || rec.Sweep.Stats == nil || rec.Sweep.Stats.Deleted != 2 || rec.Sweep.FinishedAt == "" {
		t.Fatalf("job %+v sweep %+v", rec, rec.Sweep)
	}
}

func TestXFSS3NextExportStopsTheSandboxSweepFirst(t *testing.T) {
	ctx := context.Background()
	b := newBlockingSweep()
	client, store := sweepTestEnv(t, b.sweep)
	x := randomBytes(t, 2*testMiB)

	// The sweep after snap-1 runs on this node ...
	entry1, files1 := sweepTestPackage(t, "snap-1", x)
	m1 := exportFirst(t, client, "sb-n", files1, entry1)
	job1 := &xfss3Job{Role: xfss3RoleOrigin, SnapshotID: "snap-1", Session: "s-1", State: xfss3StateExported, SandboxID: "sb-n",
		Sweep: &xfss3SweepRecord{State: xfss3SweepPending}}
	if err := writeXFSS3Job(entry1.SnapshotPath, job1); err != nil {
		t.Fatal(err)
	}
	s := newXFSS3Sweep(ctx, client, entry1.SnapshotPath, "s-1", "snap-1", m1)
	s.release()
	<-b.started
	store.mu.Lock()
	store.firstPut = time.Time{}
	store.mu.Unlock()

	// ... when the sandbox's next export starts.
	entry2, files2 := sweepTestPackage(t, "snap-2", x)
	job2 := &xfss3Job{SnapshotID: "snap-2", Session: "s-2", SandboxID: "sb-n", Generation: 2}
	_, _, err := exportXFSS3Incremental(ctx, client, entry2, job2, files2, nil, time.Now(), &checkpoint.Progress{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("the export did not wait for the sweep it cancelled")
	}
	b.mu.Lock()
	cancelled := b.cancelled
	b.mu.Unlock()
	store.mu.Lock()
	firstPut := store.firstPut
	store.mu.Unlock()
	if cancelled.IsZero() || firstPut.IsZero() || !cancelled.Before(firstPut) {
		t.Fatalf("sweep cancelled at %v, first upload at %v: the sweep must stop first", cancelled, firstPut)
	}
	if rec := readJob(t, entry1.SnapshotPath); rec.Sweep == nil || rec.Sweep.State != xfss3SweepCancelled {
		t.Fatalf("sweep record %+v", rec.Sweep)
	}
	// The new export's own sweep is registered in its place.
	if v, ok := xfss3Sweeps.Load("sb-n"); !ok || v.(*xfss3Sweep) != job2.sweep {
		t.Fatal("the next export's sweep is not the registered one")
	}
	job2.sweep.cancel()
	job2.sweep.release()
	<-job2.sweep.done
}

func TestXFSS3RestartRecordsALostSweep(t *testing.T) {
	_, _ = sweepTestEnv(t, noSweep)
	home := t.TempDir()
	job := &xfss3Job{Role: xfss3RoleOrigin, SnapshotID: "snap-l", Session: "s-l", State: xfss3StateExported, SandboxID: "sb-l",
		Sweep: &xfss3SweepRecord{State: xfss3SweepPending}}
	if err := writeXFSS3Job(home, job); err != nil {
		t.Fatal(err)
	}
	resumeXFSS3Entry(context.Background(), &SnapshotCatalogEntry{SnapshotID: "snap-l", SnapshotPath: home})
	if rec := readJob(t, home); rec.Sweep.State != xfss3SweepLost || rec.Sweep.FinishedAt == "" || rec.State != xfss3StateExported {
		t.Fatalf("record %+v sweep %+v", rec, rec.Sweep)
	}
	if _, running := xfss3Exports.Load("snap-l"); running {
		t.Fatal("an exported checkpoint was exported again")
	}
	if err := updateXFSS3Job(home, "s-other", func(j *xfss3Job) bool { j.State = "x"; return true }); err == nil {
		t.Fatal("a record of another session was changed")
	}
}
