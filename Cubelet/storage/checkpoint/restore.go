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
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoCandidate means the snapshot has no committed session yet.
var ErrNoCandidate = errors.New("checkpoint has no committed candidate")

// ErrCorrupt marks a checkpoint whose objects do not match its manifest.
// Retrying does not fix it; the caller must stop trusting the checkpoint.
var ErrCorrupt = errors.New("checkpoint data is corrupt")

// LoadCandidate reads and validates the fixed key of snapshotID.
func LoadCandidate(ctx context.Context, c *Client, snapshotID string) (*Candidate, error) {
	if err := ValidateID("snapshot id", snapshotID); err != nil {
		return nil, err
	}
	raw, err := c.Get(ctx, CandidateKey(c.Prefix(), snapshotID))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNoCandidate
	}
	if err != nil {
		return nil, err
	}
	var cand Candidate
	if err := json.Unmarshal(raw, &cand); err != nil {
		return nil, fmt.Errorf("%w: decode candidate: %v", ErrCorrupt, err)
	}
	if err := cand.Validate(c.Prefix(), snapshotID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return &cand, nil
}

// LoadManifest fetches the manifest cand pins and checks it.
func LoadManifest(ctx context.Context, c *Client, cand *Candidate) (*Manifest, error) {
	raw, err := c.Get(ctx, cand.ManifestKey)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: manifest %s is missing", ErrCorrupt, cand.ManifestKey)
	}
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(raw, cand, c.Prefix())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return m, nil
}

// Load resolves snapshotID to its committed manifest. When wantDigest is
// set the committed manifest must be exactly that one: a restore only uses
// the session its readback evidence was produced for.
func Load(ctx context.Context, c *Client, snapshotID, wantDigest string) (*Candidate, *Manifest, error) {
	cand, err := LoadCandidate(ctx, c, snapshotID)
	if err != nil {
		return nil, nil, err
	}
	if wantDigest != "" && cand.ManifestSHA256 != wantDigest {
		return nil, nil, fmt.Errorf("%w: committed manifest %s is not the verified %s",
			ErrCorrupt, cand.ManifestSHA256, wantDigest)
	}
	m, err := LoadManifest(ctx, c, cand)
	if err != nil {
		return nil, nil, err
	}
	return cand, m, nil
}

type fetchItem struct {
	file  int
	chunk Chunk
	// local marks a chunk a local base should already hold in place; it is
	// read back and checked, and fetched only if it does not match.
	local bool
	// trusted marks a local chunk whose blocks the file still shares with a
	// base that has not moved since it was saved; it is not read back.
	trusted bool
}

// localChunks reads chunks that a local base put in place.
type localChunks struct {
	inPlace func(file int, ch Chunk) bool
	trusted func(file int, ch Chunk) bool
	read    func(file int, ch Chunk) ([]byte, error)
	reused  atomic.Int64
	// trustedBytes counts reused chunks taken without a readback.
	trustedBytes atomic.Int64
	fetched      atomic.Int64
	objects      atomic.Int64
	// repaired counts chunks the base claimed but did not hold.
	repaired atomic.Int64
	// Time the workers spent in each step, summed over workers.
	getNS, checkNS, writeNS, localNS atomic.Int64
}

// checkChunk reports whether data is exactly the chunk ch names.
func checkChunk(data []byte, ch Chunk) error {
	if int64(len(data)) != ch.Length {
		return fmt.Errorf("%w: chunk %s is %d bytes, manifest says %d",
			ErrCorrupt, ch.Key, len(data), ch.Length)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != ch.SHA256 {
		return fmt.Errorf("%w: chunk %s digest mismatch", ErrCorrupt, ch.Key)
	}
	return nil
}

// fetchChunks gets every chunk of m once per occurrence, checks its digest
// and length, and hands it to sink. A chunk loc says is already in place is
// read locally and checked instead, and fetched only if it does not match.
// Workers stop on the first error.
func fetchChunks(ctx context.Context, c *Client, m *Manifest, workers int, progress *Progress,
	sink func(file int, ch Chunk, data []byte) error, loc *localChunks) error {
	if workers <= 0 {
		workers = 4
	}
	if progress == nil {
		progress = &Progress{}
	}
	progress.TotalBytes.Store(m.DataBytes())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	work := make(chan fetchItem)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				if it.trusted {
					loc.reused.Add(it.chunk.Length)
					loc.trustedBytes.Add(it.chunk.Length)
					progress.ScannedBytes.Add(it.chunk.Length)
					continue
				}
				if it.local {
					t := time.Now()
					data, err := loc.read(it.file, it.chunk)
					ok := err == nil && checkChunk(data, it.chunk) == nil
					loc.localNS.Add(int64(time.Since(t)))
					if ok {
						loc.reused.Add(it.chunk.Length)
						progress.ScannedBytes.Add(it.chunk.Length)
						continue
					}
					loc.repaired.Add(it.chunk.Length)
				}
				t := time.Now()
				data, err := c.Get(ctx, it.chunk.Key)
				if loc != nil {
					loc.getNS.Add(int64(time.Since(t)))
				}
				if errors.Is(err, ErrNotFound) {
					fail(fmt.Errorf("%w: chunk %s is missing", ErrCorrupt, it.chunk.Key))
					continue
				}
				if err != nil {
					fail(err)
					continue
				}
				t = time.Now()
				err = checkChunk(data, it.chunk)
				if loc != nil {
					loc.checkNS.Add(int64(time.Since(t)))
				}
				if err != nil {
					fail(err)
					continue
				}
				t = time.Now()
				err = sink(it.file, it.chunk, data)
				if loc != nil {
					loc.writeNS.Add(int64(time.Since(t)))
				}
				if err != nil {
					fail(err)
					continue
				}
				if loc != nil {
					loc.fetched.Add(it.chunk.Length)
					loc.objects.Add(1)
				}
				progress.ScannedBytes.Add(it.chunk.Length)
				progress.Objects.Add(1)
			}
		}()
	}
feed:
	for i, f := range m.Files {
		for _, ch := range f.Chunks {
			local := loc != nil && loc.inPlace(i, ch)
			select {
			case work <- fetchItem{file: i, chunk: ch, local: local, trusted: local && loc.trusted(i, ch)}:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// Readback reads every object m names and checks it, writing nothing.
func Readback(ctx context.Context, c *Client, m *Manifest, workers int, progress *Progress) error {
	return fetchChunks(ctx, c, m, workers, progress, func(int, Chunk, []byte) error { return nil }, nil)
}

// LocalBase is a local file that an earlier manifest describes, with the
// same kind and chunk size as the file being materialized: the sandbox's
// last checkpoint on this node. Its chunks are where it holds data.
type LocalBase struct {
	Path   string
	Chunks []Chunk
	// Extents is where the base's data sat on disk when it was saved
	// (ExtentMap). With TrustShared, a chunk whose blocks the clone shares
	// with a base still at those extents is taken without a readback.
	Extents     []Extent
	TrustShared bool
}

// MaterializeTiming says where a materialize spent its time. Wall times are
// elapsed; the per-step times are summed over the workers, so they show
// what the workers were busy with rather than add up to the wall time.
type MaterializeTiming struct {
	// Setup creates the files: clones of the bases, sizing, punching what
	// no chunk covers, and comparing extents for FIEMAP trust.
	SetupMS int64 `json:"setup_ms"`
	CloneMS int64 `json:"clone_ms"`
	PunchMS int64 `json:"punch_ms"`
	ShareMS int64 `json:"share_ms"`
	// Fetch is the worker pool: downloads, checks, writes, local reads.
	FetchMS int64 `json:"fetch_ms"`
	SyncMS  int64 `json:"sync_ms"`
	Workers int   `json:"workers"`
	// GetMS includes the bandwidth limit's waits (BandwidthWaitMS) and the
	// retries' backoff.
	GetMS           int64 `json:"get_ms"`
	BandwidthWaitMS int64 `json:"bandwidth_wait_ms"`
	Retries         int64 `json:"retries"`
	CheckMS         int64 `json:"check_ms"`
	WriteMS         int64 `json:"write_ms"`
	// LocalMS reads back and checks base chunks not taken on trust.
	LocalMS int64 `json:"local_ms"`
	// Throttled429 and Throttled503 count the requests the store asked to
	// slow down; ThrottleBackoffMS is the time their retries waited.
	Throttled429      int64 `json:"throttled_429"`
	Throttled503      int64 `json:"throttled_503"`
	ThrottleBackoffMS int64 `json:"throttle_backoff_ms"`
}

// MaterializeStats says where the data of a materialized checkpoint came
// from. Reused bytes were cloned from a local base and checked in place.
type MaterializeStats struct {
	DataBytes      int64 `json:"data_bytes"`
	ReusedBytes    int64 `json:"reused_bytes"`
	FetchedBytes   int64 `json:"fetched_bytes"`
	FetchedObjects int64 `json:"fetched_objects"`
	// RepairedBytes were claimed by a base but did not match, so they were
	// fetched instead.
	RepairedBytes int64 `json:"repaired_bytes"`
	// TrustedBytes of ReusedBytes were taken on FIEMAP evidence alone.
	TrustedBytes int64             `json:"trusted_bytes"`
	BasedFiles   int               `json:"based_files"`
	Timing       MaterializeTiming `json:"timing"`
}

// Materialize writes every file of m to the path pathFor returns, which must
// not exist yet. Ranges no chunk covers stay holes, which read as zero.
// Files are synced before Materialize returns; their directories are the
// caller's to sync.
func Materialize(ctx context.Context, c *Client, m *Manifest, workers int, progress *Progress,
	pathFor func(f *File) (string, error)) error {
	_, err := MaterializeFrom(ctx, c, m, workers, progress, pathFor, nil)
	return err
}

// MaterializeFrom is Materialize, except that a volume file baseFor names a
// local base for starts as a reflink clone of it. A chunk the base holds at
// the same offset with the same digest is read back and checked instead of
// downloaded, and whatever the base holds outside m's chunks is punched out,
// so the result is exactly what a full download would write.
func MaterializeFrom(ctx context.Context, c *Client, m *Manifest, workers int, progress *Progress,
	pathFor func(f *File) (string, error), baseFor func(f *File) *LocalBase) (*MaterializeStats, error) {
	handles := make([]*os.File, len(m.Files))
	defer func() {
		for _, h := range handles {
			if h != nil {
				_ = h.Close()
			}
		}
	}()
	inPlace := make([]map[int64]Chunk, len(m.Files))
	trusted := make([]map[int64]bool, len(m.Files))
	stats := &MaterializeStats{DataBytes: m.DataBytes()}
	tm := &stats.Timing
	since := func(t time.Time) int64 { return time.Since(t).Milliseconds() }
	setupStart := time.Now()
	for i := range m.Files {
		f := &m.Files[i]
		p, err := pathFor(f)
		if err != nil {
			return nil, err
		}
		mode := os.FileMode(0o600)
		if f.Mode != 0 {
			mode = os.FileMode(f.Mode) & os.ModePerm
		}
		h, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode) // NOCC:Path Traversal()
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", p, err)
		}
		handles[i] = h
		var base *LocalBase
		var baseExt []extent
		if f.Role == RoleVolume && baseFor != nil {
			if base = baseFor(f); base != nil {
				t := time.Now()
				baseExt, err = cloneBase(h, base.Path)
				tm.CloneMS += since(t)
				if err != nil {
					base = nil
				}
			}
		}
		if base != nil {
			inPlace[i] = map[int64]Chunk{}
			for _, ch := range base.Chunks {
				inPlace[i][ch.Offset] = ch
			}
			stats.BasedFiles++
		}
		if err := h.Truncate(f.Size); err != nil {
			return nil, fmt.Errorf("size %s: %w", p, err)
		}
		if base != nil {
			t := time.Now()
			if err := punchUncovered(h, f); err != nil {
				return nil, fmt.Errorf("clear %s outside its chunks: %w", p, err)
			}
			tm.PunchMS += since(t)
			if base.TrustShared && baseExt != nil && sameExtents(normalExtents(baseExt), fromExtents(base.Extents)) {
				t = time.Now()
				trusted[i] = sharedChunks(h, baseExt, f, inPlace[i])
				tm.ShareMS += since(t)
			}
		}
	}
	tm.SetupMS = since(setupStart)
	loc := &localChunks{
		inPlace: func(file int, ch Chunk) bool {
			b, ok := inPlace[file][ch.Offset]
			return ok && b.Length == ch.Length && b.SHA256 == ch.SHA256
		},
		trusted: func(file int, ch Chunk) bool { return trusted[file][ch.Offset] },
		read: func(file int, ch Chunk) ([]byte, error) {
			buf := make([]byte, ch.Length)
			if _, err := handles[file].ReadAt(buf, ch.Offset); err != nil {
				return nil, err
			}
			return buf, nil
		},
	}
	if workers <= 0 {
		workers = 4
	}
	tm.Workers = workers
	waited0, retries0, throttle0 := BandwidthWaited(), RequestRetries(), Throttles()
	fetchStart := time.Now()
	err := fetchChunks(ctx, c, m, workers, progress, func(file int, ch Chunk, data []byte) error {
		if _, err := handles[file].WriteAt(data, ch.Offset); err != nil {
			return fmt.Errorf("write %s at %d: %w", handles[file].Name(), ch.Offset, err)
		}
		return nil
	}, loc)
	if err != nil {
		return nil, err
	}
	tm.FetchMS = since(fetchStart)
	tm.BandwidthWaitMS = (BandwidthWaited() - waited0).Milliseconds()
	tm.Retries = RequestRetries() - retries0
	th := Throttles().Sub(throttle0)
	tm.Throttled429, tm.Throttled503, tm.ThrottleBackoffMS = th.Status429, th.Status503, th.Backoff.Milliseconds()
	ms := func(ns int64) int64 { return time.Duration(ns).Milliseconds() }
	tm.GetMS, tm.CheckMS, tm.WriteMS = ms(loc.getNS.Load()), ms(loc.checkNS.Load()), ms(loc.writeNS.Load())
	tm.LocalMS = ms(loc.localNS.Load())
	syncStart := time.Now()
	for _, h := range handles {
		if err := h.Sync(); err != nil {
			return nil, fmt.Errorf("sync %s: %w", h.Name(), err)
		}
	}
	tm.SyncMS = since(syncStart)
	stats.ReusedBytes = loc.reused.Load()
	stats.FetchedBytes = loc.fetched.Load()
	stats.FetchedObjects = loc.objects.Load()
	stats.RepairedBytes = loc.repaired.Load()
	stats.TrustedBytes = loc.trustedBytes.Load()
	return stats, nil
}

// cloneBase makes dst a reflink clone of the base at path and returns where
// the base's data sits on disk, or nil when FIEMAP cannot tell.
func cloneBase(dst *os.File, path string) ([]extent, error) {
	src, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	ext, err := physicalExtents(src)
	if err != nil {
		ext = nil
	}
	return ext, cloneInto(dst, src)
}

// sharedChunks returns the offsets of the in-place chunks of f whose
// windows in h sit on exactly the base's blocks.
func sharedChunks(h *os.File, baseExt []extent, f *File, inPlace map[int64]Chunk) map[int64]bool {
	cloneExt, err := physicalExtents(h)
	if err != nil {
		return nil
	}
	chunks := append([]Chunk(nil), f.Chunks...)
	sort.Slice(chunks, func(a, b int) bool { return chunks[a].Offset < chunks[b].Offset })
	cc, bc := &extentCursor{exts: cloneExt}, &extentCursor{exts: baseExt}
	out := map[int64]bool{}
	for _, ch := range chunks {
		b, ok := inPlace[ch.Offset]
		if !ok || b.Length != ch.Length || b.SHA256 != ch.SHA256 {
			continue
		}
		end := ch.Offset + ch.Length
		cw, bw := cc.window(ch.Offset, end), bc.window(ch.Offset, end)
		if len(bw) > 0 && sameExtents(cw, bw) {
			out[ch.Offset] = true
		}
	}
	return out
}

// Extent is one written range of a file and where it sits on disk, as
// FIEMAP reports it with physically contiguous neighbours merged.
type Extent struct {
	Logical  int64 `json:"l"`
	Physical int64 `json:"p"`
	Length   int64 `json:"n"`
}

// ExtentMap returns where the data of the file at path sits on disk.
func ExtentMap(path string) ([]Extent, error) {
	f, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ext, err := physicalExtents(f)
	if err != nil {
		return nil, err
	}
	norm := normalExtents(ext)
	out := make([]Extent, len(norm))
	for i, e := range norm {
		out[i] = Extent{Logical: e.logical, Physical: e.physical, Length: e.length}
	}
	return out, nil
}

func normalExtents(ext []extent) []extent {
	return (&extentCursor{exts: ext}).window(0, math.MaxInt64)
}

func fromExtents(in []Extent) []extent {
	out := make([]extent, len(in))
	for i, e := range in {
		out[i] = extent{logical: e.Logical, physical: e.Physical, length: e.Length}
	}
	return out
}

// punchUncovered turns every data range of h that no chunk of f covers into
// a hole, so a clone of a base reads as zero wherever m says it should.
func punchUncovered(h *os.File, f *File) error {
	data, err := dataExtents(h, f.Size)
	if err != nil {
		return err
	}
	chunks := append([]Chunk(nil), f.Chunks...)
	sort.Slice(chunks, func(a, b int) bool { return chunks[a].Offset < chunks[b].Offset })
	ci := 0
	for _, ext := range data {
		pos, end := ext[0], ext[1]
		for pos < end {
			for ci < len(chunks) && chunks[ci].Offset+chunks[ci].Length <= pos {
				ci++
			}
			next := end
			if ci < len(chunks) && chunks[ci].Offset < end {
				next = chunks[ci].Offset
			}
			if next > pos {
				if err := punchHole(h, pos, next-pos); err != nil {
					return err
				}
				pos = next
			}
			if ci < len(chunks) && chunks[ci].Offset <= pos {
				pos = min(chunks[ci].Offset+chunks[ci].Length, end)
			}
		}
	}
	return nil
}

// Evidence is what a readback leaves behind in the store.
type Evidence struct {
	SnapshotID     string `json:"snapshot_id"`
	Session        string `json:"session"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Verifier       string `json:"verifier"`
	DataBytes      int64  `json:"data_bytes"`
	LogicalBytes   int64  `json:"logical_bytes"`
	Chunks         int64  `json:"chunks"`
	VerifiedAt     string `json:"verified_at"`
	DurationMS     int64  `json:"duration_ms"`
	// TrialRestore records whether an isolated trial restore backed this
	// evidence. Readback alone proves the data, not the restore path.
	TrialRestore string `json:"trial_restore"`
	// Verify is how the checkpoint was checked: full or existence.
	Verify string `json:"verify,omitempty"`
	// Trial carries the trial runner's own report.
	Trial json.RawMessage `json:"trial,omitempty"`
}

// PutEvidence stores ev next to the session it covers.
func PutEvidence(ctx context.Context, c *Client, ev *Evidence) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return c.Put(ctx, EvidenceKey(c.Prefix(), ev.SnapshotID, ev.Session), raw)
}
