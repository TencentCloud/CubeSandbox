// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// xfs-s3 keeps the xfs data plane and adds one thing to a pause: the sealed
// package is published to S3 as a self-contained checkpoint.
//
//   - The origin seals the package locally (fsync of files and directories),
//     records an export job next to it and returns from Pause. The job
//     uploads every file and commits the session by writing the fixed
//     candidate key last. It survives a Cubelet restart.
//   - A node other than the origin reads every object back and checks it
//     against the manifest. Only that node reports ready, and the digest it
//     verified travels with the locator Master keeps.
//   - A cross-node Resume downloads the verified session in full, imports
//     the disks into the local reflink pool under the package's own object
//     names and writes the catalog, after which the target holds an ordinary
//     xfs pause package and the ordinary xfs Resume runs.

const (
	xfss3JobFileName = "checkpoint.json"

	xfss3RoleOrigin       = "origin"
	xfss3RoleMaterialized = "materialized"

	xfss3StateExporting = "exporting"
	xfss3StateExported  = "exported"
	xfss3StateFailed    = "failed"
	xfss3StateRetired   = "retired"

	xfss3StagingDirName = "checkpoint-staging"
	xfss3CatalogAttr    = "catalog"
	xfss3OriginAttr     = "origin_node"
)

// XFSS3Locator is what Pause hands Master as remote_uuids. Master adds the
// manifest digest the readback verified, and a cross-node Resume refuses any
// other session.
type XFSS3Locator struct {
	Checkpoint     string `json:"checkpoint"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
}

// ParseXFSS3Locator decodes remote_uuids of an xfs-s3 pause.
func ParseXFSS3Locator(raw string) (*XFSS3Locator, error) {
	var loc XFSS3Locator
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &loc); err != nil {
		return nil, fmt.Errorf("decode xfs-s3 locator: %w", err)
	}
	if strings.TrimSpace(loc.Checkpoint) == "" {
		return nil, fmt.Errorf("xfs-s3 locator names no checkpoint")
	}
	return &loc, nil
}

// JSON encodes the locator.
func (l *XFSS3Locator) JSON() string {
	raw, _ := json.Marshal(l)
	return string(raw)
}

type xfss3Job struct {
	Role           string `json:"role"`
	SnapshotID     string `json:"snapshot_id"`
	Session        string `json:"session"`
	State          string `json:"state"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	Error          string `json:"error,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	DataBytes      int64  `json:"data_bytes,omitempty"`
	LogicalBytes   int64  `json:"logical_bytes,omitempty"`
	// TransferMS ends when the checkpoint is committed; the sweep after it
	// runs in the background and is timed in Sweep.
	TransferMS int64 `json:"transfer_ms,omitempty"`
	// Incremental exports: the sandbox and the template it started
	// from, the base the export was cut against and what it moved.
	SandboxID  string                       `json:"sandbox_id,omitempty"`
	TemplateID string                       `json:"template_id,omitempty"`
	Mode       string                       `json:"mode,omitempty"`
	Base       string                       `json:"base,omitempty"`
	Stats      *checkpoint.IncrementalStats `json:"stats,omitempty"`
	// Generation is the lineage generation the export uploads into, decided
	// when the job is created: above the generation of RestoredFrom, the
	// checkpoint the paused instance had been restored from.
	Generation   int64              `json:"generation,omitempty"`
	RestoredFrom *xfss3RestoredFrom `json:"restored_from,omitempty"`
	// Sweep is the background deletion of what this checkpoint superseded
	// in the lineage.
	Sweep *xfss3SweepRecord `json:"sweep,omitempty"`
	// Import says how much of a materialized package came from this node's
	// baseline of the sandbox rather than from the store.
	Import *checkpoint.MaterializeStats `json:"import,omitempty"`

	// sweep is the sweep an export registered, started once the job is
	// recorded exported.
	sweep *xfss3Sweep
}

var (
	xfss3ClientMu  sync.Mutex
	xfss3ClientVal *checkpoint.Client
	// xfss3NewClient is a test hook.
	xfss3NewClient = func() (*checkpoint.Client, error) {
		path := xfss3ConfigPath()
		cfg, err := checkpoint.LoadConfig(path)
		if err != nil {
			return nil, fmt.Errorf("xfs-s3 checkpoint store: %w", err)
		}
		// One cap for every transfer of this node: exports, readbacks
		// and restores share it.
		checkpoint.SetBandwidthLimit(cfg.BandwidthBytesPerSecond())
		return checkpoint.NewClient(*cfg)
	}

	xfss3Exports  sync.Map // snapshot id -> *xfss3Export
	xfss3Verifies sync.Map // snapshot id -> *xfss3Verify
	xfss3Locks    sync.Map // snapshot id -> *sync.Mutex
)

// xfss3ConfigPath is the credential file: CUBE_XFSS3_CONFIG, else the
// RCOW_S3_CFG override one-click shares with s3lvol (exported to Cubelet
// from .one-click.env), else the one-click default.
func xfss3ConfigPath() string {
	for _, env := range []string{"CUBE_XFSS3_CONFIG", "RCOW_S3_CFG"} {
		if p := strings.TrimSpace(os.Getenv(env)); p != "" {
			return p
		}
	}
	return checkpoint.DefaultConfigPath
}

// XFSS3Ready reports whether this node can export xfs-s3 checkpoints. Pause
// asks before it freezes the guest: a node without a usable store refuses
// the pause and leaves the sandbox running, instead of finding out after
// the package exists. A store seen to overwrite a committed object refuses
// too, unless the configuration accepts that (conditional_writes=optional).
// Until the startup probe has an answer, the store is asked here, within
// xfss3ReadyProbeTimeout, so a Pause that comes first is not frozen into an
// export that can never commit. A store that cannot be asked right now does
// not refuse: the export waits for it, as it waits for the upload.
func XFSS3Ready() error {
	c, err := xfss3Client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), xfss3ReadyProbeTimeout)
	defer cancel()
	if err := xfss3CheckConditionalWrites(ctx, c); errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
		return err
	}
	return nil
}

// xfss3ReadyProbeTimeout bounds how long a Pause waits for the store to say
// whether it honours If-None-Match; the answer is kept afterwards.
const xfss3ReadyProbeTimeout = 10 * time.Second

// The store must refuse to overwrite an object under If-None-Match: *, or a
// committed session could be replaced after readers verified it. What the
// store answered is kept for the life of the process, like the client.
var (
	xfss3CondMu       sync.Mutex
	xfss3CondKnown    bool
	xfss3CondErr      error
	xfss3CondDegraded bool
	// xfss3ProbeConditional is a test hook.
	xfss3ProbeConditional = func(ctx context.Context, c *checkpoint.Client) error {
		return c.ProbeConditionalWrite(ctx)
	}
)

const xfss3CondProbeTimeout = 30 * time.Second

// xfss3CheckConditionalWrites returns nil once the store has refused an
// overwrite, an error wrapping ErrConditionalWriteUnsupported once it has
// been seen not to, and any other error while it cannot be asked.
func xfss3CheckConditionalWrites(ctx context.Context, c *checkpoint.Client) error {
	xfss3CondMu.Lock()
	known, knownErr := xfss3CondKnown, xfss3CondErr
	xfss3CondMu.Unlock()
	if known {
		return knownErr
	}
	pctx, cancel := context.WithTimeout(ctx, xfss3CondProbeTimeout)
	defer cancel()
	err := xfss3ProbeConditional(pctx, c)
	if err != nil && !errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
		return err
	}
	if err != nil && c.ConditionalWritesOptional() {
		// The operator accepted a store without If-None-Match: commits are
		// checked around the write instead (see putImmutableUnconditional).
		c.UseConditionalWrites(false)
		xfss3CondMu.Lock()
		defer xfss3CondMu.Unlock()
		xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = true, nil, true
		return nil
	}
	xfss3CondMu.Lock()
	defer xfss3CondMu.Unlock()
	xfss3CondKnown, xfss3CondErr = true, err
	return err
}

// xfss3CommitsDegraded reports whether commits run without If-None-Match.
func xfss3CommitsDegraded() bool {
	xfss3CondMu.Lock()
	defer xfss3CondMu.Unlock()
	return xfss3CondDegraded
}

// probeXFSS3ConditionalWrites asks the store at startup, so a store that
// would overwrite a commit refuses xfs-s3 pauses from the first one on.
func probeXFSS3ConditionalWrites(ctx context.Context) {
	wait := xfss3RetryMin
	for attempt := 0; ; attempt++ {
		c, err := xfss3Client()
		if err != nil {
			return
		}
		if limit := checkpoint.BandwidthLimit(); attempt == 0 && limit > 0 {
			log.G(ctx).Infof("xfs-s3: checkpoint transfers of this node are capped at %d Mbit/s", limit*8/1_000_000)
		}
		err = xfss3CheckConditionalWrites(ctx, c)
		switch {
		case err == nil && xfss3CommitsDegraded():
			log.G(ctx).Warnf("xfs-s3: checkpoint store ignores If-None-Match; conditional_writes=optional, so commits are checked before and after each write instead")
			return
		case err == nil:
			log.G(ctx).Infof("xfs-s3: checkpoint store refuses overwrites of committed objects")
			return
		case errors.Is(err, checkpoint.ErrConditionalWriteUnsupported):
			log.G(ctx).Errorf("xfs-s3: checkpoint store cannot protect committed checkpoints; xfs-s3 pause is refused on this node: %v", err)
			return
		}
		log.G(ctx).Warnf("xfs-s3: cannot ask the checkpoint store about conditional writes yet, retrying in %s: %v", wait, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, xfss3RetryMax)
	}
}

func xfss3Client() (*checkpoint.Client, error) {
	xfss3ClientMu.Lock()
	defer xfss3ClientMu.Unlock()
	if xfss3ClientVal != nil {
		return xfss3ClientVal, nil
	}
	c, err := xfss3NewClient()
	if err != nil {
		return nil, err
	}
	xfss3ClientVal = c
	return c, nil
}

// xfss3Workers is configured unless the environment variable env holds a
// count between 1 and checkpoint.MaxTransferWorkers, which wins: an operator
// can change one node without editing the shared store configuration.
func xfss3Workers(env string, configured int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(env))); err == nil && n > 0 && n <= checkpoint.MaxTransferWorkers {
		return n
	}
	return configured
}

// xfss3ExportWorkers is how many chunks an export moves at once:
// CUBE_XFSS3_EXPORT_WORKERS, else export_workers (default 32).
func xfss3ExportWorkers(c *checkpoint.Client) int {
	cfg := c.Config()
	n, _ := cfg.TransferWorkers()
	return xfss3Workers("CUBE_XFSS3_EXPORT_WORKERS", n)
}

// xfss3ReadWorkers is how many chunks a readback or an import moves at
// once: CUBE_XFSS3_READ_WORKERS, else read_workers (default 32).
func xfss3ReadWorkers(c *checkpoint.Client) int {
	cfg := c.Config()
	_, n := cfg.TransferWorkers()
	return xfss3Workers("CUBE_XFSS3_READ_WORKERS", n)
}

func xfss3Lock(snapshotID string) *sync.Mutex {
	v, _ := xfss3Locks.LoadOrStore(snapshotID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func newXFSS3Session() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102t150405") + "-" + hex.EncodeToString(b[:])
}

func xfss3JobPath(home string) string { return filepath.Join(home, xfss3JobFileName) }

func readXFSS3Job(home string) (*xfss3Job, error) {
	raw, err := os.ReadFile(xfss3JobPath(home)) // NOCC:Path Traversal()
	if err != nil {
		return nil, err
	}
	var job xfss3Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, fmt.Errorf("decode %s: %w", xfss3JobPath(home), err)
	}
	return &job, nil
}

// writeXFSS3Job replaces the job record durably: a Pause that returned
// success must find its job again after a host crash.
func writeXFSS3Job(home string, job *xfss3Job) error {
	raw, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(xfss3JobPath(home), raw, 0o644)
}

func writeFileDurable(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func syncFile(path string) error {
	f, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func xfss3Store() (*XfsCow, error) {
	store, err := requireCowStoreFor(cow.BackendXFS)
	if err != nil {
		return nil, err
	}
	xfs, ok := store.(*XfsCow)
	if !ok || xfs == nil {
		return nil, fmt.Errorf("xfs store is %T, want *XfsCow", store)
	}
	return xfs, nil
}

// xfss3SourceFiles lists what one pause package consists of: its two disks
// and every regular file under its metadata directory.
func xfss3SourceFiles(ctx context.Context, entry *SnapshotCatalogEntry) ([]checkpoint.SourceFile, error) {
	store, err := xfss3Store()
	if err != nil {
		return nil, err
	}
	var files []checkpoint.SourceFile
	for _, obj := range []struct{ name, kind string }{
		{entry.RootfsVol, entry.RootfsKind},
		{entry.MemoryVol, entry.MemoryKind},
	} {
		if strings.TrimSpace(obj.name) == "" {
			return nil, fmt.Errorf("pause package %s has no rootfs or memory object", entry.SnapshotID)
		}
		kind, err := normalizeCowKind(obj.kind)
		if err != nil {
			return nil, err
		}
		path, err := store.ResolveDevPath(ctx, obj.name, kind)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", obj.name, err)
		}
		files = append(files, checkpoint.SourceFile{Role: checkpoint.RoleVolume, Name: obj.name, Path: path})
	}
	meta := strings.TrimSpace(entry.MetaDir)
	if meta == "" {
		meta = filepath.Join(entry.SnapshotPath, SnapshotMetadataDir)
	}
	err = filepath.WalkDir(meta, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(meta, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, checkpoint.SourceFile{
			Role: checkpoint.RoleMeta, Name: filepath.ToSlash(rel), Path: path, Mode: uint32(info.Mode().Perm()),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list metadata of %s: %w", entry.SnapshotID, err)
	}
	return files, nil
}

// sealXFSS3Package makes the package durable before Pause returns. Data the
// VMM wrote is already synced; this covers the rootfs clone, the metadata
// files and every directory entry on the way to the package.
func sealXFSS3Package(files []checkpoint.SourceFile, home string) error {
	dirs := map[string]bool{}
	for _, f := range files {
		if err := syncFile(f.Path); err != nil {
			return fmt.Errorf("sync %s: %w", f.Path, err)
		}
		dirs[filepath.Dir(f.Path)] = true
	}
	dirs[home] = true
	dirs[filepath.Dir(home)] = true
	for dir := range dirs {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("sync dir %s: %w", dir, err)
		}
	}
	return nil
}

type xfss3Export struct {
	cancel   context.CancelFunc
	done     chan struct{}
	progress *checkpoint.Progress
}

// StartXFSS3PauseExport seals the pause package snapshotID and starts its
// background export. It returns the locator Master stores; the checkpoint is
// not remotely usable until a readback on another node says so.
func StartXFSS3PauseExport(ctx context.Context, snapshotID, sandboxID, templateID string) (string, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if err := checkpoint.ValidateID("snapshot id", snapshotID); err != nil {
		return "", err
	}
	entry, err := GetLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID)
	if err != nil {
		return "", err
	}
	files, err := xfss3SourceFiles(ctx, entry)
	if err != nil {
		return "", err
	}
	if err := sealXFSS3Package(files, entry.SnapshotPath); err != nil {
		return "", err
	}
	job := &xfss3Job{
		Role:       xfss3RoleOrigin,
		SnapshotID: snapshotID,
		Session:    newXFSS3Session(),
		State:      xfss3StateExporting,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		SandboxID:  sandboxID,
		TemplateID: templateID,
	}
	if checkpoint.ValidateID("sandbox id", sandboxID) == nil {
		job.RestoredFrom = loadXFSS3RestoredFrom(sandboxID)
		job.Generation = xfss3NextGeneration(job.RestoredFrom, time.Now())
	}
	if err := writeXFSS3Job(entry.SnapshotPath, job); err != nil {
		return "", fmt.Errorf("record checkpoint export of %s: %w", snapshotID, err)
	}
	if job.Generation > 0 {
		// A same-node resume restarts from this pause.
		if err := saveXFSS3RestoredFrom(sandboxID, &xfss3RestoredFrom{SnapshotID: snapshotID,
			Generation: job.Generation, Via: "pause"}); err != nil {
			return "", fmt.Errorf("record that %s resumes from %s: %w", sandboxID, snapshotID, err)
		}
	}
	launchXFSS3Export(ctx, entry, job)
	// The store is not needed to return: the package is sealed and its job
	// recorded, and the export retries until the store is usable. The
	// locator here is provisional; Master replaces it with the one the
	// verifier read back.
	prefix := checkpoint.DefaultPrefix
	if client, err := xfss3Client(); err == nil {
		prefix = client.Prefix()
	}
	loc := &XFSS3Locator{Checkpoint: checkpoint.CandidateKey(prefix, snapshotID)}
	return loc.JSON(), nil
}

func launchXFSS3Export(ctx context.Context, entry *SnapshotCatalogEntry, job *xfss3Job) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	exp := &xfss3Export{cancel: cancel, done: make(chan struct{}), progress: &checkpoint.Progress{}}
	if prev, loaded := xfss3Exports.LoadOrStore(entry.SnapshotID, exp); loaded {
		cancel()
		log.G(ctx).Infof("xfs-s3 export %s already running (%p)", entry.SnapshotID, prev)
		return
	}
	go func() {
		defer close(exp.done)
		defer cancel()
		runXFSS3Export(runCtx, entry, job, exp.progress)
	}()
}

const (
	xfss3RetryMin = 2 * time.Second
	xfss3RetryMax = 5 * time.Minute
)

// xfss3ExportOnce is a test hook.
var xfss3ExportOnce = exportXFSS3Once

// runXFSS3Export retries until the session is committed, the job is retired
// or the package itself is gone. An unreachable store only delays it.
func runXFSS3Export(ctx context.Context, entry *SnapshotCatalogEntry, job *xfss3Job, progress *checkpoint.Progress) {
	logger := log.G(ctx).WithField("snapshot", entry.SnapshotID).WithField("session", job.Session)
	backoff := xfss3RetryMin
	for {
		job.Attempts++
		start := time.Now()
		cand, m, err := xfss3ExportOnce(ctx, entry, job, progress)
		if err == nil {
			job.State = xfss3StateExported
			job.ManifestSHA256 = cand.ManifestSHA256
			job.Error = ""
			job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			job.TransferMS = time.Since(start).Milliseconds()
			if m != nil {
				job.DataBytes = m.DataBytes()
				job.LogicalBytes = m.LogicalBytes()
			}
			if job.sweep != nil {
				job.Sweep = &xfss3SweepRecord{State: xfss3SweepPending}
			}
			if werr := writeXFSS3Job(entry.SnapshotPath, job); werr != nil {
				logger.Warnf("xfs-s3 export committed but job record failed: %v", werr)
			}
			logger.Infof("xfs-s3 export committed: manifest=%s data=%d logical=%d in %s mode=%s base=%s generation=%d stats=%s",
				cand.ManifestSHA256, job.DataBytes, job.LogicalBytes, time.Since(start), job.Mode, job.Base, job.Generation, xfss3StatsJSON(job.Stats))
			if job.sweep != nil {
				job.sweep.release()
				job.sweep = nil
			} else if m == nil && job.SandboxID != "" {
				logger.Infof("xfs-s3 export was committed before a restart; the sweep after it is left to the next export of %s", job.SandboxID)
			}
			return
		}
		if ctx.Err() != nil {
			logger.Infof("xfs-s3 export stopped: %v", ctx.Err())
			return
		}
		job.Error = err.Error()
		if isXFSS3Permanent(err) {
			job.State = xfss3StateFailed
			job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = writeXFSS3Job(entry.SnapshotPath, job)
			logger.Errorf("xfs-s3 export failed for good: %v", err)
			return
		}
		_ = writeXFSS3Job(entry.SnapshotPath, job)
		logger.Warnf("xfs-s3 export attempt %d failed, retrying in %s: %v", job.Attempts, backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, xfss3RetryMax)
	}
}

func xfss3StatsJSON(st *checkpoint.IncrementalStats) string {
	if st == nil {
		return "{}"
	}
	raw, _ := json.Marshal(st)
	return string(raw)
}

type xfss3PermanentError struct{ err error }

func (e *xfss3PermanentError) Error() string { return e.err.Error() }
func (e *xfss3PermanentError) Unwrap() error { return e.err }

func isXFSS3Permanent(err error) bool {
	var p *xfss3PermanentError
	return errors.As(err, &p)
}

func exportXFSS3Once(ctx context.Context, entry *SnapshotCatalogEntry, job *xfss3Job, progress *checkpoint.Progress) (*checkpoint.Candidate, *checkpoint.Manifest, error) {
	client, err := xfss3Client()
	if err != nil {
		return nil, nil, err
	}
	if err := xfss3CheckConditionalWrites(ctx, client); err != nil {
		if errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
			return nil, nil, &xfss3PermanentError{err}
		}
		return nil, nil, fmt.Errorf("ask the store about conditional writes: %w", err)
	}
	cand, err := checkpoint.LoadCandidate(ctx, client, entry.SnapshotID)
	if done, err := xfss3CommitDecision(cand, err, job.Session); done || err != nil {
		return cand, nil, err
	}
	files, err := xfss3SourceFiles(ctx, entry)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, &xfss3PermanentError{err}
		}
		return nil, nil, err
	}
	catalog, err := json.Marshal(entry)
	if err != nil {
		return nil, nil, &xfss3PermanentError{err}
	}
	origin, err := xfss3NodeID()
	if err != nil {
		return nil, nil, err
	}
	progress.TotalBytes.Store(0)
	progress.ScannedBytes.Store(0)
	progress.UploadedBytes.Store(0)
	started, perr := time.Parse(time.RFC3339Nano, job.StartedAt)
	if perr != nil {
		return nil, nil, &xfss3PermanentError{fmt.Errorf("export job of %s has no start time: %w", entry.SnapshotID, perr)}
	}
	attrs := map[string]string{xfss3CatalogAttr: string(catalog), xfss3OriginAttr: origin}
	if client.Config().Incremental && job.SandboxID != "" {
		cand, m, err := exportXFSS3Incremental(ctx, client, entry, job, files, attrs, started, progress)
		if xfss3ExportCannotSucceed(err) {
			return nil, nil, &xfss3PermanentError{err}
		}
		return cand, m, err
	}
	job.Mode = "full"
	cand, m, err := checkpoint.Export(ctx, client, entry.SnapshotID, job.Session, files, checkpoint.ExportOptions{
		Workers:    xfss3ExportWorkers(client),
		Progress:   progress,
		Attributes: attrs,
		// Commit objects carry the job's start time, so a retry of this
		// session rebuilds them byte for byte and the write-once commit
		// accepts it; anything else is a conflict, never an overwrite.
		Now: func() time.Time { return started },
	})
	if xfss3ExportCannotSucceed(err) {
		return nil, nil, &xfss3PermanentError{err}
	}
	return cand, m, err
}

// xfss3ExportCannotSucceed reports an export error that a retry repeats: the
// package holds a file no checkpoint can carry, or another session is
// already committed.
func xfss3ExportCannotSucceed(err error) bool {
	return errors.Is(err, checkpoint.ErrUnexportable) || errors.Is(err, checkpoint.ErrConflict)
}

// xfss3NodeID names this node in a checkpoint: the node id Cubelet
// registers with Master, unique per node where a hostname need not be.
// A test hook.
var xfss3NodeID = func() (string, error) {
	identity, err := utils.GetHostIdentity()
	if err != nil {
		return "", fmt.Errorf("node identity: %w", err)
	}
	if strings.TrimSpace(identity.InstanceID) == "" {
		return "", errors.New("node identity: empty node id")
	}
	return identity.InstanceID, nil
}

// xfss3CommitDecision reads the candidate lookup a (re)started export makes
// before uploading. Only a definite "nothing committed" lets it export: an
// unreadable store may hide a commit that readers have already verified, so
// that case is retried, and a commit of another session is never replaced.
func xfss3CommitDecision(cand *checkpoint.Candidate, err error, session string) (done bool, _ error) {
	switch {
	case err == nil && cand != nil && cand.Session == session:
		return true, nil
	case err == nil && cand != nil:
		return false, &xfss3PermanentError{fmt.Errorf("%w: session %s is already committed, this job owns %s",
			checkpoint.ErrConflict, cand.Session, session)}
	case errors.Is(err, checkpoint.ErrNoCandidate):
		return false, nil
	case err == nil:
		return false, fmt.Errorf("candidate lookup returned nothing")
	case errors.Is(err, checkpoint.ErrCorrupt):
		return false, &xfss3PermanentError{err}
	default:
		return false, fmt.Errorf("look up committed candidate: %w", err)
	}
}

// ResumeXFSS3Exports restarts every export a previous Cubelet left running.
// It waits for the local store, which plugin init may still be bringing up.
func ResumeXFSS3Exports(ctx context.Context) {
	go probeXFSS3ConditionalWrites(ctx)
	var entries []*SnapshotCatalogEntry
	for attempt := 1; ; attempt++ {
		_, err := xfss3Store()
		if err == nil {
			entries, err = ListLocalSnapshotsFor(ctx, cow.BackendXFS)
		}
		if err == nil {
			break
		}
		if attempt == 30 {
			log.G(ctx).Errorf("xfs-s3: cannot resume exports, local store unavailable: %v", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	CleanupXFSS3Trials(ctx)
	RetryXFSS3Retirements(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(xfss3RetireRetryEvery):
			}
			RetryXFSS3Retirements(ctx)
		}
	}()
	for _, entry := range entries {
		resumeXFSS3Entry(ctx, entry)
	}
}

// resumeXFSS3Entry restarts the export of entry if one was running, and
// records a sweep the restart lost.
func resumeXFSS3Entry(ctx context.Context, entry *SnapshotCatalogEntry) {
	if entry == nil || strings.TrimSpace(entry.SnapshotPath) == "" {
		return
	}
	job, err := readXFSS3Job(entry.SnapshotPath)
	if err != nil || job.Role != xfss3RoleOrigin {
		return
	}
	if job.State == xfss3StateExported && job.Sweep != nil && job.Sweep.State == xfss3SweepPending {
		log.G(ctx).Warnf("xfs-s3: the sweep of lineage %s after %s was lost in a restart; the next export of the sandbox sweeps what it left",
			job.SandboxID, entry.SnapshotID)
		_ = updateXFSS3Job(entry.SnapshotPath, job.Session, func(j *xfss3Job) bool {
			if j.Sweep == nil || j.Sweep.State != xfss3SweepPending {
				return false
			}
			j.Sweep.State = xfss3SweepLost
			j.Sweep.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			return true
		})
		return
	}
	if job.State != xfss3StateExporting {
		return
	}
	log.G(ctx).Infof("xfs-s3: resuming export of %s session %s", entry.SnapshotID, job.Session)
	launchXFSS3Export(ctx, entry, job)
}

// XFSS3CheckpointStatus answers Snapshot.Status for an xfs-s3 snapshot. The
// origin reports its export; any other node reports its own readback.
func XFSS3CheckpointStatus(ctx context.Context, snapshotID string) *cow.RemoteStatus {
	snapshotID = strings.TrimSpace(snapshotID)
	st := &cow.RemoteStatus{SnapshotID: snapshotID}
	if err := checkpoint.ValidateID("snapshot id", snapshotID); err != nil {
		st.State = cow.RemoteStateFailed
		st.Message = err.Error()
		return st
	}
	if entry, err := GetLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID); err == nil && entry != nil {
		if job, jerr := readXFSS3Job(entry.SnapshotPath); jerr == nil && job.Role == xfss3RoleOrigin {
			return xfss3OriginStatus(snapshotID, job)
		}
	}
	return xfss3VerifierStatus(ctx, snapshotID)
}

func xfss3OriginStatus(snapshotID string, job *xfss3Job) *cow.RemoteStatus {
	st := &cow.RemoteStatus{SnapshotID: snapshotID}
	switch job.State {
	case xfss3StateExported:
		st.State = cow.RemoteStateExported
		st.Message = fmt.Sprintf("session %s committed (manifest %s); waiting for readback on another node",
			job.Session, job.ManifestSHA256)
	case xfss3StateFailed:
		st.State = cow.RemoteStateFailed
		st.Message = "export failed: " + job.Error
	case xfss3StateRetired:
		st.State = cow.RemoteStateFailed
		st.Message = "checkpoint retired"
	default:
		st.State = cow.RemoteStateRunning
		msg := "exporting session " + job.Session
		if v, ok := xfss3Exports.Load(snapshotID); ok {
			p := v.(*xfss3Export).progress
			msg += fmt.Sprintf(": scanned %d of %d bytes, uploaded %d",
				p.ScannedBytes.Load(), p.TotalBytes.Load(), p.UploadedBytes.Load())
		}
		if job.Error != "" {
			msg += fmt.Sprintf(" (attempt %d, last error: %s)", job.Attempts, job.Error)
		}
		st.Message = msg
	}
	return st
}

type xfss3Verify struct {
	mu        sync.Mutex
	running   bool
	state     string
	message   string
	notBefore time.Time
	failures  int
	// trials counts trials that downloaded the checkpoint and did not pass.
	trials int
	// halted: this node stops trying this checkpoint and keeps reporting
	// message. A restore failure, or too many failed trials, says nothing
	// more when repeated here; another node has to try.
	halted bool
	// rechecking: the last readback found the stored checkpoint corrupt and
	// the next one decides.
	rechecking bool
	progress   *checkpoint.Progress
}

const (
	xfss3VerifyRecheck = 2 * time.Second
	// xfss3MaxTrials bounds the full downloads one node spends on a
	// checkpoint whose trials keep failing (agent, infra, cleanup, ...).
	xfss3MaxTrials = 6
	// xfss3CorruptRecheck is how long a corrupt readback waits before the
	// readback that confirms it, long enough for a lagging listing or read
	// to catch up with the commit.
	xfss3CorruptRecheck = 30 * time.Second
)

func xfss3VerifierStatus(ctx context.Context, snapshotID string) *cow.RemoteStatus {
	v, _ := xfss3Verifies.LoadOrStore(snapshotID, &xfss3Verify{})
	ver := v.(*xfss3Verify)
	ver.mu.Lock()
	defer ver.mu.Unlock()
	st := &cow.RemoteStatus{SnapshotID: snapshotID}
	switch {
	case ver.state == cow.RemoteStateReady || ver.state == cow.RemoteStateFailed:
		st.State, st.Message = ver.state, ver.message
		return st
	case ver.running:
		st.State = cow.RemoteStateRunning
		st.Message = fmt.Sprintf("reading back: %d of %d bytes", ver.progress.ScannedBytes.Load(), ver.progress.TotalBytes.Load())
		return st
	case ver.halted || time.Now().Before(ver.notBefore):
		st.State, st.Message = cow.RemoteStatePending, ver.message
		return st
	}
	ver.running = true
	ver.progress = &checkpoint.Progress{}
	progress := ver.progress
	go func() {
		out := verifyXFSS3(context.WithoutCancel(ctx), snapshotID, progress)
		ver.mu.Lock()
		defer ver.mu.Unlock()
		ver.running = false
		ver.noteVerifyOutcome(out)
	}()
	st.State = cow.RemoteStateRunning
	st.Message = "readback started"
	return st
}

// noteVerifyOutcome records one verification of this node. Called with mu
// held.
func (ver *xfss3Verify) noteVerifyOutcome(out xfss3VerifyOutcome) {
	if out.recheck && !ver.rechecking {
		ver.rechecking = true
		ver.state = cow.RemoteStatePending
		ver.message = "readback found the checkpoint corrupt, checking again: " + out.msg
		ver.notBefore = time.Now().Add(xfss3CorruptRecheck)
		return
	}
	if out.state != cow.RemoteStateFailed {
		ver.rechecking = false
	}
	ver.state, ver.message = out.state, out.msg
	wait := xfss3VerifyRecheck
	if out.backoff {
		ver.failures++
		wait = xfss3TrialBackoff(ver.failures)
	}
	ver.notBefore = time.Now().Add(wait)
	if ver.state != cow.RemoteStatePending || out.trialStage == "" {
		return
	}
	ver.trials++
	switch {
	case out.trialStage == "restore":
		// Whether the package or this node is at fault, another node
		// restoring it is what tells; Master asks one.
		ver.halted = true
	case ver.trials >= xfss3MaxTrials:
		ver.halted = true
		ver.message = trialRetryPrefix("exhausted") +
			fmt.Sprintf("%d trial restores failed on this node; last: %s", ver.trials, out.msg)
	}
}

// xfss3VerifyOutcome is the result of one verification on this node.
type xfss3VerifyOutcome struct {
	state   string
	msg     string
	backoff bool
	// trialStage is set when a full download and trial restore ran and did
	// not pass: the stage it failed at ("unknown" when it did not say).
	trialStage string
	// recheck: a failed verdict read from the store, which stands only once
	// a second readback repeats it.
	recheck bool
}

// verifyXFSS3 reads every object of the committed session back and checks it
// against the manifest. Missing or corrupt data fails the checkpoint for
// good once a second readback finds it again; a store that cannot be reached
// leaves it pending.
func verifyXFSS3(ctx context.Context, snapshotID string, progress *checkpoint.Progress) xfss3VerifyOutcome {
	pending := func(msg string, backoff bool) xfss3VerifyOutcome {
		return xfss3VerifyOutcome{state: cow.RemoteStatePending, msg: msg, backoff: backoff}
	}
	failed := func(msg string) xfss3VerifyOutcome {
		return xfss3VerifyOutcome{state: cow.RemoteStateFailed, msg: msg}
	}
	// corrupt is a failure read from the store: a chunk or the manifest
	// missing, or bytes that do not match. A store whose listings or reads
	// lag a write answers the same way for a while, so the verdict stands
	// only once a later readback repeats it.
	corrupt := func(msg string) xfss3VerifyOutcome {
		out := failed(msg)
		out.recheck = true
		return out
	}
	client, err := xfss3Client()
	if err != nil {
		return pending(err.Error(), true)
	}
	start := time.Now()
	cand, m, err := checkpoint.Load(ctx, client, snapshotID, "")
	switch {
	case errors.Is(err, checkpoint.ErrNoCandidate):
		return pending("no committed session yet", false)
	case errors.Is(err, checkpoint.ErrCorrupt):
		return corrupt(err.Error())
	case err != nil:
		return pending(err.Error(), true)
	}
	entry, err := xfss3CatalogFromManifest(m)
	if err != nil {
		return failed(err.Error())
	}
	node, err := xfss3NodeID()
	if err != nil {
		return pending(trialRetryPrefix("isolation")+"cannot tell whether this node is the origin: "+err.Error(), true)
	}
	if origin := m.Attributes[xfss3OriginAttr]; origin == node {
		return pending(trialRetryPrefix("isolation")+"readback must run on a node other than the origin "+origin, true)
	}
	if client.Config().Verify == checkpoint.VerifyExistence {
		// Upload complete is ready: every chunk was checked against its
		// digest when it was sent, so only its presence is checked here.
		if err := checkpoint.VerifyStored(ctx, client, m); err != nil {
			if errors.Is(err, checkpoint.ErrCorrupt) {
				return corrupt(err.Error())
			}
			return pending(err.Error(), true)
		}
		ev := &checkpoint.Evidence{SnapshotID: snapshotID, Session: cand.Session, ManifestSHA256: cand.ManifestSHA256,
			Verifier: node, DataBytes: m.DataBytes(), LogicalBytes: m.LogicalBytes(),
			VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano), DurationMS: time.Since(start).Milliseconds(),
			TrialRestore: "skipped", Verify: checkpoint.VerifyExistence}
		if err := checkpoint.PutEvidence(ctx, client, ev); err != nil {
			return pending("store readback evidence: "+err.Error(), true)
		}
		loc := &XFSS3Locator{Checkpoint: checkpoint.CandidateKey(client.Prefix(), snapshotID), ManifestSHA256: cand.ManifestSHA256}
		return xfss3VerifyOutcome{state: cow.RemoteStateReady, msg: loc.JSON()}
	}
	trial, verdict, err := trialXFSS3(ctx, client, m, entry, progress)
	if err != nil {
		stage := "unknown"
		if trial != nil && trial.Stage != "" {
			stage = trial.Stage
		}
		log.G(ctx).Warnf("xfs-s3 trial restore of %s session %s: %v", snapshotID, cand.Session, err)
		if verdict == trialDefect {
			if errors.Is(err, checkpoint.ErrCorrupt) {
				return corrupt(err.Error())
			}
			return failed(err.Error())
		}
		if errors.Is(err, errXFSS3TrialDownload) {
			return pending(trialRetryPrefix("download")+err.Error(), true)
		}
		out := pending(trialRetryPrefix(stage)+err.Error(), true)
		out.trialStage = stage
		return out
	}
	if err := xfss3TrialPassed(trial); err != nil {
		out := pending(trialRetryPrefix("cleanup")+err.Error(), true)
		out.trialStage = "cleanup"
		return out
	}
	trialRaw, _ := json.Marshal(trial)
	ev := &checkpoint.Evidence{
		SnapshotID:     snapshotID,
		Session:        cand.Session,
		ManifestSHA256: cand.ManifestSHA256,
		Verifier:       node,
		DataBytes:      m.DataBytes(),
		LogicalBytes:   m.LogicalBytes(),
		Chunks:         progress.Objects.Load(),
		VerifiedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		DurationMS:     time.Since(start).Milliseconds(),
		TrialRestore:   "passed",
		Trial:          trialRaw,
	}
	if err := checkpoint.PutEvidence(ctx, client, ev); err != nil {
		return pending("store readback evidence: "+err.Error(), true)
	}
	log.G(ctx).Infof("xfs-s3 readback and trial restore of %s session %s passed: %d bytes in %s (restore %dms, agent %dms)",
		snapshotID, cand.Session, ev.DataBytes, time.Since(start), trial.RestoreMS, trial.AgentMS)
	loc := &XFSS3Locator{Checkpoint: checkpoint.CandidateKey(client.Prefix(), snapshotID), ManifestSHA256: cand.ManifestSHA256}
	return xfss3VerifyOutcome{state: cow.RemoteStateReady, msg: loc.JSON()}
}

// xfss3CatalogFromManifest recovers the origin catalog entry and checks it
// against the files the manifest actually carries.
func xfss3CatalogFromManifest(m *checkpoint.Manifest) (*SnapshotCatalogEntry, error) {
	raw := m.Attributes[xfss3CatalogAttr]
	if raw == "" {
		return nil, fmt.Errorf("%w: manifest carries no catalog", checkpoint.ErrCorrupt)
	}
	var entry SnapshotCatalogEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return nil, fmt.Errorf("%w: decode catalog: %v", checkpoint.ErrCorrupt, err)
	}
	if entry.SnapshotID != m.SnapshotID {
		return nil, fmt.Errorf("%w: catalog belongs to %s", checkpoint.ErrCorrupt, entry.SnapshotID)
	}
	if entry.Kind != CatalogKindPauseSnapshot {
		return nil, fmt.Errorf("%w: catalog kind %q is not a pause package", checkpoint.ErrCorrupt, entry.Kind)
	}
	// The disks of a pause package are named after it. A checkpoint naming
	// any other object would make this node replace an object it does not
	// own, so the names are checked here, before any trial or import.
	if entry.RootfsVol != cowTemplateRootfsName(m.SnapshotID) || entry.MemoryVol != cowTemplateMemoryName(m.SnapshotID) {
		return nil, fmt.Errorf("%w: package objects %q and %q are not the disks of %s",
			checkpoint.ErrCorrupt, entry.RootfsVol, entry.MemoryVol, m.SnapshotID)
	}
	volumes := 0
	for _, f := range m.Files {
		if f.Role != checkpoint.RoleVolume {
			continue
		}
		volumes++
		if f.Name != entry.RootfsVol && f.Name != entry.MemoryVol {
			return nil, fmt.Errorf("%w: manifest carries foreign object %q", checkpoint.ErrCorrupt, f.Name)
		}
	}
	if volumes != 2 {
		return nil, fmt.Errorf("%w: manifest carries %d disks, want rootfs and memory", checkpoint.ErrCorrupt, volumes)
	}
	for _, name := range []string{snapshotCatalogFileName, "metadata.json", "snapshot/config.json", "snapshot/state.json", "sandbox_spec.json"} {
		if m.FindFile(checkpoint.RoleMeta, name) == nil {
			return nil, fmt.Errorf("%w: manifest lacks restore file %q", checkpoint.ErrCorrupt, name)
		}
	}
	if s := strings.TrimSpace(entry.SpecDir); s != "" && (strings.ContainsAny(s, `/\`) || strings.Contains(s, "..")) {
		return nil, fmt.Errorf("%w: spec dir %q is invalid", checkpoint.ErrCorrupt, s)
	}
	return &entry, nil
}

// MaterializeXFSS3Pause makes snapshotID an ordinary local xfs pause package
// on this node, downloading the verified session in full. It is idempotent:
// a package already in the local catalog is used as is.
func MaterializeXFSS3Pause(ctx context.Context, snapshotID, locatorJSON string) error {
	snapshotID = strings.TrimSpace(snapshotID)
	if err := checkpoint.ValidateID("snapshot id", snapshotID); err != nil {
		return err
	}
	lock := xfss3Lock(snapshotID)
	lock.Lock()
	defer lock.Unlock()
	if entry, err := GetLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID); err == nil && entry != nil {
		return nil
	}
	loc, err := ParseXFSS3Locator(locatorJSON)
	if err != nil {
		return err
	}
	if loc.ManifestSHA256 == "" {
		return fmt.Errorf("checkpoint of %s has no verified manifest digest", snapshotID)
	}
	client, err := xfss3Client()
	if err != nil {
		return err
	}
	if want := checkpoint.CandidateKey(client.Prefix(), snapshotID); loc.Checkpoint != want {
		return fmt.Errorf("checkpoint locator %q does not name %s", loc.Checkpoint, want)
	}
	store, err := xfss3Store()
	if err != nil {
		return err
	}
	importer, ok := store.engine.(cowVolumeImporter)
	if !ok || importer == nil {
		return fmt.Errorf("xfs engine cannot import checkpoint objects")
	}
	start := time.Now()
	stages := map[string]int64{}
	stage := time.Now()
	mark := func(name string) {
		stages[name] = time.Since(stage).Milliseconds()
		stage = time.Now()
	}
	_, m, err := checkpoint.Load(ctx, client, snapshotID, loc.ManifestSHA256)
	if err != nil {
		return fmt.Errorf("load checkpoint %s: %w", snapshotID, err)
	}
	origin, err := xfss3CatalogFromManifest(m)
	if err != nil {
		return err
	}
	if m.SchemaVersion == checkpoint.SchemaVersionIncremental && m.SandboxID != "" {
		// Before the package can be resumed here: from now on the sandbox's
		// baseline on this node is trusted only if it is this checkpoint.
		if err := saveXFSS3RestoredFrom(m.SandboxID, &xfss3RestoredFrom{SnapshotID: snapshotID,
			ManifestSHA256: loc.ManifestSHA256, Generation: m.Generation, Via: "import"}); err != nil {
			return fmt.Errorf("record that %s resumes from %s: %w", m.SandboxID, snapshotID, err)
		}
	}
	mark("load_ms")

	home := SnapshotHome(cow.BackendXFS, SnapshotKindPause, snapshotID)
	kindRoot := filepath.Dir(home)
	if err := os.MkdirAll(kindRoot, 0o755); err != nil {
		return err
	}
	suffix := newXFSS3Session()
	tmpHome := filepath.Join(kindRoot, ".ckpt-"+snapshotID+"-"+suffix)
	staging := filepath.Join(XFSObjectsDir(), xfss3StagingDirName, snapshotID+"-"+suffix)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(staging)
		_ = os.RemoveAll(tmpHome)
	}()
	meta := filepath.Join(tmpHome, SnapshotMetadataDir)
	if err := os.MkdirAll(meta, 0o755); err != nil {
		return err
	}
	dirs := map[string]bool{meta: true, staging: true}
	baseFor, base := xfss3ImportBase(m, client.Config().ImportVerify)
	imported, err := checkpoint.MaterializeFrom(ctx, client, m, xfss3ReadWorkers(client), nil, func(f *checkpoint.File) (string, error) {
		if f.Role == checkpoint.RoleVolume {
			return filepath.Join(staging, f.Name), nil
		}
		if f.Name == snapshotCatalogFileName {
			// This node writes its own catalog once the package is in
			// place; a copy of the origin's would be found by the catalog
			// scan while the package is still being assembled.
			return filepath.Join(staging, "origin-"+snapshotCatalogFileName), nil
		}
		p := filepath.Join(meta, filepath.FromSlash(f.Name))
		dirs[filepath.Dir(p)] = true
		return p, os.MkdirAll(filepath.Dir(p), 0o755)
	}, baseFor)
	if err != nil {
		return fmt.Errorf("download checkpoint %s: %w", snapshotID, err)
	}
	mark("materialize_ms")
	for dir := range dirs {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	downloaded := time.Since(start)
	mark("sync_dirs_ms")

	// The marker is written before the first import and removed after the
	// catalog: an object of this package's name found without it was not
	// put there by an import of this snapshot and is never deleted.
	marker := xfss3ImportMarker(snapshotID)
	_, markErr := os.Stat(marker)
	marked := markErr == nil
	for _, name := range []string{origin.RootfsVol, origin.MemoryVol} {
		present, err := cowObjectPresent(store.engine.GetObjectInfo(name))
		if err != nil {
			return fmt.Errorf("look up %s: %w", name, err)
		}
		replace, err := xfss3ClaimLeftover(name, snapshotID, present, marked)
		if err != nil {
			return err
		}
		if replace {
			if err := store.DeleteByKind(ctx, name, cow.KindVolume); err != nil {
				return fmt.Errorf("drop leftover %s of an earlier import: %w", name, err)
			}
		}
	}
	if err := writeFileDurable(marker, []byte(m.Session), 0o600); err != nil {
		return fmt.Errorf("record import of %s: %w", snapshotID, err)
	}
	mark("claim_ms")
	for _, name := range []string{origin.RootfsVol, origin.MemoryVol} {
		if _, err := importer.ImportLvol(name, filepath.Join(staging, name)); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}
		mark("import_" + map[bool]string{true: "memory", false: "rootfs"}[name == origin.MemoryVol] + "_ms")
	}
	if err := os.RemoveAll(home); err != nil {
		return err
	}
	if err := os.Rename(tmpHome, home); err != nil {
		return fmt.Errorf("place package %s: %w", snapshotID, err)
	}
	if err := syncDir(kindRoot); err != nil {
		return err
	}
	if err := EnsureShimSpecDirLink(home, origin.SpecDir); err != nil {
		return err
	}
	mark("place_ms")
	local := *origin
	local.SnapshotPath = home
	local.MetaDir = filepath.Join(home, SnapshotMetadataDir)
	local.RootfsKind = cow.KindVolume
	local.MemoryKind = cow.KindVolume
	local.MetadataVol = ""
	local.MetadataKind = ""
	local.BuildRootfsVol = ""
	local.BuildRootfsKind = ""
	local.Backend = cow.BackendXFS
	local.CreatedAt = ""
	if err := writeXFSS3Job(home, &xfss3Job{
		Role:           xfss3RoleMaterialized,
		SnapshotID:     snapshotID,
		Session:        m.Session,
		State:          xfss3StateExported,
		ManifestSHA256: loc.ManifestSHA256,
		FinishedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		DataBytes:      m.DataBytes(),
		LogicalBytes:   m.LogicalBytes(),
		TransferMS:     downloaded.Milliseconds(),
		SandboxID:      m.SandboxID,
		Base:           base,
		Import:         imported,
	}); err != nil {
		return err
	}
	if err := WriteSnapshotCatalogFor(cow.BackendXFS, &local); err != nil {
		return fmt.Errorf("write catalog of %s: %w", snapshotID, err)
	}
	if err := syncDir(local.MetaDir); err != nil {
		return err
	}
	_ = os.Remove(marker)
	mark("record_ms")
	saveImportedXFSS3Baseline(ctx, store, origin, m)
	mark("baseline_ms")
	stages["total_ms"] = time.Since(start).Milliseconds()
	log.G(ctx).Infof("xfs-s3 materialized %s session %s: %d data bytes (%d logical) in %s, total %s, base=%s reused=%d (trusted %d) fetched=%d repaired=%d",
		snapshotID, m.Session, m.DataBytes(), m.LogicalBytes(), downloaded, time.Since(start),
		base, imported.ReusedBytes, imported.TrustedBytes, imported.FetchedBytes, imported.RepairedBytes)
	if raw, err := json.Marshal(map[string]any{"stages": stages, "materialize": imported.Timing}); err == nil {
		log.G(ctx).Infof("xfs-s3 import stages %s: %s", snapshotID, raw)
	}
	return nil
}

func xfss3ImportMarker(snapshotID string) string {
	return filepath.Join(XFSObjectsDir(), xfss3StagingDirName, snapshotID+".import")
}

// xfss3ClaimLeftover decides about an object named after the package being
// imported. Nothing there: import. Something there and this node's import
// marker for the snapshot: an earlier import died before its catalog, so
// the object is replaced. Something there without the marker: it is not
// ours to delete.
func xfss3ClaimLeftover(name, snapshotID string, present, marked bool) (replace bool, _ error) {
	switch {
	case !present:
		return false, nil
	case marked:
		return true, nil
	default:
		return false, fmt.Errorf("object %s exists but no import of %s left it; refusing to replace it", name, snapshotID)
	}
}

// RetireXFSS3Checkpoint stops the export of snapshotID and deletes its
// objects. Callers retire a checkpoint once nothing can resume from it any
// more: the pause binding is consumed or the sandbox is gone.
func RetireXFSS3Checkpoint(ctx context.Context, snapshotID string) error {
	snapshotID = strings.TrimSpace(snapshotID)
	if err := checkpoint.ValidateID("snapshot id", snapshotID); err != nil {
		return err
	}
	// The intent outlives the package: once the caller drops the local
	// package and the binding, it is the only record that remote objects
	// are left to delete.
	if err := writeXFSS3RetireIntent(snapshotID); err != nil {
		return fmt.Errorf("record retirement of %s: %w", snapshotID, err)
	}
	if v, ok := xfss3Exports.Load(snapshotID); ok {
		exp := v.(*xfss3Export)
		exp.cancel()
		select {
		case <-exp.done:
		case <-time.After(30 * time.Second):
			return fmt.Errorf("export of %s did not stop", snapshotID)
		case <-ctx.Done():
			return ctx.Err()
		}
		xfss3Exports.Delete(snapshotID)
	}
	if entry, err := GetLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID); err == nil && entry != nil {
		// The sweep after the checkpoint may still be running; it is not
		// stopped, what it deletes is superseded either way.
		_ = updateXFSS3Job(entry.SnapshotPath, "", func(job *xfss3Job) bool {
			if job.Role != xfss3RoleOrigin {
				return false
			}
			job.State = xfss3StateRetired
			return true
		})
	}
	xfss3Verifies.Delete(snapshotID)
	return deleteRetiredXFSS3(ctx, snapshotID)
}

// xfss3RetireDir holds one intent file per checkpoint whose remote objects
// still have to go. A test hook.
var xfss3RetireDir = func() string { return filepath.Join(XFSObjectsDir(), "checkpoint-retire") }

func writeXFSS3RetireIntent(snapshotID string) error {
	dir := xfss3RetireDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileDurable(filepath.Join(dir, snapshotID), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600)
}

// deleteRetiredXFSS3 deletes the remote objects of snapshotID and only then
// drops its intent, so an interrupted or partial delete is retried.
func deleteRetiredXFSS3(ctx context.Context, snapshotID string) error {
	client, err := xfss3Client()
	if err != nil {
		return err
	}
	// Paced like a lineage sweep: a checkpoint is thousands of objects, and
	// the exports and imports running beside it share the store's quota.
	cfg := client.Config()
	n, err := client.DeletePrefix(ctx, checkpoint.SnapshotPrefix(client.Prefix(), snapshotID), checkpoint.SweepOptions{
		Workers: xfss3Workers("CUBE_XFSS3_SWEEP_WORKERS", 4), RequestsPerSecond: cfg.SweepRate()})
	if err != nil {
		return fmt.Errorf("delete checkpoint objects of %s (will retry): %w", snapshotID, err)
	}
	if err := os.Remove(filepath.Join(xfss3RetireDir(), snapshotID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	log.G(ctx).Infof("xfs-s3 retired %s: deleted %d objects", snapshotID, n)
	return nil
}

const xfss3RetireRetryEvery = 5 * time.Minute

// RetryXFSS3Retirements finishes every retirement an earlier delete left
// behind (store unreachable, Cubelet restarted mid-delete).
func RetryXFSS3Retirements(ctx context.Context) {
	entries, err := os.ReadDir(xfss3RetireDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		id := e.Name()
		if e.IsDir() || strings.HasSuffix(id, ".tmp") || checkpoint.ValidateID("snapshot id", id) != nil {
			continue
		}
		if err := deleteRetiredXFSS3(ctx, id); err != nil {
			log.G(ctx).Warnf("xfs-s3: retirement of %s still pending: %v", id, err)
		}
	}
}

// IsXFSS3Package reports whether the local package snapshotID is part of an
// xfs-s3 checkpoint, as its origin or as a downloaded copy.
func IsXFSS3Package(ctx context.Context, snapshotID string) bool {
	entry, err := GetLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID)
	if err != nil || entry == nil {
		return false
	}
	_, err = os.Stat(xfss3JobPath(entry.SnapshotPath))
	return err == nil
}
