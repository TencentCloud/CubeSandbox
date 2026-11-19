// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// An incremental export cuts every volume into fixed windows of its own
// chunk size and names each non-zero window by its digest. A window the
// store already holds, in the sandbox's lineage or its template's base, is
// referenced instead of uploaded, so a checkpoint only sends what changed
// since the one before it. Every manifest still lists every chunk: a
// restore reads one manifest and never walks a chain.
//
// Windows are fixed, not cut at extent boundaries, so the same bytes at the
// same offset always produce the same digest whatever the file's layout on
// disk: that is what lets a sandbox reuse its template's memory.

// ErrBaselineMismatch means a file no longer matches the baseline it was
// described against; the caller exports again without that baseline.
var ErrBaselineMismatch = errors.New("file does not match its baseline")

// IncrementalVolume is one volume of an incremental export.
type IncrementalVolume struct {
	Kind      string
	Name      string
	Path      string
	ChunkSize int64
	// BaselinePath, when set, is a local file Path was reflink-derived from,
	// and BaseChunks are the chunks a committed manifest recorded for it,
	// cut with the same ChunkSize. A window whose blocks are still the
	// baseline's is described by its base chunk without being read.
	BaselinePath string
	BaseChunks   []Chunk
}

// IncrementalOptions tunes one incremental export.
type IncrementalOptions struct {
	SandboxID          string
	TemplateID         string
	BaseManifestSHA256 string
	// Generation is the lineage generation new chunks are uploaded into. It
	// must be greater than the generation of the checkpoint the sandbox was
	// restored from; zero takes the clock.
	Generation int64
	// Known maps a digest to a key that holds it in the store. Exports add
	// what they upload. Callers fill it from the template base and from the
	// checkpoint the sandbox was restored from (Manifest.ReusableChunks),
	// never from a listing of the lineage: only chunks that checkpoint
	// names are safe from the sweeps that may still run after it. Keys the
	// export may not name (older layouts, this generation or a newer one,
	// another template) are ignored.
	Known      map[string]string
	Workers    int
	Progress   *Progress
	Attributes map[string]string
	Now        func() time.Time
}

// IncrementalStats says where the bytes of an incremental export came from.
type IncrementalStats struct {
	DataBytes       int64        `json:"data_bytes"`
	ReusedBytes     int64        `json:"reused_bytes"`
	ReadBytes       int64        `json:"read_bytes"`
	DedupedBytes    int64        `json:"deduped_bytes"`
	UploadedBytes   int64        `json:"uploaded_bytes"`
	UploadedObjects int64        `json:"uploaded_objects"`
	RepairedObjects int64        `json:"repaired_objects"`
	Chunks          int64        `json:"chunks"`
	Timing          ExportTiming `json:"timing"`
}

// ExportTiming says where an incremental export spent its time. Wall times
// are elapsed; ReadMS, HashMS and UploadMS are summed over the workers, so
// they show what the workers were busy with rather than add up.
type ExportTiming struct {
	// Plan maps each volume's extents and compares it with its baseline.
	PlanMS int64 `json:"plan_ms"`
	// Transfer is the worker pool: reads, digests, uploads.
	TransferMS int64 `json:"transfer_ms"`
	Workers    int   `json:"workers"`
	ReadMS     int64 `json:"read_ms"`
	HashMS     int64 `json:"hash_ms"`
	// UploadMS includes the bandwidth limit's waits and the retries' backoff.
	UploadMS int64 `json:"upload_ms"`
	// Ensure checks that the chunks the manifest names are in the store.
	EnsureMS int64 `json:"ensure_ms"`
	// Commit writes the manifest and the candidate.
	CommitMS          int64 `json:"commit_ms"`
	BandwidthWaitMS   int64 `json:"bandwidth_wait_ms"`
	Retries           int64 `json:"retries"`
	Throttled429      int64 `json:"throttled_429"`
	Throttled503      int64 `json:"throttled_503"`
	ThrottleBackoffMS int64 `json:"throttle_backoff_ms"`
}

type windowJob struct {
	file   int
	offset int64
	length int64
}

type knownKeys struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *knownKeys) get(sum string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, ok := k.m[sum]
	return key, ok
}

// claim returns the key holding sum and whether the caller must upload it
// there; a digest is claimed for upload once.
func (k *knownKeys) claim(sum, key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if have, ok := k.m[sum]; ok {
		return have, false
	}
	k.m[sum] = key
	return key, true
}

// ExportIncremental uploads what the store does not hold yet of vols and
// meta as session of snapshotID, and commits it like Export: chunks, then
// the manifest, then the candidate. New volume chunks go under the lineage
// generation of the export. Before committing it checks that every chunk
// the manifest names is in the store and uploads any that is not again,
// from the local files and into its own generation: a checkpoint is never
// committed short.
func ExportIncremental(ctx context.Context, c *Client, snapshotID, session string, vols []IncrementalVolume,
	meta []SourceFile, opt IncrementalOptions) (*Candidate, *Manifest, *IncrementalStats, error) {
	for kind, id := range map[string]string{"snapshot id": snapshotID, "session": session, "sandbox id": opt.SandboxID} {
		if err := ValidateID(kind, id); err != nil {
			return nil, nil, nil, err
		}
	}
	if opt.TemplateID != "" {
		if err := ValidateID("template id", opt.TemplateID); err != nil {
			return nil, nil, nil, err
		}
	}
	if opt.Workers <= 0 {
		opt.Workers = 4
	}
	if opt.Progress == nil {
		opt.Progress = &Progress{}
	}
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	prefix := c.Prefix()
	gen := opt.Generation
	if gen <= 0 {
		gen = now().UnixNano()
	}
	lineage := LineagePrefix(prefix, opt.SandboxID)
	tpl := ""
	if opt.TemplateID != "" {
		tpl = TemplatePrefix(prefix, opt.TemplateID) + "c/"
	}
	// reusable says whether the export may name key for sum without
	// uploading it: a chunk of its template's base, or of an older lineage
	// generation.
	reusable := func(key, sum string) bool {
		if tpl != "" && key == tpl+sum {
			return true
		}
		g, s, ok := parseLineageKey(lineage, key)
		return ok && s == sum && g > 0 && g < gen
	}
	known := &knownKeys{m: map[string]string{}}
	for sum, key := range opt.Known {
		if reusable(key, sum) {
			known.m[sum] = key
		}
	}
	stats := &IncrementalStats{}
	tm := &stats.Timing
	tm.Workers = opt.Workers
	// The export's own requests are counted apart from the sweeps and
	// imports that may run beside it.
	ctx, hooks := withRequestHooks(ctx, 0)
	waited0 := BandwidthWaited()
	// Counters are read into the stats on every return, failed ones too.
	defer func() {
		tm.BandwidthWaitMS = (BandwidthWaited() - waited0).Milliseconds()
		tm.Retries = hooks.retries.Load()
		th := hooks.throttles()
		tm.Throttled429, tm.Throttled503, tm.ThrottleBackoffMS = th.Status429, th.Status503, th.Backoff.Milliseconds()
	}()
	planStart := time.Now()
	m := &Manifest{
		SchemaVersion:      SchemaVersionIncremental,
		SnapshotID:         snapshotID,
		Session:            session,
		CreatedAt:          now().UTC().Format(time.RFC3339Nano),
		ChunkSize:          DefaultChunkSize,
		Attributes:         opt.Attributes,
		SandboxID:          opt.SandboxID,
		TemplateID:         opt.TemplateID,
		BaseManifestSHA256: opt.BaseManifestSHA256,
		Generation:         gen,
	}
	handles := map[int]*os.File{}
	defer func() {
		for _, h := range handles {
			_ = h.Close()
		}
	}()

	var jobs []windowJob
	for _, v := range vols {
		if !safeNameRE.MatchString(v.Name) {
			return nil, nil, nil, fmt.Errorf("volume name %q %w", v.Name, ErrUnexportable)
		}
		if v.ChunkSize < minChunkSize || v.ChunkSize > maxChunkSize {
			return nil, nil, nil, fmt.Errorf("chunk size %d of %s is out of range", v.ChunkSize, v.Name)
		}
		h, err := os.Open(v.Path) // NOCC:Path Traversal()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open %s: %w", v.Path, err)
		}
		idx := len(m.Files)
		handles[idx] = h
		st, err := h.Stat()
		if err != nil {
			return nil, nil, nil, err
		}
		size := st.Size()
		m.Files = append(m.Files, File{Role: RoleVolume, Name: v.Name, Size: size, Kind: v.Kind, ChunkSize: v.ChunkSize, Chunks: []Chunk{}})
		data, err := dataExtents(h, size)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("map extents of %s: %w", v.Path, err)
		}
		reuse := baselineReuse(h, v, size, reusable)
		dc := &dataCursor{ranges: data}
		for off := int64(0); off < size; off += v.ChunkSize {
			end := min(off+v.ChunkSize, size)
			if !dc.any(off, end) {
				continue
			}
			if ch, ok := reuse(off, end); ok {
				if ch != nil {
					m.Files[idx].Chunks = append(m.Files[idx].Chunks, *ch)
					stats.ReusedBytes += ch.Length
				}
				continue
			}
			jobs = append(jobs, windowJob{file: idx, offset: off, length: end - off})
		}
	}
	for _, sf := range meta {
		if err := validateMetaPath(sf.Name); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: %w", err, ErrUnexportable)
		}
		h, err := os.Open(sf.Path) // NOCC:Path Traversal()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open %s: %w", sf.Path, err)
		}
		idx := len(m.Files)
		handles[idx] = h
		st, err := h.Stat()
		if err != nil {
			return nil, nil, nil, err
		}
		m.Files = append(m.Files, File{Role: RoleMeta, Name: sf.Name, Size: st.Size(), Mode: sf.Mode, Chunks: []Chunk{}})
		for off := int64(0); off < st.Size(); off += DefaultChunkSize {
			jobs = append(jobs, windowJob{file: idx, offset: off, length: min(DefaultChunkSize, st.Size()-off)})
		}
	}

	tm.PlanMS = time.Since(planStart).Milliseconds()
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup

		readNS, hashNS, uploadNS atomic.Int64
	)
	transferStart := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	work := make(chan windowJob)
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				f := &m.Files[j.file]
				data := make([]byte, j.length)
				t0 := time.Now()
				if err := readChunk(handles[j.file], data, j.offset); err != nil {
					fail(fmt.Errorf("read %s at %d: %w", f.Name, j.offset, err))
					continue
				}
				dropCache(handles[j.file], j.offset, j.length)
				readNS.Add(int64(time.Since(t0)))
				opt.Progress.ScannedBytes.Add(j.length)
				mu.Lock()
				stats.ReadBytes += j.length
				mu.Unlock()
				if allZero(data) {
					continue
				}
				t0 = time.Now()
				sum := sha256.Sum256(data)
				hexSum := hex.EncodeToString(sum[:])
				hashNS.Add(int64(time.Since(t0)))
				var target string
				if f.Role == RoleMeta {
					target = ChunkKey(prefix, snapshotID, session, hexSum)
				} else {
					target = LineageChunkKey(prefix, opt.SandboxID, gen, hexSum)
				}
				key, upload := known.claim(hexSum, target)
				if upload {
					t0 = time.Now()
					err := c.Put(ctx, key, data)
					uploadNS.Add(int64(time.Since(t0)))
					if err != nil {
						fail(fmt.Errorf("upload chunk of %s at %d: %w", f.Name, j.offset, err))
						continue
					}
					opt.Progress.UploadedBytes.Add(j.length)
					opt.Progress.Objects.Add(1)
				}
				mu.Lock()
				if upload {
					stats.UploadedBytes += j.length
					stats.UploadedObjects++
				} else {
					stats.DedupedBytes += j.length
				}
				f.Chunks = append(f.Chunks, Chunk{Offset: j.offset, Length: j.length, SHA256: hexSum, Key: key})
				mu.Unlock()
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case work <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	tm.TransferMS = time.Since(transferStart).Milliseconds()
	ms := func(ns int64) int64 { return time.Duration(ns).Milliseconds() }
	tm.ReadMS, tm.HashMS, tm.UploadMS = ms(readNS.Load()), ms(hashNS.Load()), ms(uploadNS.Load())
	if firstErr != nil {
		return nil, nil, stats, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, stats, err
	}
	for i := range m.Files {
		sortChunks(m.Files[i].Chunks)
		for _, ch := range m.Files[i].Chunks {
			stats.DataBytes += ch.Length
			stats.Chunks++
		}
	}
	if err := m.Validate(prefix, snapshotID); err != nil {
		return nil, nil, stats, fmt.Errorf("built an invalid manifest: %w", err)
	}
	ensureStart := time.Now()
	if err := ensureChunksStored(ctx, c, m, handles, stats); err != nil {
		return nil, nil, stats, err
	}
	tm.EnsureMS = time.Since(ensureStart).Milliseconds()
	commitStart := time.Now()
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, nil, stats, err
	}
	mkey := ManifestKey(prefix, snapshotID, session)
	if err := putImmutable(ctx, c, mkey, raw); err != nil {
		return nil, nil, stats, fmt.Errorf("upload manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	cand := &Candidate{
		SchemaVersion:  SchemaVersion,
		SnapshotID:     snapshotID,
		Session:        session,
		ManifestKey:    mkey,
		ManifestSHA256: hex.EncodeToString(sum[:]),
		ManifestSize:   int64(len(raw)),
		CreatedAt:      now().UTC().Format(time.RFC3339Nano),
	}
	craw, err := json.Marshal(cand)
	if err != nil {
		return nil, nil, stats, err
	}
	if err := putImmutable(ctx, c, CandidateKey(prefix, snapshotID), craw); err != nil {
		return nil, nil, stats, fmt.Errorf("commit candidate: %w", err)
	}
	tm.CommitMS = time.Since(commitStart).Milliseconds()
	return cand, m, stats, nil
}

// baselineReuse returns a function that says whether the window [off,end)
// of the file behind h is still the baseline's, and if so which base chunk
// describes it (nil: the window was all zero). It never says yes when the
// blocks cannot be compared, or for a base chunk whose key the export may
// not name.
func baselineReuse(h *os.File, v IncrementalVolume, size int64, reusable func(key, sum string) bool) func(off, end int64) (*Chunk, bool) {
	never := func(int64, int64) (*Chunk, bool) { return nil, false }
	if v.BaselinePath == "" {
		return never
	}
	b, err := os.Open(v.BaselinePath) // NOCC:Path Traversal()
	if err != nil {
		return never
	}
	defer b.Close()
	if st, err := b.Stat(); err != nil || st.Size() != size {
		return never
	}
	newExt, err := physicalExtents(h)
	if err != nil {
		return never
	}
	baseExt, err := physicalExtents(b)
	if err != nil {
		return never
	}
	base := make(map[int64]Chunk, len(v.BaseChunks))
	for _, ch := range v.BaseChunks {
		base[ch.Offset] = ch
	}
	nc, bc := &extentCursor{exts: newExt}, &extentCursor{exts: baseExt}
	return func(off, end int64) (*Chunk, bool) {
		if !sameExtents(nc.window(off, end), bc.window(off, end)) {
			return nil, false
		}
		ch, ok := base[off]
		switch {
		case !ok:
			// Same blocks as a window the base found all zero.
			return nil, true
		case ch.Length != end-off, !reusable(ch.Key, ch.SHA256):
			return nil, false
		default:
			return &ch, true
		}
	}
}

// dataCursor answers, window by window in order, whether a file has any
// allocated data in the window.
type dataCursor struct {
	ranges [][2]int64
	i      int
}

func (d *dataCursor) any(start, end int64) bool {
	for d.i < len(d.ranges) && d.ranges[d.i][1] <= start {
		d.i++
	}
	return d.i < len(d.ranges) && d.ranges[d.i][0] < end
}

// ensureChunksStored lists every prefix m names chunks in and uploads again,
// from the local files, any chunk the store does not hold with the right
// size. A missing volume chunk goes into m's own generation and m is changed
// to name it there: the old key belongs to an older generation, which a
// sweep may be deleting right now. A chunk whose local bytes no longer match
// its digest means the file is not what the manifest was built from.
func ensureChunksStored(ctx context.Context, c *Client, m *Manifest, handles map[int]*os.File, stats *IncrementalStats) error {
	stored, err := listChunkPrefixes(ctx, c, m)
	if err != nil {
		return err
	}
	for i := range m.Files {
		f := &m.Files[i]
		for j := range f.Chunks {
			ch := &f.Chunks[j]
			if size, ok := stored[ch.Key]; ok && size == ch.Length {
				continue
			}
			if f.Role == RoleVolume {
				key := LineageChunkKey(c.Prefix(), m.SandboxID, m.Generation, ch.SHA256)
				if size, ok := stored[key]; ok && size == ch.Length {
					ch.Key = key
					continue
				}
				ch.Key = key
			}
			data := make([]byte, ch.Length)
			if err := readChunk(handles[i], data, ch.Offset); err != nil {
				return fmt.Errorf("read %s at %d: %w", f.Name, ch.Offset, err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != ch.SHA256 {
				return fmt.Errorf("%w: %s at %d", ErrBaselineMismatch, f.Name, ch.Offset)
			}
			if err := c.Put(ctx, ch.Key, data); err != nil {
				return fmt.Errorf("upload missing chunk %s: %w", ch.Key, err)
			}
			stored[ch.Key] = ch.Length
			stats.RepairedObjects++
			stats.UploadedBytes += ch.Length
		}
	}
	return nil
}

// listChunkPrefixes lists the chunk prefixes m names and returns key->size.
// The lineage is listed once as a whole rather than generation by
// generation: a long chain names many generations.
func listChunkPrefixes(ctx context.Context, c *Client, m *Manifest) (map[string]int64, error) {
	lineage := ""
	if m.SchemaVersion == SchemaVersionIncremental && m.SandboxID != "" {
		lineage = LineagePrefix(c.Prefix(), m.SandboxID)
	}
	prefixes := map[string]bool{}
	for _, f := range m.Files {
		for _, ch := range f.Chunks {
			if lineage != "" && strings.HasPrefix(ch.Key, lineage) {
				prefixes[lineage] = true
			} else if i := strings.LastIndex(ch.Key, "/c/"); i >= 0 {
				prefixes[ch.Key[:i+3]] = true
			}
		}
	}
	stored := map[string]int64{}
	for p := range prefixes {
		objs, err := c.ListObjects(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", p, err)
		}
		for _, o := range objs {
			stored[o.Key] = o.Size
		}
	}
	return stored, nil
}

// VerifyStored checks that every chunk m names is in the store with the
// right size, without reading any. It is the readback of a checkpoint whose
// chunks were each checked when they were uploaded.
func VerifyStored(ctx context.Context, c *Client, m *Manifest) error {
	stored, err := listChunkPrefixes(ctx, c, m)
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		for _, ch := range f.Chunks {
			size, ok := stored[ch.Key]
			if !ok {
				return fmt.Errorf("%w: chunk %s is missing", ErrCorrupt, ch.Key)
			}
			if size != ch.Length {
				return fmt.Errorf("%w: chunk %s is %d bytes, manifest says %d", ErrCorrupt, ch.Key, size, ch.Length)
			}
		}
	}
	return nil
}

// KnownChunks lists the chunks the store holds under prefixes (each ending
// in "/c/") as digest -> key. Earlier prefixes win.
func KnownChunks(ctx context.Context, c *Client, prefixes ...string) (map[string]string, error) {
	known := map[string]string{}
	for _, p := range prefixes {
		objs, err := c.ListObjects(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", p, err)
		}
		for _, o := range objs {
			sum := strings.TrimPrefix(o.Key, p)
			if !sha256RE.MatchString(sum) {
				continue
			}
			if _, ok := known[sum]; !ok {
				known[sum] = o.Key
			}
		}
	}
	return known, nil
}

// TemplateBase records the chunks of a template's memory in the store. It
// carries nothing node-specific, so every node that uploads the same
// template memory builds the same bytes.
type TemplateBase struct {
	SchemaVersion int     `json:"schema_version"`
	TemplateID    string  `json:"template_id"`
	Kind          string  `json:"kind"`
	Size          int64   `json:"size"`
	ChunkSize     int64   `json:"chunk_size"`
	Chunks        []Chunk `json:"chunks"`
}

// Validate checks every key is the template's own.
func (b *TemplateBase) Validate(prefix, templateID string) error {
	if b.SchemaVersion != SchemaVersionIncremental || b.TemplateID != templateID {
		return fmt.Errorf("template base of %q is not valid for %q", b.TemplateID, templateID)
	}
	if b.ChunkSize < minChunkSize || b.ChunkSize > maxChunkSize {
		return fmt.Errorf("template base chunk size %d is out of range", b.ChunkSize)
	}
	var end int64
	for _, ch := range b.Chunks {
		if ch.Offset < end || ch.Length <= 0 || ch.Length > b.ChunkSize || ch.Offset > b.Size-ch.Length ||
			!sha256RE.MatchString(ch.SHA256) || ch.Key != TemplateChunkKey(prefix, templateID, ch.SHA256) {
			return fmt.Errorf("template base of %s has an invalid chunk at %d", templateID, ch.Offset)
		}
		end = ch.Offset + ch.Length
	}
	return nil
}

// LoadTemplateBase reads a template's base; ErrNotFound when none exists.
func LoadTemplateBase(ctx context.Context, c *Client, templateID string) (*TemplateBase, error) {
	raw, err := c.Get(ctx, TemplateBaseKey(c.Prefix(), templateID))
	if err != nil {
		return nil, err
	}
	var b TemplateBase
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%w: decode template base: %v", ErrCorrupt, err)
	}
	if err := b.Validate(c.Prefix(), templateID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return &b, nil
}

// ExportTemplateBase uploads the memory of templateID once, as fixed windows
// of chunkSize, and commits its base. A base another node committed with
// the same bytes is success; different bytes are ErrConflict and the
// caller does without the template base.
func ExportTemplateBase(ctx context.Context, c *Client, templateID, path string, chunkSize int64, workers int) (*TemplateBase, error) {
	if err := ValidateID("template id", templateID); err != nil {
		return nil, err
	}
	have, err := KnownChunks(ctx, c, TemplatePrefix(c.Prefix(), templateID)+"c/")
	if err != nil {
		return nil, err
	}
	h, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return nil, err
	}
	defer h.Close()
	st, err := h.Stat()
	if err != nil {
		return nil, err
	}
	data, err := dataExtents(h, st.Size())
	if err != nil {
		return nil, err
	}
	base := &TemplateBase{SchemaVersion: SchemaVersionIncremental, TemplateID: templateID, Kind: KindMemory,
		Size: st.Size(), ChunkSize: chunkSize, Chunks: []Chunk{}}
	if workers <= 0 {
		workers = 4
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	work := make(chan [2]int64)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for win := range work {
				buf := make([]byte, win[1]-win[0])
				if err := readChunk(h, buf, win[0]); err != nil {
					mu.Lock()
					firstErr = errors.Join(firstErr, err)
					mu.Unlock()
					continue
				}
				dropCache(h, win[0], win[1]-win[0])
				if allZero(buf) {
					continue
				}
				sum := sha256.Sum256(buf)
				hexSum := hex.EncodeToString(sum[:])
				key := TemplateChunkKey(c.Prefix(), templateID, hexSum)
				mu.Lock()
				_, stored := have[hexSum]
				have[hexSum] = key
				mu.Unlock()
				if !stored {
					if err := c.Put(ctx, key, buf); err != nil {
						mu.Lock()
						firstErr = errors.Join(firstErr, err)
						mu.Unlock()
						continue
					}
				}
				mu.Lock()
				base.Chunks = append(base.Chunks, Chunk{Offset: win[0], Length: win[1] - win[0], SHA256: hexSum, Key: key})
				mu.Unlock()
			}
		}()
	}
	dc := &dataCursor{ranges: data}
	for off := int64(0); off < st.Size(); off += chunkSize {
		end := min(off+chunkSize, st.Size())
		if dc.any(off, end) {
			work <- [2]int64{off, end}
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(base.Chunks, func(i, j int) bool { return base.Chunks[i].Offset < base.Chunks[j].Offset })
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	if err := putImmutable(ctx, c, TemplateBaseKey(c.Prefix(), templateID), raw); err != nil {
		return nil, err
	}
	return base, nil
}
