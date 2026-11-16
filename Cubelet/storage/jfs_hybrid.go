// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/jfsdiff"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// jfs local-rootfs mode ([cow.jfs] rootfs_placement = "local").
//
// The jfs backend normally keeps every object, including each sandbox's
// writable rootfs, on the shared JuiceFS mount. JuiceFS serves 4 KiB random
// writes two orders of magnitude slower than local XFS, so in local mode:
//
//   - Template rootfs stays on JuiceFS (built once, shared). Each node keeps a
//     sparse local replica of it in the XFS reflink store, created on first
//     use by copying the JuiceFS file while skipping holes and all-zero 1 MiB
//     chunks.
//   - A sandbox rootfs is a FICLONE of that replica, so runtime I/O is local.
//   - Pause / snapshot commits the rootfs with a local FICLONE. The package
//     home (catalog, VM state, sandbox spec) and memory stay on JuiceFS as
//     before. Asynchronously, the rootfs difference against the replica is
//     published into the package home as one sequential blob plus a manifest
//     (rootfs.diff, rootfs.diff.json) and marked with rootfs.published.
//     remote_status stays "running" until that marker exists and JuiceFS has
//     uploaded every block.
//   - Another node restores by cloning its own replica of the same template
//     and applying the published difference locally.
//
// Memory, template build objects and other volumes keep using the JuiceFS
// store unchanged. Every Store call routes to the local store when the named
// object exists there and to the shared store otherwise.

const (
	jfsRootfsDiffBlob     = "rootfs.diff"
	jfsRootfsDiffManifest = "rootfs.diff.json"
	jfsRootfsPublished    = "rootfs.published"

	jfsLocalDirName      = "jfs-local"
	jfsLocalReplicaDir   = "replicas"
	jfsLocalTmpDir       = "tmp"
	jfsLocalGCInterval   = 10 * time.Minute
	jfsLocalGCGrace      = 30 * time.Minute
	jfsLocalPublishLimit = 2
)

// errJFSRootfsPublishPending marks a local-rootfs package whose rootfs
// difference is not yet on JuiceFS. Status callers report it as running.
var errJFSRootfsPublishPending = errors.New("jfs package rootfs difference is being published")

// jfsLocalEngine is the subset of the local XFS cubecow handle used directly.
type jfsLocalEngine interface {
	CreateVolume(name string, sizeBytes uint64) (string, error)
	CreateSnapshotFromVolume(sourceName, snapshotName string, activate bool) (string, error)
	ImportLvol(lvolName, exportUUID string) (string, error)
	GetObjectInfo(name string) (*cubecow.Volume, error)
	ListSnapshots(volumeName string, pageSize uint64, pageToken string) (*cubecow.ListSnapshotsResult, error)
	DeleteSnapshot(name string) error
	DeleteVolume(name string) error
}

// jfsSharedEngine is the subset of the JuiceFS cubecow handle used directly.
type jfsSharedEngine interface {
	GetObjectInfo(name string) (*cubecow.Volume, error)
}

type jfsHybridStore struct {
	shared       cow.Store
	local        cow.Store
	localEngine  jfsLocalEngine
	sharedEngine jfsSharedEngine
	localRoot    string // <work>/xfs/jfs-local, same filesystem as the local objects

	locks      sync.Map // object name -> *sync.Mutex
	publishing sync.Map // snapshot ID -> struct{}
	publishSem chan struct{}
	// liveSources holds, per package rootfs name, the sandbox rootfs it was
	// just cloned from, kept open so publishing reads its warm page cache.
	liveSources sync.Map // package rootfs name -> *jfsLiveSource

	// packageEntry resolves a snapshot ID to its catalog entry on JuiceFS,
	// bypassing the in-memory catalog cache so packages deleted by another
	// node are seen as gone (overridable in tests).
	packageEntry func(ctx context.Context, snapshotID string) (*SnapshotCatalogEntry, error)
}

var _ cow.Store = (*jfsHybridStore)(nil)

func (l *local) newJFSHybridStore(shared cowVolumeManager, sharedEngine *cubecow.Engine) (*jfsHybridStore, error) {
	if err := l.ensureCowManager(); err != nil {
		return nil, err
	}
	if l.cowManager == nil || l.cowEngine == nil {
		return nil, fmt.Errorf("local XFS cubecow handle is not initialized")
	}
	root := filepath.Join(filepath.Dir(defaultReflinkAutoRootDir(l.config.DataPath)), jfsLocalDirName)
	h := &jfsHybridStore{
		shared:       shared,
		local:        l.cowManager,
		localEngine:  l.cowEngine,
		sharedEngine: sharedEngine,
		localRoot:    root,
		publishSem:   make(chan struct{}, jfsLocalPublishLimit),
		packageEntry: jfsPackageEntryFromDisk,
	}
	for _, d := range []string{h.replicaDir(), h.tmpDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Leftover copies from an interrupted replica import.
	if entries, err := os.ReadDir(h.tmpDir()); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(h.tmpDir(), e.Name()))
		}
	}
	return h, nil
}

func (h *jfsHybridStore) replicaDir() string { return filepath.Join(h.localRoot, jfsLocalReplicaDir) }
func (h *jfsHybridStore) tmpDir() string     { return filepath.Join(h.localRoot, jfsLocalTmpDir) }

// activeJFSHybrid returns the local-rootfs store when it is the published jfs
// Store, or nil.
func activeJFSHybrid() *jfsHybridStore {
	if localStorage == nil {
		return nil
	}
	_, m := localStorage.loadJFSCowState()
	h, _ := m.(*jfsHybridStore)
	return h
}

func jfsPackageEntryFromDisk(ctx context.Context, snapshotID string) (*SnapshotCatalogEntry, error) {
	_ = ctx
	return findSnapshotCatalogOnDisk(catalogBackendKey(cow.BackendJFS), snapshotID)
}

func (h *jfsHybridStore) packageHome(ctx context.Context, snapshotID string) (string, error) {
	entry, err := h.packageEntry(ctx, snapshotID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(entry.SnapshotPath) == "" {
		return "", fmt.Errorf("jfs package %s has no home", snapshotID)
	}
	return entry.SnapshotPath, nil
}

func (h *jfsHybridStore) lock(name string) func() {
	v, _ := h.locks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (h *jfsHybridStore) localInfo(name string) (*cubecow.Volume, bool) {
	info, err := h.localEngine.GetObjectInfo(name)
	if err != nil || info == nil {
		return nil, false
	}
	return info, true
}

func (h *jfsHybridStore) hasLocal(name string) bool {
	_, ok := h.localInfo(name)
	return ok
}

// pick returns the store holding name: local when it exists there.
func (h *jfsHybridStore) pick(name string) cow.Store {
	if h.hasLocal(strings.TrimSpace(name)) {
		return h.local
	}
	return h.shared
}

// ---- cow.Store ----

func (h *jfsHybridStore) Name() string { return h.shared.Name() }

func (h *jfsHybridStore) CreateDefaultMediumVolume(ctx context.Context, sandboxID, volumeName string, sizeBytes uint64) (*cowVolume, error) {
	return h.shared.CreateDefaultMediumVolume(ctx, sandboxID, volumeName, sizeBytes)
}

func (h *jfsHybridStore) CreateSandboxRootfsFromTemplate(ctx context.Context, sandboxID, templateID string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	if err := h.ensureLocalRootfs(ctx, cowTemplateRootfsName(templateID)); err != nil {
		return nil, err
	}
	return h.local.CreateSandboxRootfsFromTemplate(ctx, sandboxID, templateID, gen, desiredSizeBytes)
}

func (h *jfsHybridStore) RollbackDeriveNewGen(ctx context.Context, sandboxID, snapshotRootfsVol string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	if err := h.ensureLocalRootfs(ctx, snapshotRootfsVol); err != nil {
		return nil, err
	}
	return h.local.RollbackDeriveNewGen(ctx, sandboxID, snapshotRootfsVol, gen, desiredSizeBytes)
}

func (h *jfsHybridStore) CreateTemplateBuildRootfs(ctx context.Context, templateID string, sizeBytes uint64) (*cowVolume, error) {
	return h.shared.CreateTemplateBuildRootfs(ctx, templateID, sizeBytes)
}

type jfsLiveSource struct {
	f  *os.File
	at time.Time
}

const jfsLiveSourceTTL = 15 * time.Minute

// CommitTemplateRootfs commits locally when the source is a local runtime
// rootfs (pause / snapshot) and on JuiceFS otherwise (template build).
//
// For a local commit the source file stays open until the package is
// published: page cache is per inode, so the freshly cloned package reads
// cold from disk while the sandbox file the guest just wrote is still cached.
func (h *jfsHybridStore) CommitTemplateRootfs(ctx context.Context, sourceName, templateID string) (*cowVolume, error) {
	store := h.pick(sourceName)
	if store != h.local {
		return store.CommitTemplateRootfs(ctx, sourceName, templateID)
	}
	var live *os.File
	if info, ok := h.localInfo(strings.TrimSpace(sourceName)); ok && info.DevicePath != "" {
		live, _ = os.Open(info.DevicePath)
	}
	vol, err := h.local.CommitTemplateRootfs(ctx, sourceName, templateID)
	if err != nil {
		if live != nil {
			live.Close()
		}
		return nil, err
	}
	if live != nil {
		if old, loaded := h.liveSources.Swap(vol.VolumeName, &jfsLiveSource{f: live, at: time.Now()}); loaded {
			old.(*jfsLiveSource).f.Close()
		}
	}
	return vol, nil
}

// takeLiveSource returns and forgets the open sandbox file for a package.
func (h *jfsHybridStore) takeLiveSource(pkgName string) *os.File {
	v, ok := h.liveSources.LoadAndDelete(pkgName)
	if !ok {
		return nil
	}
	return v.(*jfsLiveSource).f
}

// expireLiveSources closes handles of packages that were never published.
func (h *jfsHybridStore) expireLiveSources() {
	h.liveSources.Range(func(k, v any) bool {
		if ls := v.(*jfsLiveSource); time.Since(ls.at) > jfsLiveSourceTTL {
			if h.liveSources.CompareAndDelete(k, v) {
				ls.f.Close()
			}
		}
		return true
	})
}

func (h *jfsHybridStore) CreateMemoryVolume(ctx context.Context, templateID string, sizeBytes uint64) (*cowVolume, error) {
	return h.shared.CreateMemoryVolume(ctx, templateID, sizeBytes)
}

func (h *jfsHybridStore) CommitTemplateMemory(ctx context.Context, sourceName, templateID string, sizeBytes uint64) (*cowVolume, error) {
	return h.shared.CommitTemplateMemory(ctx, sourceName, templateID, sizeBytes)
}

func (h *jfsHybridStore) CloneSandboxMemory(ctx context.Context, sandboxID, sourceName string) (*cowVolume, error) {
	return h.shared.CloneSandboxMemory(ctx, sandboxID, sourceName)
}

// DeleteByKind removes name from whichever store holds it. A template rootfs
// exists in both (JuiceFS original + local replica); the local replica is
// removed best-effort because sandboxes or packages may still be cloned from
// it, in which case the background GC retires it later.
func (h *jfsHybridStore) DeleteByKind(ctx context.Context, name, kind string) error {
	name = strings.TrimSpace(name)
	if !h.hasLocal(name) {
		return h.shared.DeleteByKind(ctx, name, kind)
	}
	if !h.isReplica(name) {
		return h.local.DeleteByKind(ctx, name, kind)
	}
	// Template rootfs: drop the JuiceFS original, then the replica if idle.
	var err error
	if _, serr := h.sharedEngine.GetObjectInfo(name); serr == nil {
		err = h.shared.DeleteByKind(ctx, name, kind)
	}
	unlock := h.lock(name)
	h.tryRetireReplica(name)
	unlock()
	return err
}

func (h *jfsHybridStore) DeactivateByKind(ctx context.Context, name, kind string) error {
	return h.pick(name).DeactivateByKind(ctx, name, kind)
}

func (h *jfsHybridStore) ResolveDevPath(ctx context.Context, name, kind string) (string, error) {
	return h.pick(name).ResolveDevPath(ctx, name, kind)
}

func (h *jfsHybridStore) GetSizeBytes(ctx context.Context, name string) (uint64, error) {
	return h.pick(name).GetSizeBytes(ctx, name)
}

func (h *jfsHybridStore) GetVolumeInfo(ctx context.Context, name string) (*cubecow.Volume, error) {
	return h.pick(name).GetVolumeInfo(ctx, name)
}

func (h *jfsHybridStore) GetMetrics(ctx context.Context) (map[string]uint64, error) {
	return h.shared.GetMetrics(ctx)
}

// ---- local rootfs materialization ----

// ensureLocalRootfs makes name available in the local store:
//   - already local: nothing to do (a stale template replica is refreshed);
//   - a published local-rootfs package: clone the base replica and apply the
//     published difference;
//   - otherwise (template, or a package made in shared mode): copy the
//     JuiceFS object sparsely into a local replica.
func (h *jfsHybridStore) ensureLocalRootfs(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("rootfs object name is required")
	}
	unlock := h.lock(name)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.hasLocal(name) {
		return h.refreshReplicaIfStale(ctx, name)
	}
	if id, ok := snapshotIDFromRootfsName(name); ok {
		if home, err := h.packageHome(ctx, id); err == nil {
			meta := filepath.Join(home, SnapshotMetadataDir)
			if _, err := os.Stat(filepath.Join(meta, jfsRootfsPublished)); err == nil {
				return h.importPublishedRootfs(ctx, name, meta)
			}
		}
	}
	return h.createReplica(ctx, name)
}

// snapshotIDFromRootfsName inverts cowTemplateRootfsName.
func snapshotIDFromRootfsName(name string) (string, bool) {
	if !strings.HasPrefix(name, "tpl-") || !strings.HasSuffix(name, "-rootfs") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "tpl-"), "-rootfs")
	if id == "" || strings.HasSuffix(id, "-build") {
		return "", false
	}
	return id, true
}

type jfsReplicaRecord struct {
	Name      string           `json:"name"`
	Source    jfsdiff.Identity `json:"source"`
	ReadBytes int64            `json:"read_bytes"`
	DataBytes int64            `json:"data_bytes"`
	CreatedAt string           `json:"created_at"`
}

func (h *jfsHybridStore) replicaRecordPath(name string) string {
	return filepath.Join(h.replicaDir(), name+".json")
}

func (h *jfsHybridStore) isReplica(name string) bool {
	_, err := os.Stat(h.replicaRecordPath(name))
	return err == nil
}

func (h *jfsHybridStore) readReplica(name string) (*jfsReplicaRecord, error) {
	raw, err := os.ReadFile(h.replicaRecordPath(name))
	if err != nil {
		return nil, err
	}
	var rec jfsReplicaRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (h *jfsHybridStore) sharedPath(name string) (string, error) {
	info, err := h.sharedEngine.GetObjectInfo(name)
	if err != nil {
		return "", err
	}
	if info == nil || strings.TrimSpace(info.DevicePath) == "" {
		return "", fmt.Errorf("jfs object %s has no path", name)
	}
	return info.DevicePath, nil
}

// createReplica copies the JuiceFS object name into the local store.
// Caller holds the name lock.
func (h *jfsHybridStore) createReplica(ctx context.Context, name string) error {
	src, err := h.sharedPath(name)
	if err != nil {
		return fmt.Errorf("local rootfs %s: no local copy and no JuiceFS object: %w", name, err)
	}
	ident, err := jfsdiff.StatIdentity(src)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(h.tmpDir(), name+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	start := time.Now()
	read, written, err := jfsdiff.ZeroSkipCopy(src, tmpPath)
	if err != nil {
		return fmt.Errorf("copy %s to local replica: %w", src, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := h.localEngine.ImportLvol(name, tmpPath); err != nil {
		return fmt.Errorf("import local replica %s: %w", name, err)
	}
	rec := jfsReplicaRecord{Name: name, Source: ident, ReadBytes: read, DataBytes: written,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(rec)
	if err := jfsdiff.WriteFileAtomic(h.replicaRecordPath(name), raw); err != nil {
		_ = h.localEngine.DeleteVolume(name)
		return err
	}
	CubeLog.Warnf("jfs local rootfs: replica %s from %s (read %d, data %d bytes) in %s",
		name, src, read, written, time.Since(start).Round(time.Millisecond))
	return nil
}

// refreshReplicaIfStale replaces a template replica whose JuiceFS source was
// rebuilt under the same name. Caller holds the name lock.
func (h *jfsHybridStore) refreshReplicaIfStale(ctx context.Context, name string) error {
	rec, err := h.readReplica(name)
	if err != nil {
		return nil // not a replica (local package / sandbox object)
	}
	src, err := h.sharedPath(name)
	if err != nil {
		return nil // source gone; the replica keeps serving existing clones
	}
	cur, err := jfsdiff.StatIdentity(src)
	if err != nil || cur.Equal(rec.Source) {
		return nil
	}
	if h.replicaHasClones(name) {
		return fmt.Errorf("local replica %s is stale (JuiceFS source changed) but still has clones; retire them first", name)
	}
	if err := h.localEngine.DeleteVolume(name); err != nil && !isCowSemantic(err, cubecow.SemNotFound) {
		return fmt.Errorf("drop stale local replica %s: %w", name, err)
	}
	_ = os.Remove(h.replicaRecordPath(name))
	CubeLog.Warnf("jfs local rootfs: replica %s was stale (source changed), recopying", name)
	return h.createReplica(ctx, name)
}

func (h *jfsHybridStore) replicaHasClones(name string) bool {
	res, err := h.localEngine.ListSnapshots(name, 1, "")
	return err != nil || (res != nil && len(res.Snapshots) > 0)
}

// tryRetireReplica deletes a template replica once nothing is cloned from it.
func (h *jfsHybridStore) tryRetireReplica(name string) {
	if h.replicaHasClones(name) {
		return
	}
	if err := h.localEngine.DeleteVolume(name); err != nil && !isCowSemantic(err, cubecow.SemNotFound) {
		CubeLog.Warnf("jfs local rootfs: retire replica %s: %v", name, err)
		return
	}
	_ = os.Remove(h.replicaRecordPath(name))
	CubeLog.Infof("jfs local rootfs: retired replica %s", name)
}

// importPublishedRootfs rebuilds a local-rootfs package on this node from the
// published difference. Caller holds the name lock.
func (h *jfsHybridStore) importPublishedRootfs(ctx context.Context, name, meta string) error {
	m, err := jfsdiff.ReadManifest(filepath.Join(meta, jfsRootfsDiffManifest))
	if err != nil {
		return fmt.Errorf("read rootfs diff manifest for %s: %w", name, err)
	}
	start := time.Now()
	var dev string
	var baseDur, cloneDur time.Duration
	if m.Base != "" {
		if m.Base == name {
			return fmt.Errorf("rootfs diff manifest for %s names itself as base", name)
		}
		tb := time.Now()
		if err := h.ensureLocalRootfs(ctx, m.Base); err != nil {
			return fmt.Errorf("prepare base %s for %s: %w", m.Base, name, err)
		}
		baseDur = time.Since(tb)
		if m.BaseIdentity != nil {
			rec, err := h.readReplica(m.Base)
			if err != nil {
				return fmt.Errorf("base %s of %s is not a template replica on this node: %w", m.Base, name, err)
			}
			if !rec.Source.Equal(*m.BaseIdentity) {
				return fmt.Errorf("base %s of %s differs from the version the package was made against (have %+v, want %+v)",
					m.Base, name, rec.Source, *m.BaseIdentity)
			}
		}
		tc := time.Now()
		dev, err = h.localEngine.CreateSnapshotFromVolume(m.Base, name, true)
		if err != nil {
			return fmt.Errorf("clone base %s to %s: %w", m.Base, name, err)
		}
		cloneDur = time.Since(tc)
	} else {
		dev, err = h.localEngine.CreateVolume(name, uint64(m.SizeBytes))
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	ios, err := jfsdiff.Apply(dev, filepath.Join(meta, jfsRootfsDiffBlob), m)
	if err != nil {
		if m.Base != "" {
			_ = h.localEngine.DeleteSnapshot(name)
		} else {
			_ = h.localEngine.DeleteVolume(name)
		}
		return fmt.Errorf("apply rootfs diff to %s: %w", name, err)
	}
	CubeLog.Warnf("jfs local rootfs: imported %s segments=%d bytes=%d base_ready=%s clone=%s blob_read=%s local_write=%s punch=%s total=%s",
		name, len(m.Segments), m.BlobBytes, baseDur.Round(time.Millisecond), cloneDur.Round(time.Millisecond),
		time.Duration(ios.ReadNs).Round(time.Millisecond), time.Duration(ios.WriteNs).Round(time.Millisecond),
		time.Duration(ios.PunchNs).Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	return nil
}

// ---- publishing ----

type jfsPublishRecord struct {
	PublishedAt string `json:"published_at"`
	Base        string `json:"base,omitempty"`
	BlobBytes   int64  `json:"blob_bytes"`
	Segments    int    `json:"segments"`
	Holes       int    `json:"holes"`
	DiffMs      int64  `json:"diff_ms"`
	WriteMs     int64  `json:"write_ms"`
	// Breakdown of WriteMs and context.
	StartDelayMs         int64 `json:"start_delay_ms"`
	ReadMs               int64 `json:"read_ms"`
	BlobWriteMs          int64 `json:"blob_write_ms"`
	SyncMs               int64 `json:"sync_ms"`
	ManifestMs           int64 `json:"manifest_ms"`
	StagingBlocksAtStart int64 `json:"staging_blocks_at_start"`
	StagingBlocksAtEnd   int64 `json:"staging_blocks_at_end"`
	// LiveSourceBytes were read from the still-cached sandbox rootfs.
	LiveSourceBytes int64 `json:"live_source_bytes"`
}

// jfsRootfsPublishedIn reports whether meta holds a published rootfs diff.
func jfsRootfsPublishedIn(meta string) bool {
	_, err := os.Stat(filepath.Join(meta, jfsRootfsPublished))
	return err == nil
}

// publishAsync starts publishing snapshotID's rootfs difference unless that is
// already running. Completion is observed through the published marker.
func (h *jfsHybridStore) publishAsync(snapshotID string) {
	if _, loaded := h.publishing.LoadOrStore(snapshotID, struct{}{}); loaded {
		return
	}
	go func() {
		defer h.publishing.Delete(snapshotID)
		h.publishSem <- struct{}{}
		defer func() { <-h.publishSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := h.publish(ctx, snapshotID); err != nil {
			CubeLog.Errorf("jfs local rootfs: publish %s: %v", snapshotID, err)
		}
	}()
}

// publish writes the rootfs difference of a local package into its JuiceFS
// home. Idempotent.
func (h *jfsHybridStore) publish(ctx context.Context, snapshotID string) error {
	entry, err := h.packageEntry(ctx, snapshotID)
	if err != nil {
		return err
	}
	meta := filepath.Join(entry.SnapshotPath, SnapshotMetadataDir)
	if jfsRootfsPublishedIn(meta) {
		return nil
	}
	name := strings.TrimSpace(entry.RootfsVol)
	unlock := h.lock(name)
	defer unlock()
	info, ok := h.localInfo(name)
	if !ok || strings.TrimSpace(info.DevicePath) == "" {
		return fmt.Errorf("package %s rootfs %s is not on this node", snapshotID, name)
	}
	pkgPath := info.DevicePath
	// cubecow flattens every snapshot under its ultimate origin volume, so
	// the directory holding the file names the replica it was cloned from.
	base := filepath.Base(filepath.Dir(pkgPath))
	var basePath string
	var baseIdent *jfsdiff.Identity
	if base == name || base == "" || base == "." {
		base = ""
	} else if rec, err := h.readReplica(base); err == nil {
		if binfo, ok := h.localInfo(base); ok {
			basePath = binfo.DevicePath
			ident := rec.Source
			baseIdent = &ident
		} else {
			base = ""
		}
	} else {
		// The origin is not a template replica (for example a package copied
		// from shared mode): publish a self-contained image.
		base = ""
	}

	stagingStart, _ := jfsGlobalStagingBlocks()
	var startDelay time.Duration
	if st, err := os.Stat(pkgPath); err == nil {
		startDelay = time.Since(st.ModTime()) // time since the pause committed the rootfs
	}
	t0 := time.Now()
	d, err := jfsdiff.Compute(pkgPath, basePath)
	if err != nil {
		return err
	}
	diffDur := time.Since(t0)

	t1 := time.Now()
	blobPath := filepath.Join(meta, jfsRootfsDiffBlob)
	tmpBlob := blobPath + ".tmp"
	f, err := os.OpenFile(tmpBlob, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	live := h.takeLiveSource(name)
	if live != nil {
		defer live.Close()
	}
	segs, blobBytes, ios, fromLive, err := jfsdiff.WriteBlobPreferring(pkgPath, live, d, f)
	tSync := time.Now()
	if err == nil {
		err = f.Sync()
	}
	syncDur := time.Since(tSync)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpBlob, blobPath)
	}
	if err != nil {
		_ = os.Remove(tmpBlob)
		return fmt.Errorf("write rootfs diff blob: %w", err)
	}
	tMan := time.Now()
	m := jfsdiff.Manifest{
		Version: jfsdiff.ManifestVersion, Base: base, BaseIdentity: baseIdent,
		SizeBytes: d.SizeBytes, Blob: jfsRootfsDiffBlob, BlobBytes: blobBytes,
		Segments: segs, Holes: d.Holes,
	}
	raw, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	if err := jfsdiff.WriteFileAtomic(filepath.Join(meta, jfsRootfsDiffManifest), raw); err != nil {
		return err
	}
	manDur := time.Since(tMan)
	writeDur := time.Since(t1)
	stagingEnd, _ := jfsGlobalStagingBlocks()
	rec := jfsPublishRecord{
		PublishedAt: time.Now().UTC().Format(time.RFC3339Nano), Base: base, BlobBytes: blobBytes,
		Segments: len(segs), Holes: len(d.Holes), DiffMs: diffDur.Milliseconds(), WriteMs: writeDur.Milliseconds(),
		StartDelayMs: startDelay.Milliseconds(), ReadMs: ios.ReadNs / 1e6, BlobWriteMs: ios.WriteNs / 1e6,
		SyncMs: syncDur.Milliseconds(), ManifestMs: manDur.Milliseconds(),
		StagingBlocksAtStart: stagingStart, StagingBlocksAtEnd: stagingEnd,
		LiveSourceBytes: fromLive,
	}
	raw, _ = json.Marshal(rec)
	if err := jfsdiff.WriteFileAtomic(filepath.Join(meta, jfsRootfsPublished), raw); err != nil {
		return err
	}
	deleteJFSUploadTracker(snapshotID)
	// WARN so the timing survives the runtime log level (INFO is filtered).
	CubeLog.Warnf("jfs local rootfs: published %s base=%q segments=%d bytes=%d holes=%d start_delay=%s diff=%s read=%s blob_write=%s sync=%s manifest=%s total=%s staging_blocks=%d->%d from_live=%d",
		snapshotID, base, len(segs), blobBytes, len(d.Holes), startDelay.Round(time.Millisecond),
		diffDur.Round(time.Millisecond), time.Duration(ios.ReadNs).Round(time.Millisecond),
		time.Duration(ios.WriteNs).Round(time.Millisecond), syncDur.Round(time.Millisecond),
		manDur.Round(time.Millisecond), (diffDur + writeDur).Round(time.Millisecond), stagingStart, stagingEnd, fromLive)
	return nil
}

// ---- background: re-publish after restart, retire orphans ----

func (h *jfsHybridStore) startBackground(ctx context.Context) {
	go func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			h.sweep(ctx)
			timer.Reset(jfsLocalGCInterval)
		}
	}()
}

// sweep walks every local template replica and the package rootfs cloned
// from it: unpublished packages whose home exists are (re)published; packages
// whose home is gone (deleted, or resumed and cleaned up on another node) are
// deleted after a grace period; replicas with no clones whose JuiceFS source
// is gone are retired.
func (h *jfsHybridStore) sweep(ctx context.Context) {
	h.expireLiveSources()
	entries, err := os.ReadDir(h.replicaDir())
	if err != nil {
		CubeLog.Warnf("jfs local rootfs sweep: %v", err)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		replica := strings.TrimSuffix(e.Name(), ".json")
		h.sweepReplica(ctx, replica)
	}
}

func (h *jfsHybridStore) sweepReplica(ctx context.Context, replica string) {
	token := ""
	for {
		res, err := h.localEngine.ListSnapshots(replica, 256, token)
		if err != nil {
			if isCowSemantic(err, cubecow.SemNotFound) {
				_ = os.Remove(h.replicaRecordPath(replica))
			}
			return
		}
		if res == nil {
			break
		}
		for _, s := range res.Snapshots {
			id, ok := snapshotIDFromRootfsName(s.Name)
			if !ok {
				continue // sandbox rootfs and other clones follow sandbox lifecycle
			}
			h.sweepPackage(ctx, id, s.Name, s.DevicePath)
		}
		if res.NextPageToken == "" {
			break
		}
		token = res.NextPageToken
	}
	if _, err := h.sharedEngine.GetObjectInfo(replica); err != nil && isCowSemantic(err, cubecow.SemNotFound) {
		unlock := h.lock(replica)
		h.tryRetireReplica(replica)
		unlock()
	}
}

func (h *jfsHybridStore) sweepPackage(ctx context.Context, id, name, devPath string) {
	home, err := h.packageHome(ctx, id)
	if err == nil {
		if !jfsRootfsPublishedIn(filepath.Join(home, SnapshotMetadataDir)) {
			h.publishAsync(id)
		}
		return
	}
	if !errors.Is(err, ErrSnapshotCatalogNotFound) {
		return
	}
	if devPath == "" {
		if info, ok := h.localInfo(name); ok {
			devPath = info.DevicePath
		}
	}
	// Without a path the age is unknown: never delete what may be a pause
	// that has committed its rootfs but not yet published its home.
	st, err := os.Stat(devPath)
	if devPath == "" || err != nil || time.Since(st.ModTime()) < jfsLocalGCGrace {
		return
	}
	unlock := h.lock(name)
	defer unlock()
	if err := h.localEngine.DeleteSnapshot(name); err != nil && !isCowSemantic(err, cubecow.SemNotFound) {
		CubeLog.Warnf("jfs local rootfs sweep: delete orphan package rootfs %s: %v", name, err)
		return
	}
	CubeLog.Infof("jfs local rootfs sweep: deleted orphan package rootfs %s (package %s has no home)", name, id)
}

// jfsLocalPackageState inspects a jfs package for local-rootfs publishing.
// published: the diff is on JuiceFS. pendingHere: the package rootfs is a
// local object on this node that still has to be published.
func jfsLocalPackageState(entry *SnapshotCatalogEntry) (meta string, published, pendingHere bool) {
	meta = filepath.Join(entry.SnapshotPath, SnapshotMetadataDir)
	if jfsRootfsPublishedIn(meta) {
		return meta, true, false
	}
	h := activeJFSHybrid()
	if h == nil {
		return meta, false, false
	}
	name := strings.TrimSpace(entry.RootfsVol)
	// A template's rootfs stays on JuiceFS; a local copy of it is a replica,
	// not a package to publish.
	return meta, false, h.hasLocal(name) && !h.isReplica(name)
}
