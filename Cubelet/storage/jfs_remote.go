// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// A jfs package is already on the volume every node mounts, so publishing it
// takes no copy: its "remote uuids" are the absolute paths of its rootfs and
// memory files and of its package home, which are the same on every node.
// What another node must wait for is JuiceFS itself. With --writeback a
// closed file may still have blocks in the local staging area, and another
// node reading those blocks fails, so the package is ready only once none of
// its blocks is staged.

const jfsStagingDir = "rawstaging"

var (
	jfsBinary = func() string {
		if p, err := exec.LookPath("juicefs"); err == nil {
			return p
		}
		return "/usr/local/bin/juicefs"
	}
	jfsInfoOutput = func(ctx context.Context, path string) ([]byte, error) {
		return exec.CommandContext(ctx, jfsBinary(), "info", path).CombinedOutput()
	}
	jfsObjectKeyRE = regexp.MustCompile(`chunks/\d+/\d+/\d+_\d+_\d+`)
	// A status burst must not turn into an unbounded number of JuiceFS CLI
	// processes. The limit must remain above the normal lifecycle concurrency:
	// each package inspects its files serially, so a smaller pool serializes
	// otherwise independent packages and delays READY under burst load.
	jfsInfoLimit = make(chan struct{}, 16)

	jfsUploadTrackers sync.Map // snapshot ID -> *jfsUploadTracker
)

const (
	jfsTrackerPollMin        = 100 * time.Millisecond
	jfsTrackerPollMax        = time.Second
	jfsTrackerGlobalIdleWait = 2 * time.Second
	jfsTrackerGlobalMaxWait  = 5 * time.Second
)

// jfsUploadTracker keeps the immutable object list of one finished package.
// UploadStatus is a query API, so repeated calls reuse this state instead of
// spawning `juicefs info` once per file on every 100 ms status poll.
type jfsUploadTracker struct {
	mu sync.Mutex

	files cow.RemoteUUIDs
	paths []string
	roots []string
	keys  []string

	initialized    bool
	sawStaged      bool
	checking       bool
	ready          bool
	lastStaged     int
	globalBlocks   int64
	globalSince    time.Time
	globalProgress time.Time
	nextCheck      time.Time
	backoff        time.Duration
}

type jfsUploader struct{}

// Upload implements [cow.Uploader]: it reports where the package's files are.
func (jfsUploader) Upload(ctx context.Context, snapshotID string) (*cow.RemoteUUIDs, error) {
	return jfsPackageFiles(ctx, snapshotID)
}

// UploadStatus implements [cow.Uploader]: ready once JuiceFS has uploaded
// every block of the package, including restore metadata.
func (jfsUploader) UploadStatus(ctx context.Context, snapshotID string) (*cow.RemoteStatus, error) {
	id := strings.TrimSpace(snapshotID)
	tracker, err := jfsUploadTrackerFor(ctx, id)
	if err != nil {
		if errors.Is(err, ErrSnapshotCatalogNotFound) {
			return &cow.RemoteStatus{SnapshotID: id, State: cow.RemoteStateFailed, Message: err.Error()}, nil
		}
		return nil, err
	}
	files := tracker.files
	st := &cow.RemoteStatus{SnapshotID: id, RemoteUUIDs: &files}
	staged, ready, err := tracker.check(ctx)
	if err != nil {
		return nil, err
	}
	if !ready {
		st.State = cow.RemoteStateRunning
		if staged > 0 {
			st.Message = fmt.Sprintf("%d blocks staged for upload", staged)
		} else {
			st.Message = "upload check pending"
		}
		return st, nil
	}
	st.State = cow.RemoteStateReady
	st.Message = "all blocks uploaded"
	return st, nil
}

func jfsUploadTrackerFor(ctx context.Context, snapshotID string) (*jfsUploadTracker, error) {
	if cached, ok := jfsUploadTrackers.Load(snapshotID); ok {
		return cached.(*jfsUploadTracker), nil
	}
	files, err := jfsPackageFiles(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	paths, err := jfsPackageUploadPaths(files)
	if err != nil {
		return nil, err
	}
	roots, err := jfsStagingRoots()
	if err != nil {
		return nil, err
	}
	created := &jfsUploadTracker{
		files: *files,
		paths: append([]string(nil), paths...),
		roots: append([]string(nil), roots...),
	}
	actual, _ := jfsUploadTrackers.LoadOrStore(snapshotID, created)
	return actual.(*jfsUploadTracker), nil
}

func deleteJFSUploadTracker(snapshotID string) {
	jfsUploadTrackers.Delete(strings.TrimSpace(snapshotID))
}

// check reports readiness without repeating JuiceFS metadata lookups. A zero
// global staging gauge is a safe fast path after all package files have been
// closed. Otherwise object keys are discovered once, checked in the local
// rawstaging tree, and refreshed once when a previously-staged package drains.
func (t *jfsUploadTracker) check(ctx context.Context) (int, bool, error) {
	t.mu.Lock()
	if t.ready {
		t.mu.Unlock()
		return 0, true, nil
	}
	t.mu.Unlock()

	global, hasGlobal := jfsGlobalStagingBlocks()
	if hasGlobal && global == 0 {
		t.mu.Lock()
		t.ready = true
		t.checking = false
		t.lastStaged = 0
		t.mu.Unlock()
		return 0, true, nil
	}

	now := time.Now()
	t.mu.Lock()
	// Another caller may have completed the tracker while .stats was read.
	if t.ready {
		t.mu.Unlock()
		return 0, true, nil
	}
	if hasGlobal && !t.initialized {
		if t.globalSince.IsZero() {
			t.globalSince = now
			t.globalProgress = now
			t.globalBlocks = global
		} else if global != t.globalBlocks {
			t.globalProgress = now
			t.globalBlocks = global
		}
		if now.Sub(t.globalSince) < jfsTrackerGlobalMaxWait &&
			now.Sub(t.globalProgress) < jfsTrackerGlobalIdleWait {
			t.lastStaged = int(global)
			staged := t.lastStaged
			t.mu.Unlock()
			return staged, false, nil
		}
	}
	if t.checking || now.Before(t.nextCheck) {
		staged := t.lastStaged
		t.mu.Unlock()
		return staged, false, nil
	}
	t.checking = true
	initialized := t.initialized
	sawStaged := t.sawStaged
	keys := append([]string(nil), t.keys...)
	paths := append([]string(nil), t.paths...)
	roots := append([]string(nil), t.roots...)
	t.mu.Unlock()

	if !initialized {
		var err error
		keys, err = jfsObjectKeys(ctx, paths...)
		if err != nil {
			t.finishCheck(0, false, nil, err)
			return 0, false, err
		}
	}
	staged, err := jfsStagedObjectKeys(keys, roots)
	if err != nil {
		t.finishCheck(0, false, nil, err)
		return 0, false, err
	}
	if staged == 0 && initialized && sawStaged {
		// The package is immutable, so keys normally stay fixed. Refresh once
		// at the transition to zero to cover a late slice/compaction update.
		keys, err = jfsObjectKeys(ctx, paths...)
		if err != nil {
			t.finishCheck(0, false, nil, err)
			return 0, false, err
		}
		staged, err = jfsStagedObjectKeys(keys, roots)
		if err != nil {
			t.finishCheck(0, false, nil, err)
			return 0, false, err
		}
	}
	ready := staged == 0
	t.finishCheck(staged, ready, keys, nil)
	return staged, ready, nil
}

func (t *jfsUploadTracker) finishCheck(staged int, ready bool, keys []string, checkErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Another caller may have observed the global-zero fast path while this
	// slower check was running; terminal READY must never move backwards.
	if t.ready {
		return
	}
	t.checking = false
	if checkErr != nil {
		t.scheduleNextLocked()
		return
	}
	t.initialized = true
	t.keys = append(t.keys[:0], keys...)
	t.lastStaged = staged
	t.sawStaged = t.sawStaged || staged > 0
	t.ready = ready
	if ready {
		t.nextCheck = time.Time{}
		t.backoff = 0
		return
	}
	t.scheduleNextLocked()
}

func (t *jfsUploadTracker) scheduleNextLocked() {
	if t.backoff == 0 {
		t.backoff = jfsTrackerPollMin
	} else {
		t.backoff = min(t.backoff*2, jfsTrackerPollMax)
	}
	t.nextCheck = time.Now().Add(t.backoff)
}

var _ cow.Uploader = jfsUploader{}

// jfsPackageFiles resolves a local jfs package to the shared paths another
// node imports it from.
func jfsPackageFiles(ctx context.Context, snapshotID string) (*cow.RemoteUUIDs, error) {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	entry, err := GetLocalSnapshotFor(ctx, cow.BackendJFS, id)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("jfs package %s: %w", id, ErrSnapshotCatalogNotFound)
	}
	store, err := requireCowStoreFor(cow.BackendJFS)
	if err != nil {
		return nil, err
	}
	resolve := func(name, kind string) (string, error) {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", nil
		}
		k, err := normalizeCowKind(kind)
		if err != nil {
			return "", err
		}
		return store.ResolveDevPath(ctx, name, k)
	}
	files := &cow.RemoteUUIDs{Metadata: strings.TrimSpace(entry.SnapshotPath)}
	if files.Rootfs, err = resolve(entry.RootfsVol, entry.RootfsKind); err != nil {
		return nil, fmt.Errorf("resolve jfs package %s rootfs: %w", id, err)
	}
	if files.Memory, err = resolve(entry.MemoryVol, entry.MemoryKind); err != nil {
		return nil, fmt.Errorf("resolve jfs package %s memory: %w", id, err)
	}
	if files.Rootfs == "" || files.Metadata == "" {
		return nil, fmt.Errorf("jfs package %s has no rootfs or home", id)
	}
	return files, nil
}

// jfsPackageUploadPaths validates required restore files and includes every
// metadata file, including catalog and optional producer-specific metadata.
func jfsPackageUploadPaths(files *cow.RemoteUUIDs) ([]string, error) {
	meta := filepath.Join(files.Metadata, SnapshotMetadataDir)
	entry, err := readSnapshotCatalogFile(filepath.Join(meta, snapshotCatalogFileName))
	if err != nil {
		return nil, fmt.Errorf("read jfs upload catalog: %w", err)
	}
	required := []string{snapshotCatalogFileName}
	if files.Memory != "" {
		required = append(required, "metadata.json", "snapshot/config.json", "snapshot/state.json")
	}
	if entry.Kind == CatalogKindPauseSnapshot {
		required = append(required, "sandbox_spec.json")
	}
	for _, name := range required {
		path := filepath.Join(meta, name)
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("check jfs restore metadata %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("jfs restore metadata %s is not a regular file", path)
		}
	}
	paths := []string{files.Rootfs, files.Memory}
	err = filepath.WalkDir(meta, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("jfs metadata %s is not a regular file", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan jfs restore metadata: %w", err)
	}
	return paths, nil
}

// jfsStagedBlocks counts the blocks of paths still waiting in the writeback
// staging area of this node's JuiceFS cache.
func jfsStagedBlocks(ctx context.Context, paths ...string) (int, error) {
	roots, err := jfsStagingRoots()
	if err != nil {
		return 0, err
	}
	keys, err := jfsObjectKeys(ctx, paths...)
	if err != nil {
		return 0, err
	}
	return jfsStagedObjectKeys(keys, roots)
}

func jfsStagingRoots() ([]string, error) {
	var cfg *Config
	if localStorage != nil {
		cfg = localStorage.config
	}
	var stagingRoots []string
	for _, dir := range cfg.jfsCacheDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read jfs cache %s: %w", dir, err)
		}
		found := false
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			found = true
			root := filepath.Join(dir, entry.Name(), jfsStagingDir)
			// JuiceFS creates rawstaging lazily and may remove it when empty.
			// Validate the volume cache directory even if rawstaging is absent.
			if _, err := os.ReadDir(filepath.Dir(root)); err != nil {
				return nil, fmt.Errorf("read jfs volume cache: %w", err)
			}
			if _, err := os.ReadDir(root); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("read jfs staging %s: %w", root, err)
			}
			stagingRoots = append(stagingRoots, root)
		}
		if !found {
			return nil, fmt.Errorf("jfs cache %s contains no volume cache directory", dir)
		}
	}
	if len(stagingRoots) == 0 {
		return nil, errors.New("no jfs cache directories configured")
	}
	return stagingRoots, nil
}

func jfsObjectKeys(ctx context.Context, paths ...string) ([]string, error) {
	seen := make(map[string]struct{})
	var keys []string
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		select {
		case jfsInfoLimit <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		out, err := jfsInfoOutput(ctx, p)
		<-jfsInfoLimit
		if err != nil {
			return nil, fmt.Errorf("juicefs info %s: %v: %s", p, err, strings.TrimSpace(string(out)))
		}
		for _, key := range jfsObjectKeyRE.FindAllString(string(out), -1) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func jfsStagedObjectKeys(keys, stagingRoots []string) (int, error) {
	staged := 0
	for _, key := range keys {
		for _, root := range stagingRoots {
			if _, err := os.Stat(filepath.Join(root, key)); err == nil {
				staged++
				break
			} else if !os.IsNotExist(err) {
				return 0, fmt.Errorf("stat jfs staged block %s: %w", key, err)
			}
		}
	}
	return staged, nil
}

// jfsGlobalStagingBlocks reads a local JuiceFS client gauge. Zero is a useful
// sufficient condition: no package on this mount can still depend on a local
// writeback block. Missing or malformed metrics simply disable the fast path.
func jfsGlobalStagingBlocks() (int64, bool) {
	var cfg *Config
	if localStorage != nil {
		cfg = localStorage.config
	}
	body, err := os.ReadFile(filepath.Join(cfg.jfsMountPath(), ".stats"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "juicefs_staging_blocks" {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || value < 0 {
			return 0, false
		}
		return int64(value), true
	}
	return 0, false
}

// jfsForeignPath validates a path another node handed over: it must lie in
// some node's tree on this node's JuiceFS mount.
func jfsForeignPath(raw string) (string, error) {
	var cfg *Config
	if localStorage != nil {
		cfg = localStorage.config
	}
	if cfg == nil {
		return "", errors.New("jfs storage is not configured")
	}
	p := filepath.Clean(strings.TrimSpace(raw))
	nodes := filepath.Clean(cfg.jfsNodesRoot())
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("jfs import source %q is outside %s", raw, nodes)
	}
	lexicalRel, lexicalErr := filepath.Rel(nodes, p)
	if lexicalErr != nil || lexicalRel == "." || lexicalRel == ".." ||
		strings.HasPrefix(lexicalRel, ".."+string(filepath.Separator)) || filepath.IsAbs(lexicalRel) {
		return "", fmt.Errorf("jfs import source %q is outside %s", raw, nodes)
	}
	resolvedNodes, err := filepath.EvalSymlinks(nodes)
	if err != nil {
		return "", fmt.Errorf("resolve jfs nodes root %s: %w", nodes, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve jfs import source %q: %w", raw, err)
	}
	rel, err := filepath.Rel(resolvedNodes, resolvedPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("jfs import source %q is outside %s", raw, resolvedNodes)
	}
	return resolvedPath, nil
}

// ensureJFSMetadata copies the package home into this sandbox's own home.
// The origin deletes its package once the restore succeeds, so the sandbox
// must not keep reading it. catalog.json stays behind: the copy is not a
// package of this node.
func (s *SandboxImport) ensureJFSMetadata() (string, error) {
	if fi, err := os.Stat(s.MetadataDir()); err == nil && fi.IsDir() {
		return s.MetadataDir(), nil
	}
	src, err := jfsForeignPath(s.UUIDs.Metadata)
	if err != nil {
		return "", err
	}
	if err := copyPackageHome(src, SnapshotHome(cow.BackendJFS, SnapshotKindNormal, s.SandboxID)); err != nil {
		return "", err
	}
	return s.MetadataDir(), nil
}

// sharedJFSMetadataDir returns the origin package's immutable metadata
// directory. Runtime snapshots are protected by Master's runtime references,
// so unlike pause packages they do not disappear after a successful restore.
func (s *SandboxImport) sharedJFSMetadataDir(ctx context.Context) (string, error) {
	registered, err := s.registeredJFSRuntimeSnapshot(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(registered.metadataHome, SnapshotMetadataDir), nil
}

type registeredJFSRuntimeSnapshotKey struct{}

type registeredJFSRuntimeSnapshot struct {
	snapshotID   string
	metadataHome string
	rootfsPath   string
	memoryPath   string
}

// WithRegisteredSharedJFSRuntimeSnapshot carries Master's READY registration
// through the Create request. Cube owns this internal metadata, so the target
// node uses its immutable paths directly instead of reopening the JFS catalog.
func WithRegisteredSharedJFSRuntimeSnapshot(ctx context.Context, s *SandboxImport) (context.Context, error) {
	if s == nil || !s.SharedJFSRuntimeSnapshot {
		return ctx, nil
	}
	metadataHome, err := jfsRegisteredPath(s.UUIDs.Metadata)
	if err != nil {
		return ctx, err
	}
	rootfsPath, err := jfsRegisteredPath(s.UUIDs.Rootfs)
	if err != nil {
		return ctx, err
	}
	memoryPath := ""
	if strings.TrimSpace(s.UUIDs.Memory) != "" {
		memoryPath, err = jfsRegisteredPath(s.UUIDs.Memory)
		if err != nil {
			return ctx, err
		}
	}
	registered := &registeredJFSRuntimeSnapshot{
		snapshotID:   strings.TrimSpace(s.SnapshotID),
		metadataHome: metadataHome,
		rootfsPath:   rootfsPath,
		memoryPath:   memoryPath,
	}
	return context.WithValue(ctx, registeredJFSRuntimeSnapshotKey{}, registered), nil
}

// RegisteredJFSRuntimeSnapshotHome returns the immutable package home selected
// by Master for this request, if it belongs to snapshotID.
func RegisteredJFSRuntimeSnapshotHome(ctx context.Context, snapshotID string) (string, bool) {
	registered, ok := ctx.Value(registeredJFSRuntimeSnapshotKey{}).(*registeredJFSRuntimeSnapshot)
	if !ok || registered == nil || registered.snapshotID != strings.TrimSpace(snapshotID) {
		return "", false
	}
	return registered.metadataHome, true
}

func (s *SandboxImport) registeredJFSRuntimeSnapshot(ctx context.Context) (*registeredJFSRuntimeSnapshot, error) {
	registered, ok := ctx.Value(registeredJFSRuntimeSnapshotKey{}).(*registeredJFSRuntimeSnapshot)
	if !ok || registered == nil || registered.snapshotID != strings.TrimSpace(s.SnapshotID) {
		return nil, errors.New("registered shared jfs runtime snapshot is missing")
	}
	return registered, nil
}

// jfsRegisteredPath performs only a local lexical containment check. The path
// came from Cube's immutable READY registration and is not user-controlled.
func jfsRegisteredPath(raw string) (string, error) {
	if localStorage == nil || localStorage.config == nil {
		return "", errors.New("jfs storage is not configured")
	}
	path := filepath.Clean(strings.TrimSpace(raw))
	nodesRoot := filepath.Clean(localStorage.config.jfsNodesRoot())
	rel, err := filepath.Rel(nodesRoot, path)
	if path == "." || !filepath.IsAbs(path) || err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("registered jfs path %q is outside %s", raw, nodesRoot)
	}
	return path, nil
}

// copyPackageHome copies the package home src to home, leaving an existing
// home alone. The copy lands under a temporary name first, so home is never
// seen half written.
func copyPackageHome(src, home string) error {
	if fi, err := os.Stat(home); err == nil && fi.IsDir() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
		return err
	}
	tmp := home + ".import"
	_ = os.RemoveAll(tmp)
	if err := copyPackageTree(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("copy jfs package home %s: %w", src, err)
	}
	if err := os.Rename(tmp, home); err != nil {
		_ = os.RemoveAll(tmp)
		if fi, statErr := os.Stat(home); statErr != nil || !fi.IsDir() {
			return err
		}
	}
	return nil
}

// importJFSVolume clones a file from another node's tree into a volume of
// this node named name.
func importJFSVolume(ctx context.Context, name, role, rawSrc string) (string, error) {
	src, err := jfsForeignPath(rawSrc)
	if err != nil {
		return "", err
	}
	return importJFSVolumePath(ctx, name, role, src)
}

func importRegisteredJFSVolume(ctx context.Context, name, role, src string) (string, error) {
	if strings.TrimSpace(src) == "" {
		return "", fmt.Errorf("registered jfs %s path is empty", role)
	}
	return importJFSVolumePath(ctx, name, role, src)
}

func importJFSVolumePath(ctx context.Context, name, role, src string) (string, error) {
	store, err := requireCowStoreFor(cow.BackendJFS)
	if err != nil {
		return "", err
	}
	xfs, ok := store.(*XfsCow)
	if !ok || xfs == nil {
		return "", fmt.Errorf("jfs cow store is not a reflink store")
	}
	importer, ok := xfs.engine.(cowVolumeImporter)
	if !ok || importer == nil {
		return "", fmt.Errorf("cubecow engine cannot import %s %s", role, name)
	}
	present, err := cowObjectPresent(xfs.GetVolumeInfo(ctx, name))
	if err != nil {
		return "", fmt.Errorf("lookup %s %s: %w", role, name, err)
	}
	if !present {
		if _, err := importer.ImportLvol(name, src); err != nil && !isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", fmt.Errorf("import %s %s from %s: %w", role, name, src, err)
		}
	}
	return xfs.ResolveDevPath(ctx, name, cowKindVolume)
}

// DeriveRollbackRootfsFromSharedJFS creates the writable rootfs generation
// used when a sandbox on this node rolls back to a global runtime snapshot.
func DeriveRollbackRootfsFromSharedJFS(ctx context.Context, sandboxID, sourcePath string, gen uint32, sizeBytes uint64) (*CowSnapshotObject, error) {
	name := SandboxRootfsName(sandboxID, gen)
	devPath, err := importJFSVolume(ctx, name, "global rollback rootfs", sourcePath)
	if err != nil {
		return nil, err
	}
	return &CowSnapshotObject{
		Name:      name,
		Kind:      cowKindVolume,
		DevPath:   devPath,
		SizeBytes: sizeBytes,
		Gen:       gen,
	}, nil
}

func copyPackageTree(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == filepath.Join(SnapshotMetadataDir, snapshotCatalogFileName) {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm()|0o700)
		case fi.Mode().IsRegular():
			return copyRegularFile(path, target, fi.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyRegularFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
