// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// DefaultImportClaimStaleAfter is how long a running import may sit without a
// heartbeat before another replica reclaims it. Heartbeats run every 60s.
const DefaultImportClaimStaleAfter = 5 * time.Minute

// DefaultUploadTTL is how long an unused uploads/*.tar.gz may sit before sweep
// deletes it. The console starts import jobs immediately after upload; 2h covers
// an abandoned file without parking multi-GB archives in the bucket.
const DefaultUploadTTL = 2 * time.Hour

const (
	importHeartbeatInterval = 60 * time.Second
	orphanBlobGrace         = time.Hour
	mpuAgeSlack             = 15 * time.Minute
)

type importStore interface {
	ListImportWork(ctx context.Context, staleAfter time.Duration) ([]store.ImportJob, error)
	ClaimImportJob(ctx context.Context, id string, staleAfter time.Duration) (bool, error)
	UpdateImportJob(ctx context.Context, id, status, errMsg string, bytesTotal int64) error
	TouchImportJob(ctx context.Context, id string) (bool, error)
	RequeueImportJob(ctx context.Context, id string) error
	GetImportJob(ctx context.Context, id string) (*store.ImportJob, error)
	InsertWarehouseItem(ctx context.Context, item store.WarehouseItem) (inserted bool, err error)
	GetWarehouseItem(ctx context.Context, arch, component, version string) (*store.WarehouseItem, error)
	ListWarehouseItems(ctx context.Context) ([]store.WarehouseItem, error)
	CountLiveImportJobsBySourceRef(ctx context.Context, sourceRef string) (int, error)
}

type advisoryLocker interface {
	WithAdvisoryLock(ctx context.Context, name string, fn func(context.Context) error) error
}

// Importer runs one-click imports in the background.
type Importer struct {
	store   importStore
	blobs   BlobStore
	fetch   FetchConfig
	workDir string
	putTO   time.Duration

	wake            chan struct{}
	claimStaleAfter time.Duration
	uploadTTL       time.Duration
	executeCount    atomic.Int64

	heldMu sync.Mutex
	held   map[string]struct{}
}

func NewImporter(s *store.Store, blobs BlobStore, fetch FetchConfig, workDir string, uploadTimeout time.Duration) *Importer {
	return newImporter(s, blobs, fetch, workDir, uploadTimeout)
}

func newImporter(s importStore, blobs BlobStore, fetch FetchConfig, workDir string, uploadTimeout time.Duration) *Importer {
	if strings.TrimSpace(workDir) == "" {
		workDir = "/var/tmp/cubeops-warehouse"
	}
	if uploadTimeout <= 0 {
		uploadTimeout = 30 * time.Minute
	}
	return &Importer{
		store:           s,
		blobs:           blobs,
		fetch:           fetch,
		workDir:         workDir,
		putTO:           uploadTimeout,
		wake:            make(chan struct{}, 1),
		claimStaleAfter: DefaultImportClaimStaleAfter,
		uploadTTL:       DefaultUploadTTL,
		held:            map[string]struct{}{},
	}
}

func (im *Importer) Kick() {
	select {
	case im.wake <- struct{}{}:
	default:
	}
}

func (im *Importer) Run(ctx context.Context) {
	if im.blobs == nil {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		im.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-im.wake:
		case <-ticker.C:
		}
	}
}

// ReleaseHeld returns in-flight jobs to pending so a rolling update does not
// wait out claimStaleAfter.
func (im *Importer) ReleaseHeld(ctx context.Context) {
	im.heldMu.Lock()
	ids := make([]string, 0, len(im.held))
	for id := range im.held {
		ids = append(ids, id)
	}
	im.held = map[string]struct{}{}
	im.heldMu.Unlock()
	for _, id := range ids {
		if err := im.store.RequeueImportJob(ctx, id); err != nil {
			slog.Warn("requeue import job on shutdown", "id", id, "error", err)
		}
	}
}

func (im *Importer) markHeld(id string) {
	im.heldMu.Lock()
	im.held[id] = struct{}{}
	im.heldMu.Unlock()
}

func (im *Importer) unmarkHeld(id string) {
	im.heldMu.Lock()
	delete(im.held, id)
	im.heldMu.Unlock()
}

func (im *Importer) staleAfter() time.Duration {
	if im.claimStaleAfter > 0 {
		return im.claimStaleAfter
	}
	return DefaultImportClaimStaleAfter
}

func (im *Importer) drain(ctx context.Context) {
	jobs, err := im.store.ListImportWork(ctx, im.staleAfter())
	if err != nil {
		slog.Error("list pending import jobs", "error", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		claimed, err := im.store.ClaimImportJob(ctx, job.ID, im.staleAfter())
		if err != nil {
			slog.Error("claim import job", "id", job.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		im.markHeld(job.ID)
		im.runJob(ctx, job)
		im.unmarkHeld(job.ID)
	}
	im.sweepUploads(ctx)
	im.reconcileBlobs(ctx)
	im.sweepWorkDirs(ctx)
}

func (im *Importer) uploadMaxAge() time.Duration {
	if im.uploadTTL > 0 {
		return im.uploadTTL
	}
	return DefaultUploadTTL
}

func isLiveImportStatus(status string) bool {
	return status == store.ImportPending || status == store.ImportRunning
}

func (im *Importer) sweepUploads(ctx context.Context) {
	if im.blobs == nil {
		return
	}
	ttl := im.uploadMaxAge()
	now := time.Now()
	objs, err := im.blobs.List(ctx, uploadsPrefix)
	if err != nil {
		slog.Warn("list warehouse uploads", "error", err)
		return
	}
	for _, obj := range objs {
		if now.Sub(obj.LastModified) < ttl {
			continue
		}
		im.removeUploadIfIdle(ctx, obj.Key)
	}

	mpuAge := im.putTO + mpuAgeSlack
	uploads, err := im.blobs.ListIncompleteUploads(ctx, uploadsPrefix)
	if err != nil {
		slog.Warn("list incomplete warehouse uploads", "error", err)
		return
	}
	for _, u := range uploads {
		if now.Sub(u.Initiated) < mpuAge {
			continue
		}
		if err := im.blobs.AbortMultipartUpload(ctx, u.Key, u.UploadID); err != nil {
			slog.Warn("abort leftover multipart upload", "key", u.Key, "error", err)
		}
	}
}

func (im *Importer) removeUploadIfIdle(ctx context.Context, key string) {
	if key == "" || im.store == nil || im.blobs == nil {
		return
	}
	n, err := im.store.CountLiveImportJobsBySourceRef(ctx, key)
	if err != nil || n > 0 {
		return
	}
	_ = im.blobs.Delete(ctx, key)
}

func (im *Importer) sweepWorkDirs(ctx context.Context) {
	entries, err := os.ReadDir(im.workDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), "import-") {
			continue
		}
		id := strings.TrimPrefix(ent.Name(), "import-")
		if id == "" {
			continue
		}
		job, err := im.store.GetImportJob(ctx, id)
		if err != nil {
			continue
		}
		if job != nil && isLiveImportStatus(job.Status) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(im.workDir, ent.Name()))
	}
}

func (im *Importer) reconcileBlobs(ctx context.Context) {
	if im.blobs == nil || im.store == nil {
		return
	}
	if locker, ok := im.store.(advisoryLocker); ok {
		err := locker.WithAdvisoryLock(ctx, "cubeops_warehouse_reconcile_v1", func(ctx context.Context) error {
			im.reconcileBlobsLocked(ctx)
			return nil
		})
		if err != nil {
			slog.Warn("warehouse blob reconcile lock", "error", err)
		}
		return
	}
	im.reconcileBlobsLocked(ctx)
}

func (im *Importer) reconcileBlobsLocked(ctx context.Context) {
	objs, err := im.blobs.List(ctx, blobsPrefix)
	if err != nil {
		slog.Warn("list warehouse blobs", "error", err)
		return
	}
	items, err := im.store.ListWarehouseItems(ctx)
	if err != nil {
		slog.Warn("list warehouse catalog", "error", err)
		return
	}
	inDB := map[string]struct{}{}
	for _, item := range items {
		inDB[objectKeyOf(item.Arch, item.Component, item.Version, item.ObjectKey)] = struct{}{}
	}
	inS3 := map[string]struct{}{}
	now := time.Now()
	for _, obj := range objs {
		inS3[obj.Key] = struct{}{}
		if _, ok := inDB[obj.Key]; ok {
			continue
		}
		if now.Sub(obj.LastModified) < orphanBlobGrace {
			continue
		}
		if err := im.blobs.Delete(ctx, obj.Key); err != nil {
			slog.Warn("delete orphan warehouse blob", "key", obj.Key, "error", err)
		}
	}
	for _, item := range items {
		key := objectKeyOf(item.Arch, item.Component, item.Version, item.ObjectKey)
		if _, ok := inS3[key]; !ok {
			slog.Warn("warehouse catalog missing object",
				"arch", item.Arch, "component", item.Component, "version", item.Version, "key", key)
		}
	}
}

func (im *Importer) runJob(ctx context.Context, job store.ImportJob) {
	bytesTotal, err := im.execute(ctx, job)
	if err != nil {
		slog.Error("import job failed", "id", job.ID, "error", err)
		_ = im.store.UpdateImportJob(ctx, job.ID, store.ImportFailed, err.Error(), bytesTotal)
		im.releaseUpload(ctx, job)
		return
	}
	_ = im.store.UpdateImportJob(ctx, job.ID, store.ImportSucceeded, "", bytesTotal)
	im.releaseUpload(ctx, job)
}

func (im *Importer) releaseUpload(ctx context.Context, job store.ImportJob) {
	if job.Source != SourceUpload {
		return
	}
	im.removeUploadIfIdle(ctx, job.SourceRef)
}

func (im *Importer) execute(ctx context.Context, job store.ImportJob) (int64, error) {
	im.executeCount.Add(1)
	if im.blobs == nil {
		return 0, fmt.Errorf("warehouse blob store is not configured")
	}
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go im.heartbeat(runCtx, runCancel, job.ID)

	work := filepath.Join(im.workDir, "import-"+job.ID)
	defer os.RemoveAll(work)
	if err := os.RemoveAll(work); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(work, dirPerm); err != nil {
		return 0, err
	}

	archive := filepath.Join(work, "package.tar.gz")
	var bytesTotal int64
	switch job.Source {
	case SourceUpload:
		if strings.TrimSpace(job.SourceRef) == "" {
			return 0, fmt.Errorf("uploaded archive missing")
		}
		n, err := im.copyObjectToFile(runCtx, job.SourceRef, archive)
		if err != nil {
			return 0, err
		}
		bytesTotal = n
	case SourceGitHub, SourceCNB:
		archive = filepath.Join(work, oneClickAssetName(job.Tag, job.Arch))
		if err := im.fetch.DownloadRelease(job.Source, job.SourceRef, job.Tag, job.Arch, archive); err != nil {
			return 0, err
		}
		st, err := os.Stat(archive)
		if err != nil {
			return 0, err
		}
		bytesTotal = st.Size()
	default:
		return 0, fmt.Errorf("unsupported source %q", job.Source)
	}

	unpackRoot := filepath.Join(work, "unpack")
	extracted, err := UnpackOneClick(archive, unpackRoot)
	if err != nil {
		return bytesTotal, err
	}

	for _, item := range extracted {
		if runCtx.Err() != nil {
			return bytesTotal, runCtx.Err()
		}
		if err := im.installExtracted(runCtx, job, item); err != nil {
			return bytesTotal, err
		}
	}
	return bytesTotal, nil
}

func (im *Importer) heartbeat(ctx context.Context, cancel context.CancelFunc, id string) {
	defer cancel()
	ticker := time.NewTicker(importHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := im.store.TouchImportJob(ctx, id)
			if err != nil {
				slog.Warn("touch import job", "id", id, "error", err)
				continue
			}
			if !ok {
				slog.Warn("lost import job lease", "id", id)
				cancel()
				return
			}
		}
	}
}

func (im *Importer) copyObjectToFile(ctx context.Context, key, dest string) (int64, error) {
	rc, err := im.blobs.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("uploaded archive missing: %w", err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

func (im *Importer) installExtracted(ctx context.Context, job store.ImportJob, item ExtractedComponent) error {
	existing, err := im.store.GetWarehouseItem(ctx, job.Arch, item.Component, item.Version)
	if err != nil {
		return err
	}
	if existing != nil {
		slog.Info("warehouse skip existing version",
			"arch", job.Arch, "component", item.Component, "version", item.Version)
		return nil
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(WriteTarGz(pw, item.Dir))
	}()
	defer pr.Close()
	key := ObjectKey(job.Arch, item.Component, item.Version)
	info, err := im.blobs.Put(ctx, key, pr, "application/gzip")
	if err != nil {
		return err
	}
	inserted, err := im.store.InsertWarehouseItem(ctx, store.WarehouseItem{
		Arch:      job.Arch,
		Component: item.Component,
		Version:   item.Version,
		Source:    job.Source,
		SourceRef: job.Tag,
		ObjectKey: key,
		SizeBytes: info.Size,
		Checksum:  formatChecksum(info.SHA256),
	})
	if err != nil {
		return err
	}
	if !inserted {
		slog.Info("warehouse skip existing version after put",
			"arch", job.Arch, "component", item.Component, "version", item.Version)
	}
	return nil
}
