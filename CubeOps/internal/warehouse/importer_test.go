// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

func TestExecuteCleansWorkDirOnFailure(t *testing.T) {
	work := t.TempDir()
	blobs := NewMemBlobStore()
	im := newImporter(nil, blobs, FetchConfig{}, work, time.Minute)
	job := store.ImportJob{
		ID:        "deadbeef",
		Source:    SourceUpload,
		SourceRef: UploadObjectKey("missing"),
		Arch:      "amd64",
	}
	if _, err := im.execute(context.Background(), job); err == nil {
		t.Fatal("expected missing-archive error")
	}
	dir := filepath.Join(work, "import-deadbeef")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work dir leftover after failed import: %v", err)
	}
}

func TestExecuteClearsLeftoverWorkDir(t *testing.T) {
	work := t.TempDir()
	stale := filepath.Join(work, "import-staleid")
	if err := os.MkdirAll(filepath.Join(stale, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "junk", "leftover"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	blobs := NewMemBlobStore()
	im := newImporter(nil, blobs, FetchConfig{}, work, time.Minute)
	job := store.ImportJob{
		ID:        "staleid",
		Source:    SourceUpload,
		SourceRef: UploadObjectKey("missing"),
		Arch:      "amd64",
	}
	if _, err := im.execute(context.Background(), job); err == nil {
		t.Fatal("expected missing-archive error")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("leftover work dir after execute: %v", err)
	}
}

type fakeImportStore struct {
	mu         sync.Mutex
	job        store.ImportJob
	siblings   []store.ImportJob
	claimed    bool
	updates    []string
	inserted   int
	insertKeys []string
	items      map[string]store.WarehouseItem
	insertFn   func(store.WarehouseItem) (bool, error)
}

func warehouseItemKey(arch, component, version string) string {
	return arch + "|" + component + "|" + version
}

func (f *fakeImportStore) eachJob(fn func(*store.ImportJob)) {
	fn(&f.job)
	for i := range f.siblings {
		fn(&f.siblings[i])
	}
}

func (f *fakeImportStore) ListImportWork(context.Context, time.Duration) ([]store.ImportJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.job
	return []store.ImportJob{job}, nil
}

func (f *fakeImportStore) ClaimImportJob(context.Context, string, time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed {
		return false, nil
	}
	f.claimed = true
	f.job.Status = store.ImportRunning
	return true, nil
}

func (f *fakeImportStore) UpdateImportJob(_ context.Context, id, status, _ string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, status)
	f.eachJob(func(job *store.ImportJob) {
		if job.ID == id {
			job.Status = status
		}
	})
	return nil
}

func (f *fakeImportStore) TouchImportJob(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := false
	f.eachJob(func(job *store.ImportJob) {
		if job.ID == id && job.Status == store.ImportRunning {
			ok = true
		}
	})
	return ok, nil
}

func (f *fakeImportStore) RequeueImportJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eachJob(func(job *store.ImportJob) {
		if job.ID == id && job.Status == store.ImportRunning {
			job.Status = store.ImportPending
		}
	})
	return nil
}

func (f *fakeImportStore) GetImportJob(_ context.Context, id string) (*store.ImportJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *store.ImportJob
	f.eachJob(func(job *store.ImportJob) {
		if job.ID == id {
			cp := *job
			found = &cp
		}
	})
	return found, nil
}

func (f *fakeImportStore) InsertWarehouseItem(_ context.Context, item store.WarehouseItem) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertFn != nil {
		return f.insertFn(item)
	}
	if f.items == nil {
		f.items = map[string]store.WarehouseItem{}
	}
	key := warehouseItemKey(item.Arch, item.Component, item.Version)
	if _, ok := f.items[key]; ok {
		return false, nil
	}
	f.items[key] = item
	f.insertKeys = append(f.insertKeys, key)
	f.inserted++
	return true, nil
}

func (f *fakeImportStore) GetWarehouseItem(_ context.Context, arch, component, version string) (*store.WarehouseItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[warehouseItemKey(arch, component, version)]
	if !ok {
		return nil, nil
	}
	cp := item
	return &cp, nil
}

func (f *fakeImportStore) ListWarehouseItems(context.Context) ([]store.WarehouseItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.WarehouseItem
	for _, item := range f.items {
		out = append(out, item)
	}
	return out, nil
}

func (f *fakeImportStore) CountLiveImportJobsBySourceRef(_ context.Context, sourceRef string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	f.eachJob(func(job *store.ImportJob) {
		if job.ID == "" {
			return
		}
		if job.SourceRef == sourceRef && isLiveImportStatus(job.Status) {
			n++
		}
	})
	return n, nil
}

func putUpload(t *testing.T, blobs BlobStore, src string) string {
	t.Helper()
	key := UploadObjectKey("11111111-1111-1111-1111-111111111111")
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := blobs.Put(context.Background(), key, f, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDrainClaimsOnce(t *testing.T) {
	root := t.TempDir()
	archive := buildFakeOneClick(t, filepath.Join(root, "pkg"), "v0.6.0")
	blobs := NewMemBlobStore()
	key := putUpload(t, blobs, archive)
	fake := &fakeImportStore{job: store.ImportJob{
		ID:        "job-1",
		Source:    SourceUpload,
		SourceRef: key,
		Tag:       "v0.6.0",
		Arch:      "amd64",
		Status:    store.ImportPending,
	}}
	work := t.TempDir()
	imA := newImporter(fake, blobs, FetchConfig{}, work, time.Minute)
	imB := newImporter(fake, blobs, FetchConfig{}, work, time.Minute)

	var start, done sync.WaitGroup
	start.Add(2)
	done.Add(2)
	run := func(im *Importer) {
		defer done.Done()
		start.Done()
		start.Wait()
		im.drain(context.Background())
	}
	go run(imA)
	go run(imB)
	done.Wait()

	got := imA.executeCount.Load() + imB.executeCount.Load()
	if got != 1 {
		t.Fatalf("execute count=%d want 1", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.claimed {
		t.Fatal("job was not claimed")
	}
	if len(fake.updates) != 1 || fake.updates[0] != store.ImportSucceeded {
		t.Fatalf("updates=%v want one succeeded", fake.updates)
	}
	if fake.inserted == 0 {
		t.Fatal("winner did not insert warehouse items")
	}
}

func TestExecuteInsertsWhenObjectMissingRow(t *testing.T) {
	root := t.TempDir()
	archive := buildFakeOneClick(t, filepath.Join(root, "pkg"), "v0.6.0")
	blobs := NewMemBlobStore()
	key := putUpload(t, blobs, archive)
	fake := &fakeImportStore{}
	im := newImporter(fake, blobs, FetchConfig{}, t.TempDir(), time.Minute)
	job := store.ImportJob{
		ID: "repair-1", Source: SourceUpload, SourceRef: key,
		Tag: "v0.6.0", Arch: ArchAMD64, Status: store.ImportPending,
	}
	if _, err := im.execute(context.Background(), job); err != nil {
		t.Fatalf("execute: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.items[warehouseItemKey(ArchAMD64, ComponentShim, "v0.6.0")]; !ok {
		t.Fatal("missing warehouse row for imported shim")
	}
}

func TestExecuteSkipsWhenRowExists(t *testing.T) {
	root := t.TempDir()
	archive := buildFakeOneClick(t, filepath.Join(root, "pkg"), "v0.6.0")
	blobs := NewMemBlobStore()
	key := putUpload(t, blobs, archive)
	shimKey := warehouseItemKey(ArchAMD64, ComponentShim, "v0.6.0")
	fake := &fakeImportStore{items: map[string]store.WarehouseItem{
		shimKey: {Arch: ArchAMD64, Component: ComponentShim, Version: "v0.6.0"},
	}}
	im := newImporter(fake, blobs, FetchConfig{}, t.TempDir(), time.Minute)
	job := store.ImportJob{
		ID: "skip-1", Source: SourceUpload, SourceRef: key,
		Tag: "v0.6.0", Arch: ArchAMD64, Status: store.ImportPending,
	}
	if _, err := im.execute(context.Background(), job); err != nil {
		t.Fatalf("execute: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, k := range fake.insertKeys {
		if k == shimKey {
			t.Fatal("inserted existing shim key")
		}
	}
}

func TestInsertFalseDoesNotDeleteCanonicalObject(t *testing.T) {
	root := t.TempDir()
	archive := buildFakeOneClick(t, filepath.Join(root, "pkg"), "v0.6.0")
	blobs := NewMemBlobStore()
	key := putUpload(t, blobs, archive)
	fake := &fakeImportStore{insertFn: func(store.WarehouseItem) (bool, error) {
		return false, nil
	}}
	im := newImporter(fake, blobs, FetchConfig{}, t.TempDir(), time.Minute)
	job := store.ImportJob{
		ID: "race-1", Source: SourceUpload, SourceRef: key,
		Tag: "v0.6.0", Arch: ArchAMD64, Status: store.ImportPending,
	}
	if _, err := im.execute(context.Background(), job); err != nil {
		t.Fatalf("execute: %v", err)
	}
	objKey := ObjectKey(ArchAMD64, ComponentShim, "v0.6.0")
	if _, err := blobs.Stat(context.Background(), objKey); err != nil {
		t.Fatalf("canonical object deleted after inserted=false: %v", err)
	}
}

func TestRunJobDeletesUploadWhenNoLiveSiblings(t *testing.T) {
	root := t.TempDir()
	src := buildFakeOneClick(t, filepath.Join(root, "pkg"), "v0.6.0")
	blobs := NewMemBlobStore()
	key := putUpload(t, blobs, src)
	fake := &fakeImportStore{job: store.ImportJob{
		ID: "job-a", Source: SourceUpload, SourceRef: key,
		Tag: "v0.6.0", Arch: ArchAMD64, Status: store.ImportRunning,
	}}
	im := newImporter(fake, blobs, FetchConfig{}, t.TempDir(), time.Minute)
	im.runJob(context.Background(), fake.job)
	if _, err := blobs.Stat(context.Background(), key); !IsNotExist(err) {
		t.Fatalf("upload left after last job: %v", err)
	}
}

func TestRunJobKeepsUploadWhileSiblingLive(t *testing.T) {
	root := t.TempDir()
	src := buildFakeOneClick(t, filepath.Join(root, "pkg"), "v0.6.0")
	blobs := NewMemBlobStore()
	key := putUpload(t, blobs, src)
	jobA := store.ImportJob{
		ID: "job-a", Source: SourceUpload, SourceRef: key,
		Tag: "v0.6.0", Arch: ArchAMD64, Status: store.ImportRunning,
	}
	jobB := store.ImportJob{
		ID: "job-b", Source: SourceUpload, SourceRef: key,
		Tag: "v0.6.0", Arch: ArchARM64, Status: store.ImportPending,
	}
	fake := &fakeImportStore{job: jobA, siblings: []store.ImportJob{jobB}}
	im := newImporter(fake, blobs, FetchConfig{}, t.TempDir(), time.Minute)
	im.runJob(context.Background(), jobA)
	if _, err := blobs.Stat(context.Background(), key); err != nil {
		t.Fatalf("upload removed while sibling pending: %v", err)
	}
	im.runJob(context.Background(), jobB)
	if _, err := blobs.Stat(context.Background(), key); !IsNotExist(err) {
		t.Fatalf("upload left after last sibling: %v", err)
	}
}

func TestSweepUploadsTTL(t *testing.T) {
	blobs := NewMemBlobStore()
	ctx := context.Background()
	oldKey, newKey, liveKey := uploadsPrefix+"old.tar.gz", uploadsPrefix+"new.tar.gz", uploadsPrefix+"live.tar.gz"
	for _, key := range []string{oldKey, newKey, liveKey} {
		if _, err := blobs.Put(ctx, key, strings.NewReader("pkg"), "application/gzip"); err != nil {
			t.Fatal(err)
		}
	}
	blobs.SetLastModified(oldKey, time.Now().Add(-3*time.Hour))
	blobs.SetLastModified(liveKey, time.Now().Add(-3*time.Hour))
	fake := &fakeImportStore{job: store.ImportJob{
		ID: "live-1", Source: SourceUpload, SourceRef: liveKey,
		Status: store.ImportPending,
	}}
	im := newImporter(fake, blobs, FetchConfig{}, t.TempDir(), time.Minute)
	im.uploadTTL = time.Hour
	im.sweepUploads(ctx)
	if _, err := blobs.Stat(ctx, oldKey); !IsNotExist(err) {
		t.Fatal("stale unreferenced upload was not swept")
	}
	if _, err := blobs.Stat(ctx, newKey); err != nil {
		t.Fatalf("fresh upload swept: %v", err)
	}
	if _, err := blobs.Stat(ctx, liveKey); err != nil {
		t.Fatalf("live-job upload swept: %v", err)
	}
}

func TestSweepIncompleteUploadsAgeGate(t *testing.T) {
	blobs := NewMemBlobStore()
	young := IncompleteUpload{Key: uploadsPrefix + "young.tar.gz", UploadID: "u-young", Initiated: time.Now()}
	old := IncompleteUpload{Key: uploadsPrefix + "old.tar.gz", UploadID: "u-old", Initiated: time.Now().Add(-2 * time.Hour)}
	blobs.InjectIncomplete(young)
	blobs.InjectIncomplete(old)
	im := newImporter(&fakeImportStore{}, blobs, FetchConfig{}, t.TempDir(), 30*time.Minute)
	im.sweepUploads(context.Background())
	left, err := blobs.ListIncompleteUploads(context.Background(), uploadsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].UploadID != "u-young" {
		t.Fatalf("incomplete uploads=%v want only young", left)
	}
}

func TestReleaseHeldRequeues(t *testing.T) {
	fake := &fakeImportStore{job: store.ImportJob{ID: "held-1", Status: store.ImportRunning}}
	im := newImporter(fake, NewMemBlobStore(), FetchConfig{}, t.TempDir(), time.Minute)
	im.markHeld("held-1")
	im.ReleaseHeld(context.Background())
	got, err := fake.GetImportJob(context.Background(), "held-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ImportPending {
		t.Fatalf("status=%s want pending", got.Status)
	}
}

func TestMemBlobStorePutPartSize(t *testing.T) {
	m := NewMemBlobStore()
	if m.PutPartSize() != PutPartSize {
		t.Fatalf("part size=%d want %d", m.PutPartSize(), PutPartSize)
	}
	if PutPartSize != 64<<20 {
		t.Fatalf("PutPartSize=%d want 64MiB", PutPartSize)
	}
}

func TestPutCanceledReaderDoesNotLeaveObject(t *testing.T) {
	blobs := NewMemBlobStore()
	_, err := blobs.Put(context.Background(), "uploads/fail.tar.gz", failReader{}, "application/gzip")
	if err == nil {
		t.Fatal("expected put error")
	}
	if _, statErr := blobs.Stat(context.Background(), "uploads/fail.tar.gz"); !IsNotExist(statErr) {
		t.Fatal("failed put left an object")
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, os.ErrClosed }
