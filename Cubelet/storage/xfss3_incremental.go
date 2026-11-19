// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// Incremental xfs-s3 checkpoints. A sandbox's checkpoints share the
// chunks of its lineage in the store, and its first one may reuse its
// template's memory. To find what changed without reading everything, the
// node keeps a baseline per sandbox: reflink copies of the files it last
// exported or imported, and the manifest that describes them. A package is
// cloned from the files it was restored from, so FIEMAP tells which windows
// still sit on the baseline's blocks.
//
// Lineage chunks are reused only through the baseline, and only when the
// baseline is the checkpoint the instance was restored from: the sweep
// after that checkpoint spares exactly the chunks it names (see
// checkpoint.LineageGenerationPrefix). A stale baseline, say one an import
// failed to replace, may name chunks a later sweep already deleted.

const (
	xfss3BaseDirName      = "checkpoint-base"
	xfss3BaseManifestFile = "manifest.json"
	xfss3BaseRecordFile   = "base.json"
)

// xfss3BaseRoot holds one baseline directory per sandbox. A test hook.
var xfss3BaseRoot = func() string { return filepath.Join(XFSObjectsDir(), xfss3BaseDirName) }

type xfss3Baseline struct {
	Dir            string
	SnapshotID     string
	Manifest       *checkpoint.Manifest
	ManifestSHA256 string
	// Extents is where each baseline file (by kind) sat on disk when saved.
	Extents map[string][]checkpoint.Extent
}

type xfss3BaseRecord struct {
	SnapshotID     string                         `json:"snapshot_id"`
	ManifestSHA256 string                         `json:"manifest_sha256"`
	SavedAt        string                         `json:"saved_at"`
	Extents        map[string][]checkpoint.Extent `json:"extents,omitempty"`
}

// loadXFSS3Baseline returns the sandbox's baseline, or nil when it has none
// this node can trust.
func loadXFSS3Baseline(sandboxID string) *xfss3Baseline {
	if checkpoint.ValidateID("sandbox id", sandboxID) != nil {
		return nil
	}
	dir := filepath.Join(xfss3BaseRoot(), sandboxID)
	raw, err := os.ReadFile(filepath.Join(dir, xfss3BaseManifestFile)) // NOCC:Path Traversal()
	if err != nil {
		return nil
	}
	recRaw, err := os.ReadFile(filepath.Join(dir, xfss3BaseRecordFile)) // NOCC:Path Traversal()
	if err != nil {
		return nil
	}
	var rec xfss3BaseRecord
	if json.Unmarshal(recRaw, &rec) != nil {
		return nil
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != rec.ManifestSHA256 {
		return nil
	}
	var m checkpoint.Manifest
	if json.Unmarshal(raw, &m) != nil || m.SandboxID != sandboxID {
		return nil
	}
	return &xfss3Baseline{Dir: dir, SnapshotID: rec.SnapshotID, Manifest: &m, ManifestSHA256: rec.ManifestSHA256, Extents: rec.Extents}
}

// xfss3RestoredFrom names the checkpoint the sandbox's instance on this node
// was last restored from. The node records it when it pauses the sandbox (a
// same-node resume restarts from that pause) and when it imports one of the
// sandbox's checkpoints (a cross-node resume), each time before the
// checkpoint can be resumed here.
type xfss3RestoredFrom struct {
	SnapshotID string `json:"snapshot_id"`
	// ManifestSHA256 is known for an import; a pause records it before its
	// export commits.
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	Generation     int64  `json:"generation,omitempty"`
	Via            string `json:"via"`
	RecordedAt     string `json:"recorded_at"`
}

const xfss3RestoredSuffix = ".restored.json"

func xfss3RestoredPath(sandboxID string) string {
	return filepath.Join(xfss3BaseRoot(), sandboxID+xfss3RestoredSuffix)
}

// loadXFSS3RestoredFrom returns what saveXFSS3RestoredFrom last recorded
// for the sandbox, or nil.
func loadXFSS3RestoredFrom(sandboxID string) *xfss3RestoredFrom {
	if checkpoint.ValidateID("sandbox id", sandboxID) != nil {
		return nil
	}
	raw, err := os.ReadFile(xfss3RestoredPath(sandboxID)) // NOCC:Path Traversal()
	if err != nil {
		return nil
	}
	var rf xfss3RestoredFrom
	if json.Unmarshal(raw, &rf) != nil || checkpoint.ValidateID("snapshot id", rf.SnapshotID) != nil {
		return nil
	}
	return &rf
}

func saveXFSS3RestoredFrom(sandboxID string, rf *xfss3RestoredFrom) error {
	if err := checkpoint.ValidateID("sandbox id", sandboxID); err != nil {
		return err
	}
	if err := os.MkdirAll(xfss3BaseRoot(), 0o700); err != nil {
		return err
	}
	rf.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(rf)
	if err != nil {
		return err
	}
	return writeFileDurable(xfss3RestoredPath(sandboxID), raw, 0o600)
}

// xfss3NextGeneration is the lineage generation of an export of a sandbox
// restored from rf: the clock, and always above rf's generation whatever
// the clocks of the two nodes say.
func xfss3NextGeneration(rf *xfss3RestoredFrom, now time.Time) int64 {
	gen := now.UnixNano()
	if rf != nil && rf.Generation >= gen {
		gen = rf.Generation + 1
	}
	return gen
}

// xfss3BaselineUsable says whether an export of an instance restored from
// rf may reuse the lineage chunks of baseline bl, and if not, why. No
// reason means the sandbox has no checkpoint yet.
func xfss3BaselineUsable(bl *xfss3Baseline, rf *xfss3RestoredFrom) (bool, string) {
	switch {
	case bl == nil && rf == nil:
		return false, ""
	case bl == nil:
		return false, fmt.Sprintf("no baseline of %s, the checkpoint the instance was restored from", rf.SnapshotID)
	case rf == nil:
		return false, fmt.Sprintf("baseline %s, but no record of the checkpoint the instance was restored from", bl.SnapshotID)
	case bl.SnapshotID != rf.SnapshotID:
		return false, fmt.Sprintf("baseline %s is stale: the instance was restored from %s", bl.SnapshotID, rf.SnapshotID)
	case rf.ManifestSHA256 != "" && bl.ManifestSHA256 != rf.ManifestSHA256:
		return false, fmt.Sprintf("baseline manifest %s of %s is not %s, the one the instance was restored from",
			bl.ManifestSHA256, bl.SnapshotID, rf.ManifestSHA256)
	case bl.Manifest.Generation <= 0:
		return false, fmt.Sprintf("baseline %s is from before lineage generations", bl.SnapshotID)
	}
	return true, ""
}

var xfss3SandboxLocks sync.Map // sandbox id -> *sync.Mutex

// lockXFSS3Sandbox serializes the exports of one sandbox on this node, so an
// export starts from the baseline the one before it saved.
func lockXFSS3Sandbox(sandboxID string) func() {
	v, _ := xfss3SandboxLocks.LoadOrStore(sandboxID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// saveXFSS3Baseline makes files (kind -> path) and m the sandbox's baseline,
// replacing the previous one. The copies share every block with the files.
func saveXFSS3Baseline(sandboxID, snapshotID string, m *checkpoint.Manifest, files map[string]string) error {
	if err := checkpoint.ValidateID("sandbox id", sandboxID); err != nil {
		return err
	}
	root := xfss3BaseRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root, "."+sandboxID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	extents := map[string][]checkpoint.Extent{}
	for kind, src := range files {
		dst := filepath.Join(tmp, kind)
		if err := reflinkFile(src, dst); err != nil {
			return fmt.Errorf("baseline %s of %s: %w", kind, sandboxID, err)
		}
		// Where the copy sits now, so an import can tell the baseline has
		// not moved since; without it every chunk is read back.
		if ext, err := checkpoint.ExtentMap(dst); err == nil {
			extents[kind] = ext
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	rec, _ := json.Marshal(&xfss3BaseRecord{SnapshotID: snapshotID, ManifestSHA256: hex.EncodeToString(sum[:]),
		SavedAt: time.Now().UTC().Format(time.RFC3339Nano), Extents: extents})
	if err := writeFileDurable(filepath.Join(tmp, xfss3BaseManifestFile), raw, 0o600); err != nil {
		return err
	}
	if err := writeFileDurable(filepath.Join(tmp, xfss3BaseRecordFile), rec, 0o600); err != nil {
		return err
	}
	dir := filepath.Join(root, sandboxID)
	old := dir + ".old"
	_ = os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	_ = os.RemoveAll(old)
	return syncDir(root)
}

func reflinkFile(src, dst string) error {
	s, err := os.Open(src) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer d.Close()
	if err := unix.IoctlFileClone(int(d.Fd()), int(s.Fd())); err != nil {
		return fmt.Errorf("reflink %s: %w", src, err)
	}
	return d.Sync()
}

var xfss3TemplateLocks sync.Map // template id -> *sync.Mutex

// xfss3TemplateBase returns the store's base of templateID and the local
// file of the template's memory, uploading the base once if no node has.
// Nil when this node does not hold the template or the base cannot be used.
func xfss3TemplateBase(ctx context.Context, client *checkpoint.Client, templateID string, chunkSize int64) (*checkpoint.TemplateBase, string) {
	if checkpoint.ValidateID("template id", templateID) != nil {
		return nil, ""
	}
	entry, err := GetLocalSnapshotFor(ctx, cow.BackendXFS, templateID)
	if err != nil || entry == nil || entry.MemoryVol == "" {
		return nil, ""
	}
	store, err := xfss3Store()
	if err != nil {
		return nil, ""
	}
	kind, err := normalizeCowKind(entry.MemoryKind)
	if err != nil {
		return nil, ""
	}
	path, err := store.ResolveDevPath(ctx, entry.MemoryVol, kind)
	if err != nil {
		return nil, ""
	}
	v, _ := xfss3TemplateLocks.LoadOrStore(templateID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	base, err := checkpoint.LoadTemplateBase(ctx, client, templateID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		start := time.Now()
		base, err = checkpoint.ExportTemplateBase(ctx, client, templateID, path, chunkSize, xfss3ExportWorkers(client))
		if err == nil {
			log.G(ctx).Infof("xfs-s3 uploaded the memory base of template %s: %d chunks in %s", templateID, len(base.Chunks), time.Since(start))
		}
	}
	if err != nil {
		log.G(ctx).Warnf("xfs-s3 template %s base unavailable, exporting without it: %v", templateID, err)
		return nil, ""
	}
	if base.ChunkSize != chunkSize {
		return nil, ""
	}
	return base, path
}

// exportXFSS3Incremental exports the package against the best base this
// node has: the sandbox's own baseline when it is the checkpoint the
// instance was restored from, else its template's memory, else none (every
// window is read and uploaded). It leaves the sweep of what the checkpoint
// supersedes registered on job, to start once the job is recorded exported.
func exportXFSS3Incremental(ctx context.Context, client *checkpoint.Client, entry *SnapshotCatalogEntry, job *xfss3Job,
	files []checkpoint.SourceFile, attrs map[string]string, started time.Time, progress *checkpoint.Progress) (*checkpoint.Candidate, *checkpoint.Manifest, error) {
	cfg := client.Config()
	rootfsChunk, memoryChunk := cfg.ChunkSizes()
	if err := checkpoint.ValidateID("sandbox id", job.SandboxID); err != nil {
		return nil, nil, &xfss3PermanentError{err}
	}
	unlock := lockXFSS3Sandbox(job.SandboxID)
	defer unlock()
	// Only to keep the two off the store's request quota at once: a sweep
	// is safe beside any later export.
	stopXFSS3Sweep(ctx, job.SandboxID, entry.SnapshotID)
	if job.Generation <= 0 {
		// A job recorded before lineage generations.
		job.Generation = xfss3NextGeneration(job.RestoredFrom, started)
	}
	var vols []checkpoint.IncrementalVolume
	var meta []checkpoint.SourceFile
	paths := map[string]string{}
	for _, f := range files {
		switch {
		case f.Role == checkpoint.RoleVolume && f.Name == entry.RootfsVol:
			vols = append(vols, checkpoint.IncrementalVolume{Kind: checkpoint.KindRootfs, Name: f.Name, Path: f.Path, ChunkSize: rootfsChunk})
			paths[checkpoint.KindRootfs] = f.Path
		case f.Role == checkpoint.RoleVolume && f.Name == entry.MemoryVol:
			vols = append(vols, checkpoint.IncrementalVolume{Kind: checkpoint.KindMemory, Name: f.Name, Path: f.Path, ChunkSize: memoryChunk})
			paths[checkpoint.KindMemory] = f.Path
		case f.Role == checkpoint.RoleMeta:
			meta = append(meta, f)
		default:
			return nil, nil, fmt.Errorf("unexpected file %s in package %s", f.Name, entry.SnapshotID)
		}
	}
	opt := checkpoint.IncrementalOptions{SandboxID: job.SandboxID, Generation: job.Generation,
		Workers: xfss3ExportWorkers(client), Progress: progress, Attributes: attrs,
		Now: func() time.Time { return started }}
	job.Base = "none"
	known := map[string]string{}
	bl := loadXFSS3Baseline(job.SandboxID)
	usable, why := xfss3BaselineUsable(bl, job.RestoredFrom)
	if why != "" {
		log.G(ctx).Warnf("xfs-s3 export of %s reuses no lineage chunks: %s", entry.SnapshotID, why)
	}
	if usable {
		opt.BaseManifestSHA256 = bl.ManifestSHA256
		if bl.Manifest.TemplateID != "" {
			opt.TemplateID = bl.Manifest.TemplateID
		}
		for i := range vols {
			for _, bf := range bl.Manifest.Files {
				if bf.Role == checkpoint.RoleVolume && bf.Kind == vols[i].Kind && bf.ChunkSize == vols[i].ChunkSize {
					vols[i].BaselinePath = filepath.Join(bl.Dir, vols[i].Kind)
					vols[i].BaseChunks = bf.Chunks
					job.Base = "lineage"
				}
			}
		}
		known = bl.Manifest.ReusableChunks(client.Prefix())
	}
	if job.Base == "none" && job.TemplateID != "" {
		if base, path := xfss3TemplateBase(ctx, client, job.TemplateID, memoryChunk); base != nil {
			opt.TemplateID = job.TemplateID
			for i := range vols {
				if vols[i].Kind == checkpoint.KindMemory {
					vols[i].BaselinePath = path
					vols[i].BaseChunks = base.Chunks
					job.Base = "template"
				}
			}
		}
	}
	if opt.TemplateID != "" {
		// The template base is listed, not the lineage: no sweep ever
		// deletes a template chunk, so its keys win.
		tpl, err := checkpoint.KnownChunks(ctx, client, checkpoint.TemplatePrefix(client.Prefix(), opt.TemplateID)+"c/")
		if err != nil {
			return nil, nil, err
		}
		for sum, key := range tpl {
			known[sum] = key
		}
	}
	opt.Known = known
	cand, m, stats, err := checkpoint.ExportIncremental(ctx, client, entry.SnapshotID, job.Session, vols, meta, opt)
	if errors.Is(err, checkpoint.ErrBaselineMismatch) {
		log.G(ctx).Warnf("xfs-s3 export of %s: baseline does not match, reading every window: %v", entry.SnapshotID, err)
		for i := range vols {
			vols[i].BaselinePath, vols[i].BaseChunks = "", nil
		}
		job.Base = "none"
		cand, m, stats, err = checkpoint.ExportIncremental(ctx, client, entry.SnapshotID, job.Session, vols, meta, opt)
	}
	if err != nil {
		return nil, nil, err
	}
	job.Mode = "incremental"
	job.Stats = stats
	if err := saveXFSS3Baseline(job.SandboxID, entry.SnapshotID, m, paths); err != nil {
		log.G(ctx).Warnf("xfs-s3 export of %s committed but the next one will have no baseline: %v", entry.SnapshotID, err)
	}
	job.sweep = newXFSS3Sweep(ctx, client, entry.SnapshotPath, job.Session, entry.SnapshotID, m)
	return cand, m, nil
}

// xfss3ImportBase returns, for a manifest of an incremental checkpoint, the
// local base of each of its volumes: this node's baseline of the sandbox,
// when it has a file of the same kind and chunk size. Materializing checks
// every chunk it takes from the base against m, so a stale or damaged
// baseline costs a download, never a wrong package.
//
// With import_verify = fiemap, a chunk is taken without a readback when the
// clone shares its blocks with a baseline that still sits where it sat when
// saved: the trust an export already gives a window that shares blocks with
// the baseline.
func xfss3ImportBase(m *checkpoint.Manifest, importVerify string) (func(f *checkpoint.File) *checkpoint.LocalBase, string) {
	if m.SchemaVersion != checkpoint.SchemaVersionIncremental || m.SandboxID == "" {
		return nil, "none"
	}
	bl := loadXFSS3Baseline(m.SandboxID)
	if bl == nil {
		return nil, "none"
	}
	trust := importVerify == checkpoint.ImportVerifyFIEMAP
	return func(f *checkpoint.File) *checkpoint.LocalBase {
		for _, bf := range bl.Manifest.Files {
			if bf.Role == checkpoint.RoleVolume && bf.Kind != "" && bf.Kind == f.Kind && bf.ChunkSize == f.ChunkSize {
				return &checkpoint.LocalBase{Path: filepath.Join(bl.Dir, bf.Kind), Chunks: bf.Chunks,
					Extents: bl.Extents[bf.Kind], TrustShared: trust}
			}
		}
		return nil
	}, "lineage"
}

// saveImportedXFSS3Baseline makes a cross-node import the sandbox's baseline
// on this node, so its next pause here is incremental too.
func saveImportedXFSS3Baseline(ctx context.Context, store *XfsCow, origin *SnapshotCatalogEntry, m *checkpoint.Manifest) {
	if m.SchemaVersion != checkpoint.SchemaVersionIncremental || m.SandboxID == "" {
		return
	}
	files := map[string]string{}
	for kind, name := range map[string]string{checkpoint.KindRootfs: origin.RootfsVol, checkpoint.KindMemory: origin.MemoryVol} {
		path, err := store.ResolveDevPath(ctx, name, cow.KindVolume)
		if err != nil {
			log.G(ctx).Warnf("xfs-s3 baseline of imported %s: %v", origin.SnapshotID, err)
			return
		}
		files[kind] = path
	}
	if err := saveXFSS3Baseline(m.SandboxID, origin.SnapshotID, m, files); err != nil {
		log.G(ctx).Warnf("xfs-s3 baseline of imported %s: %v", origin.SnapshotID, err)
	}
}
