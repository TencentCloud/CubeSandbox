// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// A jfs package is already on the volume every node mounts, so publishing it
// takes no copy: its "remote uuids" are the absolute paths of its rootfs and
// memory files and of its package home, which are the same on every node.
// What another node must wait for is JuiceFS itself. With --writeback a
// closed file may still have blocks in the local staging area, and another
// node reading those blocks fails, so the package is ready only once none of
// its blocks is staged.

const (
	jfsStagingDir              = "rawstaging"
	jfsControlInfoV2           = uint32(1005)
	jfsControlProgress         = byte(0xFE)
	jfsControlData             = byte(0xFF)
	jfsControlInfoBodySize     = uint32(11)
	jfsControlMaxResponseBytes = 256 << 20
)

type jfsControlFile interface {
	io.Reader
	io.Writer
	io.Closer
}

// errJFSPackageIncomplete marks a package that exists but is missing a file
// every restore needs (catalog, guest metadata, VM config -- plus guest memory
// for a full package).
// Unlike a JuiceFS lookup or CLI failure this never becomes better on its own,
// so the status poller must settle it as failed instead of retrying forever:
// the S3 backend already fails such a package at export time.
var errJFSPackageIncomplete = errors.New("jfs package is missing required restore files")

var (
	jfsOpenControl    = openJFSControl
	jfsPathObjectKeys = queryJFSControlObjectKeys
	jfsObjectKeyRE    = regexp.MustCompile(`chunks/\d+/\d+/\d+_\d+_\d+`)
	// Each query opens its own control handle, allowing concurrent queries on
	// Linux. Keep a bounded pool so independent packages do not serialize.
	jfsControlQueryLimit = make(chan struct{}, 16)

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
// querying JuiceFS's control file once per file on every 100 ms status poll.
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
		if errors.Is(err, ErrSnapshotCatalogNotFound) || errors.Is(err, errJFSPackageIncomplete) {
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
	// metadata.json (guest runtime identity + writable layer) and the VM config
	// are required by every package: a filesystem-only package cold-boots from
	// them exactly like a full one. Guest memory is the only mode-specific file.
	required := []string{snapshotCatalogFileName, "metadata.json", "snapshot/config.json"}
	if files.Memory != "" {
		required = append(required, "snapshot/state.json")
	}
	if entry.Kind == CatalogKindPauseSnapshot {
		required = append(required, "sandbox_spec.json")
	}
	for _, name := range required {
		path := filepath.Join(meta, name)
		info, err := os.Stat(path)
		if err != nil {
			// A missing file will never appear by retrying; an I/O error on the
			// JuiceFS mount may clear on its own, so only ENOENT is terminal.
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: check jfs restore metadata %s: %w",
					errJFSPackageIncomplete, path, err)
			}
			return nil, fmt.Errorf("check jfs restore metadata %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: jfs restore metadata %s is not a regular file",
				errJFSPackageIncomplete, path)
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
		pathKeys, err := jfsPathObjectKeys(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("query JuiceFS object keys for %s: %w", p, err)
		}
		for _, key := range pathKeys {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func openJFSControl(path string) (jfsControlFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	dir := path
	if !info.IsDir() {
		dir = filepath.Dir(path)
	}
	control := filepath.Join(dir, ".jfs.control")
	file, err := os.OpenFile(control, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		control = filepath.Join(dir, ".control")
		file, err = os.OpenFile(control, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open JuiceFS control file %s: %w", control, err)
	}
	return file, nil
}

func queryJFSControlObjectKeys(ctx context.Context, path string) ([]string, error) {
	select {
	case jfsControlQueryLimit <- struct{}{}:
		defer func() { <-jfsControlQueryLimit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat JuiceFS path: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat JuiceFS path %s has no inode", path)
	}
	control, err := jfsOpenControl(path)
	if err != nil {
		return nil, err
	}
	defer control.Close()

	request := make([]byte, 8+jfsControlInfoBodySize)
	binary.BigEndian.PutUint32(request[0:4], jfsControlInfoV2)
	binary.BigEndian.PutUint32(request[4:8], jfsControlInfoBodySize)
	binary.BigEndian.PutUint64(request[8:16], stat.Ino)
	// recursive, raw and strict remain false.
	if n, err := control.Write(request); err != nil {
		return nil, fmt.Errorf("write JuiceFS InfoV2 request: %w", err)
	} else if n != len(request) {
		return nil, fmt.Errorf("write JuiceFS InfoV2 request: %w", io.ErrShortWrite)
	}
	data, err := readJFSControlData(ctx, control)
	if err != nil {
		return nil, err
	}
	var response struct {
		Failed  bool
		Reason  string
		Objects []struct {
			Key string
		}
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode JuiceFS InfoV2 response: %w", err)
	}
	if response.Failed {
		return nil, fmt.Errorf("JuiceFS InfoV2 failed: %s", response.Reason)
	}
	keys := make([]string, 0, len(response.Objects))
	for _, object := range response.Objects {
		if key := jfsObjectKeyRE.FindString(object.Key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func readJFSControlData(ctx context.Context, reader io.Reader) ([]byte, error) {
	const readSize = 128 << 10
	buf := make([]byte, readSize)
	var pending []byte
	delay := time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
		parse:
			for len(pending) > 0 {
				switch pending[0] {
				case jfsControlProgress:
					if len(pending) < 17 {
						break parse
					}
					pending = pending[17:]
					continue
				case jfsControlData:
					if len(pending) < 5 {
						break parse
					}
					size := binary.BigEndian.Uint32(pending[1:5])
					if size > jfsControlMaxResponseBytes {
						return nil, fmt.Errorf("JuiceFS control response too large: %d bytes", size)
					}
					if uint64(len(pending)-5) < uint64(size) {
						break parse
					}
					return append([]byte(nil), pending[5:5+size]...), nil
				default:
					if len(pending) == 1 {
						if errno := syscall.Errno(pending[0]); errno != 0 {
							return nil, fmt.Errorf("JuiceFS control response: %w", errno)
						}
					}
					return nil, fmt.Errorf("invalid JuiceFS control response marker 0x%02x", pending[0])
				}
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("read JuiceFS control response: %w", err)
			}
			// The control file may split a large response across reads. Keep
			// draining available data without applying the EOF retry backoff.
			delay = time.Millisecond
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read JuiceFS control response: %w", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 300*time.Millisecond)
	}
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
