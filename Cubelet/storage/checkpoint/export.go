// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// SourceFile is one local file an export publishes.
type SourceFile struct {
	Role string
	Name string
	Path string
	Mode uint32
}

// Progress counts what a transfer has done so far.
type Progress struct {
	ScannedBytes  atomic.Int64
	TotalBytes    atomic.Int64
	UploadedBytes atomic.Int64
	Objects       atomic.Int64
}

// ExportOptions tunes one export.
type ExportOptions struct {
	ChunkSize int64
	Workers   int
	Progress  *Progress
	// Attributes are copied into the manifest; restore reads package
	// identity (catalog fields) from them.
	Attributes map[string]string
	Now        func() time.Time
}

type exportPiece struct {
	file   int
	offset int64
	length int64
}

// Export uploads files as session of snapshotID and commits the session by
// writing the fixed candidate key last. Every chunk is uploaded before the
// manifest, and the manifest before the candidate, so a reader that finds
// the candidate finds everything it names.
func Export(ctx context.Context, c *Client, snapshotID, session string, files []SourceFile, opt ExportOptions) (*Candidate, *Manifest, error) {
	if err := ValidateID("snapshot id", snapshotID); err != nil {
		return nil, nil, err
	}
	if err := ValidateID("session", session); err != nil {
		return nil, nil, err
	}
	if opt.ChunkSize == 0 {
		opt.ChunkSize = DefaultChunkSize
	}
	if opt.ChunkSize < minChunkSize || opt.ChunkSize > maxChunkSize {
		return nil, nil, fmt.Errorf("chunk size %d is out of range", opt.ChunkSize)
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
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		SnapshotID:    snapshotID,
		Session:       session,
		CreatedAt:     now().UTC().Format(time.RFC3339Nano),
		ChunkSize:     opt.ChunkSize,
		Attributes:    opt.Attributes,
		Files:         make([]File, len(files)),
	}
	handles := make([]*os.File, len(files))
	defer func() {
		for _, h := range handles {
			if h != nil {
				_ = h.Close()
			}
		}
	}()
	var pieces []exportPiece
	for i, sf := range files {
		switch sf.Role {
		case RoleVolume:
			if !safeNameRE.MatchString(sf.Name) {
				return nil, nil, fmt.Errorf("volume name %q %w", sf.Name, ErrUnexportable)
			}
		case RoleMeta:
			if err := validateMetaPath(sf.Name); err != nil {
				return nil, nil, fmt.Errorf("%w: %w", err, ErrUnexportable)
			}
		default:
			return nil, nil, fmt.Errorf("role %q %w", sf.Role, ErrUnexportable)
		}
		h, err := os.Open(sf.Path) // NOCC:Path Traversal()
		if err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", sf.Path, err)
		}
		handles[i] = h
		st, err := h.Stat()
		if err != nil {
			return nil, nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%s is not a regular file and %w", sf.Path, ErrUnexportable)
		}
		size := st.Size()
		m.Files[i] = File{Role: sf.Role, Name: sf.Name, Size: size, Mode: sf.Mode, Chunks: []Chunk{}}
		extents, err := dataExtents(h, size)
		if err != nil {
			return nil, nil, fmt.Errorf("map extents of %s: %w", sf.Path, err)
		}
		for _, ext := range extents {
			// Pieces never straddle a chunk boundary so a re-export of the
			// same file produces the same chunk layout.
			for off := ext[0]; off < ext[1]; {
				next := (off/opt.ChunkSize + 1) * opt.ChunkSize
				if next > ext[1] {
					next = ext[1]
				}
				pieces = append(pieces, exportPiece{file: i, offset: off, length: next - off})
				opt.Progress.TotalBytes.Add(next - off)
				off = next
			}
		}
	}

	var (
		mu       sync.Mutex
		uploaded sync.Map
		firstErr error
		wg       sync.WaitGroup
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan exportPiece)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, opt.ChunkSize)
			for p := range work {
				data := buf[:p.length]
				h := handles[p.file]
				if err := readChunk(h, data, p.offset); err != nil {
					fail(fmt.Errorf("read %s at %d: %w", files[p.file].Path, p.offset, err))
					continue
				}
				dropCache(h, p.offset, p.length)
				opt.Progress.ScannedBytes.Add(p.length)
				if allZero(data) {
					continue
				}
				sum := sha256.Sum256(data)
				hexSum := hex.EncodeToString(sum[:])
				key := ChunkKey(prefix, snapshotID, session, hexSum)
				if _, dup := uploaded.LoadOrStore(hexSum, true); !dup {
					if err := c.Put(ctx, key, data); err != nil {
						uploaded.Delete(hexSum)
						fail(fmt.Errorf("upload chunk of %s at %d: %w", files[p.file].Path, p.offset, err))
						continue
					}
					opt.Progress.UploadedBytes.Add(p.length)
					opt.Progress.Objects.Add(1)
				}
				mu.Lock()
				m.Files[p.file].Chunks = append(m.Files[p.file].Chunks, Chunk{
					Offset: p.offset, Length: p.length, SHA256: hexSum, Key: key,
				})
				mu.Unlock()
			}
		}()
	}
feed:
	for _, p := range pieces {
		select {
		case work <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	for i := range m.Files {
		sortChunks(m.Files[i].Chunks)
	}
	if err := m.Validate(prefix, snapshotID); err != nil {
		return nil, nil, fmt.Errorf("built an invalid manifest: %w", err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	mkey := ManifestKey(prefix, snapshotID, session)
	if err := putImmutable(ctx, c, mkey, raw); err != nil {
		return nil, nil, fmt.Errorf("upload manifest: %w", err)
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
		return nil, nil, err
	}
	if err := putImmutable(ctx, c, CandidateKey(prefix, snapshotID), craw); err != nil {
		return nil, nil, fmt.Errorf("commit candidate: %w", err)
	}
	return cand, m, nil
}

// ErrUnexportable marks a source file a checkpoint cannot carry: a name the
// manifest would reject, or a file that is not a regular one. The same
// files fail the same way on every attempt, so the export stops.
var ErrUnexportable = errors.New("cannot be exported")

// ErrConflict means a commit object already holds different content. A
// committed session is never rewritten: readers may already have verified
// it, so the export stops instead.
var ErrConflict = errors.New("checkpoint object already holds different content")

// putImmutable writes key once. A key that already holds exactly body (an
// earlier attempt of this same export landed) is success; anything else is
// ErrConflict. With the creation time taken from the job, a re-export of a
// session builds byte-identical commit objects.
func putImmutable(ctx context.Context, c *Client, key string, body []byte) error {
	if !c.ConditionalWritesInUse() {
		return putImmutableUnconditional(ctx, c, key, body)
	}
	err := c.PutIfAbsent(ctx, key, body)
	if !errors.Is(err, ErrExists) {
		return err
	}
	existing, gerr := c.Get(ctx, key)
	if gerr != nil {
		return gerr
	}
	if !bytes.Equal(existing, body) {
		return fmt.Errorf("%w: %s", ErrConflict, key)
	}
	return nil
}

// putImmutableUnconditional commits key on a store that ignores
// If-None-Match, the way S3lvol commits on COS: the store cannot refuse a
// second writer, so the write is checked on both sides instead. An existing
// object must already hold exactly body. A missing one is written and read
// back, and must then hold exactly body.
//
// This narrows the window to the moment between the HEAD and the read-back;
// it is not mutual exclusion. It relies on what the exporter guarantees
// anyway: only the origin exports a snapshot, one job at a time, and a
// retried session rebuilds byte-identical commit objects, so a rewrite of
// the same session can never change what readers verified.
func putImmutableUnconditional(ctx context.Context, c *Client, key string, body []byte) error {
	_, err := c.Head(ctx, key)
	switch {
	case err == nil:
		existing, gerr := c.Get(ctx, key)
		if gerr != nil {
			return gerr
		}
		if !bytes.Equal(existing, body) {
			return fmt.Errorf("%w: %s", ErrConflict, key)
		}
		return nil
	case !errors.Is(err, ErrNotFound):
		return err
	}
	if err := c.Put(ctx, key, body); err != nil {
		return err
	}
	back, err := c.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read back %s: %w", key, err)
	}
	if !bytes.Equal(back, body) {
		return fmt.Errorf("%w: %s changed while it was committed", ErrConflict, key)
	}
	return nil
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if b[0]|b[1]|b[2]|b[3]|b[4]|b[5]|b[6]|b[7] != 0 {
			return false
		}
		b = b[8:]
	}
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// readChunk fills data from offset off of r. A file that ends before data is
// full is an error, not a short chunk: a sealed package never shrinks, and
// the rest of data would otherwise be committed as whatever the buffer held.
func readChunk(r io.ReaderAt, data []byte, off int64) error {
	n, err := r.ReadAt(data, off)
	if n == len(data) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return fmt.Errorf("short read: %d of %d bytes", n, len(data))
	}
	return err
}
