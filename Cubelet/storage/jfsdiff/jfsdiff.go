// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package jfsdiff computes, packs and applies the rootfs difference between a
// reflinked package file and the local template replica it was cloned from.
//
// The jfs backend in local-rootfs mode keeps every runtime rootfs on the
// node-local XFS filesystem. A pause package or snapshot only needs to ship
// what changed relative to the template: blocks the guest rewrote and blocks
// it discarded. Reflink clones share physical extents with their source until
// written, so a logical block is unchanged exactly when the package and the
// replica map it to the same physical block. That comparison is exact for any
// clone chain rooted at the replica, regardless of which other files also
// share those extents; FIEMAP_EXTENT_SHARED is NOT usable for this (a changed
// block is usually shared with a sibling generation).
//
// The difference is shipped as one sequentially written blob plus a mapping,
// never as an in-place patch of a shared file: on JuiceFS, scattered 4 KiB
// writes and reads are two orders of magnitude slower than sequential ones.
package jfsdiff

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// ManifestVersion is the on-disk format version of [Manifest].
const ManifestVersion = 1

// Range is a half-open byte interval [Start, End).
type Range struct {
	Start int64 `json:"s"`
	End   int64 `json:"e"`
}

// Len returns End-Start.
func (r Range) Len() int64 { return r.End - r.Start }

// Segment places Length bytes found at BlobOffset of the blob at Offset of the
// reconstructed file.
type Segment struct {
	Offset     int64 `json:"o"`
	Length     int64 `json:"l"`
	BlobOffset int64 `json:"b"`
}

// Identity pins the exact base template file a diff was computed against.
// Template rootfs files on JuiceFS are immutable once committed, so path,
// size, inode and mtime identify one physical version.
type Identity struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size"`
	Inode     uint64 `json:"ino"`
	MtimeNs   int64  `json:"mtime_ns"`
}

// Equal reports whether two identities refer to the same base file version.
func (i Identity) Equal(o Identity) bool {
	return i.Path == o.Path && i.SizeBytes == o.SizeBytes && i.Inode == o.Inode && i.MtimeNs == o.MtimeNs
}

// Manifest describes a published rootfs difference.
type Manifest struct {
	Version int `json:"version"`
	// Base is the cubecow object name of the template rootfs the diff is
	// relative to (empty for a self-contained full image).
	Base         string    `json:"base,omitempty"`
	BaseIdentity *Identity `json:"base_identity,omitempty"`
	// SizeBytes is the logical size of the reconstructed file.
	SizeBytes int64 `json:"size"`
	// Blob is the file name (relative to the manifest) holding segment data.
	Blob      string    `json:"blob"`
	BlobBytes int64     `json:"blob_bytes"`
	Segments  []Segment `json:"segments"`
	Holes     []Range   `json:"holes,omitempty"`
}

// Diff is the result of [Compute].
type Diff struct {
	SizeBytes int64
	// Data are ranges whose content differs from the base (or may differ).
	Data []Range
	// Holes are ranges that read as zero in the package but carry data in the
	// base.
	Holes []Range
}

// DataBytes sums the Data ranges.
func (d *Diff) DataBytes() int64 {
	var n int64
	for _, r := range d.Data {
		n += r.Len()
	}
	return n
}

// mapping is one data extent: [start,end) at physical phys.
type mapping struct {
	start, end, phys int64
}

func (m mapping) physAt(off int64) int64 { return m.phys + (off - m.start) }

// lookup returns the mapping covering off, if any. ms is sorted by start.
func lookup(ms []mapping, off int64) (mapping, bool) {
	i := sort.Search(len(ms), func(i int) bool { return ms[i].end > off })
	if i < len(ms) && ms[i].start <= off {
		return ms[i], true
	}
	return mapping{}, false
}

// computeFromMaps implements the physical comparison on already-read extent
// maps. pkgSize/baseSize bound the respective files.
func computeFromMaps(pkg []mapping, pkgSize int64, base []mapping, baseSize int64) *Diff {
	pts := map[int64]struct{}{0: {}, pkgSize: {}}
	add := func(v int64) {
		if v > 0 && v < pkgSize {
			pts[v] = struct{}{}
		}
	}
	for _, m := range pkg {
		add(m.start)
		add(m.end)
	}
	for _, m := range base {
		add(m.start)
		add(m.end)
	}
	if baseSize > 0 {
		add(baseSize)
	}
	sorted := make([]int64, 0, len(pts))
	for p := range pts {
		sorted = append(sorted, p)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	d := &Diff{SizeBytes: pkgSize}
	for i := 0; i+1 < len(sorted); i++ {
		a, b := sorted[i], sorted[i+1]
		if a >= b {
			continue
		}
		pm, pok := lookup(pkg, a)
		var bm mapping
		bok := false
		if a < baseSize {
			bm, bok = lookup(base, a)
		}
		switch {
		case pok:
			// No breakpoint lies inside [a,b), so both mappings are contiguous
			// over the whole interval and comparing at a decides it.
			if !bok || pm.physAt(a) != bm.physAt(a) {
				d.Data = appendRange(d.Data, a, b)
			}
		case bok:
			d.Holes = appendRange(d.Holes, a, b)
		}
	}
	return d
}

func appendRange(rs []Range, a, b int64) []Range {
	if n := len(rs); n > 0 && rs[n-1].End == a {
		rs[n-1].End = b
		return rs
	}
	return append(rs, Range{Start: a, End: b})
}

// Compute returns the ranges where pkgPath differs from basePath. basePath may
// be empty, in which case every data extent of pkgPath is reported (a
// self-contained image).
func Compute(pkgPath, basePath string) (*Diff, error) {
	pkg, pkgSize, err := dataMap(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("map package %s: %w", pkgPath, err)
	}
	var base []mapping
	var baseSize int64
	if basePath != "" {
		base, baseSize, err = dataMap(basePath)
		if err != nil {
			return nil, fmt.Errorf("map base %s: %w", basePath, err)
		}
	}
	return computeFromMaps(pkg, pkgSize, base, baseSize), nil
}

const copyChunk = 8 << 20

// IOStats splits the time spent in an I/O pass.
type IOStats struct {
	ReadNs  int64 // time blocked reading the source
	WriteNs int64 // time blocked writing the destination (incl. final sync)
	PunchNs int64 // time spent punching holes (Apply only)
}

// WriteBlob streams d.Data of pkgPath sequentially into w and returns the
// segment mapping, the number of bytes written and the read/write split. Ranges are read in logical
// order; for a freshly paused package they are almost always still in the
// host page cache.
func WriteBlob(pkgPath string, d *Diff, w io.Writer) ([]Segment, int64, IOStats, error) {
	var st IOStats
	f, err := os.Open(pkgPath)
	if err != nil {
		return nil, 0, st, err
	}
	defer f.Close()
	segs := make([]Segment, 0, len(d.Data))
	buf := make([]byte, copyChunk)
	var boff int64
	for _, r := range d.Data {
		segs = append(segs, Segment{Offset: r.Start, Length: r.Len(), BlobOffset: boff})
		for p := r.Start; p < r.End; {
			n := int64(len(buf))
			if rem := r.End - p; rem < n {
				n = rem
			}
			t := time.Now()
			if err := readFullAt(f, buf[:n], p); err != nil {
				return nil, 0, st, fmt.Errorf("read %s at %d: %w", pkgPath, p, err)
			}
			st.ReadNs += int64(time.Since(t))
			t = time.Now()
			if err := writeFull(w, buf[:n]); err != nil {
				return nil, 0, st, err
			}
			st.WriteNs += int64(time.Since(t))
			p += n
			boff += n
		}
	}
	return segs, boff, st, nil
}

// WriteBlobPreferring is WriteBlob, but reads every byte range from alt
// instead of pkgPath wherever alt maps it to the same physical block, which
// guarantees identical content.
//
// alt is meant to be the sandbox rootfs the package was just reflinked from.
// Page cache is per inode, so right after the clone the package reads cold
// from disk while the sandbox file (which the guest just wrote) is still
// cached. Blocks the sandbox rewrote after the clone (a snapshot of a running
// sandbox) moved to new physical blocks and are read from the package.
// It also returns how many bytes came from alt.
func WriteBlobPreferring(pkgPath string, alt *os.File, d *Diff, w io.Writer) ([]Segment, int64, IOStats, int64, error) {
	var st IOStats
	if alt == nil {
		segs, n, st, err := WriteBlob(pkgPath, d, w)
		return segs, n, st, 0, err
	}
	pmap, _, err := dataMap(pkgPath)
	if err != nil {
		return nil, 0, st, 0, err
	}
	amap, _, err := dataMapFile(alt)
	if err != nil {
		return nil, 0, st, 0, err
	}
	f, err := os.Open(pkgPath)
	if err != nil {
		return nil, 0, st, 0, err
	}
	defer f.Close()
	segs := make([]Segment, 0, len(d.Data))
	buf := make([]byte, copyChunk)
	var boff, fromAlt int64
	for _, r := range d.Data {
		segs = append(segs, Segment{Offset: r.Start, Length: r.Len(), BlobOffset: boff})
		for p := r.Start; p < r.End; {
			n := int64(len(buf))
			if rem := r.End - p; rem < n {
				n = rem
			}
			// Largest prefix of [p, p+n) that one source can serve.
			src, cut := f, p+n
			pm, pok := lookup(pmap, p)
			am, aok := lookup(amap, p)
			if pok && aok && pm.physAt(p) == am.physAt(p) {
				src = alt
				cut = minInt64(cut, minInt64(pm.end, am.end))
			} else {
				if pok {
					cut = minInt64(cut, pm.end)
				}
				if aok {
					cut = minInt64(cut, am.end)
				} else if i := sort.Search(len(amap), func(i int) bool { return amap[i].start > p }); i < len(amap) {
					cut = minInt64(cut, amap[i].start)
				}
			}
			if cut <= p {
				cut = p + minInt64(n, 4096)
			}
			n = cut - p
			t := time.Now()
			if err := readFullAt(src, buf[:n], p); err != nil {
				return nil, 0, st, 0, fmt.Errorf("read %s at %d: %w", src.Name(), p, err)
			}
			st.ReadNs += int64(time.Since(t))
			if src == alt {
				fromAlt += n
			}
			t = time.Now()
			if err := writeFull(w, buf[:n]); err != nil {
				return nil, 0, st, 0, err
			}
			st.WriteNs += int64(time.Since(t))
			p += n
			boff += n
		}
	}
	return segs, boff, st, fromAlt, nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Apply reconstructs a package on dstPath, which must already hold the base
// content (a reflink clone of the base replica). It streams blobPath once,
// placing each segment at its offset, punches the holes and sets the size.
func Apply(dstPath, blobPath string, m *Manifest) (IOStats, error) {
	var ist IOStats
	if m == nil {
		return ist, errors.New("nil manifest")
	}
	if m.Version != ManifestVersion {
		return ist, fmt.Errorf("unsupported rootfs diff manifest version %d", m.Version)
	}
	dst, err := os.OpenFile(dstPath, os.O_RDWR, 0)
	if err != nil {
		return ist, err
	}
	defer dst.Close()
	st, err := dst.Stat()
	if err != nil {
		return ist, err
	}
	if st.Size() < m.SizeBytes {
		if err := dst.Truncate(m.SizeBytes); err != nil {
			return ist, err
		}
	}
	if len(m.Segments) > 0 {
		blob, err := os.Open(blobPath)
		if err != nil {
			return ist, err
		}
		defer blob.Close()
		segs := append([]Segment(nil), m.Segments...)
		sort.Slice(segs, func(i, j int) bool { return segs[i].BlobOffset < segs[j].BlobOffset })
		buf := make([]byte, copyChunk)
		for _, s := range segs {
			if s.Length <= 0 || s.Offset < 0 || s.Offset+s.Length > m.SizeBytes {
				return ist, fmt.Errorf("invalid segment %+v for size %d", s, m.SizeBytes)
			}
			for done := int64(0); done < s.Length; {
				n := int64(len(buf))
				if rem := s.Length - done; rem < n {
					n = rem
				}
				t := time.Now()
				if err := readFullAt(blob, buf[:n], s.BlobOffset+done); err != nil {
					return ist, fmt.Errorf("read blob at %d: %w", s.BlobOffset+done, err)
				}
				ist.ReadNs += int64(time.Since(t))
				t = time.Now()
				if err := writeFullAt(dst, buf[:n], s.Offset+done); err != nil {
					return ist, err
				}
				ist.WriteNs += int64(time.Since(t))
				done += n
			}
		}
	}
	tp := time.Now()
	for _, h := range m.Holes {
		if h.Start < 0 || h.End > m.SizeBytes || h.Len() <= 0 {
			return ist, fmt.Errorf("invalid hole %+v for size %d", h, m.SizeBytes)
		}
		if err := punchHole(dst, h.Start, h.Len()); err != nil {
			return ist, fmt.Errorf("punch %d+%d: %w", h.Start, h.Len(), err)
		}
	}
	ist.PunchNs = int64(time.Since(tp))
	if st.Size() > m.SizeBytes {
		if err := dst.Truncate(m.SizeBytes); err != nil {
			return ist, err
		}
	}
	ts := time.Now()
	err = dst.Sync()
	ist.WriteNs += int64(time.Since(ts))
	return ist, err
}

// ZeroSkipCopy copies srcPath to dstPath (created/truncated), skipping holes
// and all-zero 1 MiB aligned chunks so dstPath stays sparse. It returns bytes
// read and bytes written.
func ZeroSkipCopy(srcPath, dstPath string) (read, written int64, err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, 0, err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return 0, 0, err
	}
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if cerr := dst.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	size := st.Size()
	if err := dst.Truncate(size); err != nil {
		return 0, 0, err
	}
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	off := int64(0)
	for off < size {
		ds, de, ok, serr := nextData(src, off, size)
		if serr != nil {
			return read, written, serr
		}
		if !ok {
			break
		}
		for p := ds; p < de; {
			n := int64(chunk) - p%chunk
			if rem := de - p; rem < n {
				n = rem
			}
			if err := readFullAt(src, buf[:n], p); err != nil {
				return read, written, fmt.Errorf("read %s at %d: %w", srcPath, p, err)
			}
			read += n
			if !allZero(buf[:n]) {
				if err := writeFullAt(dst, buf[:n], p); err != nil {
					return read, written, err
				}
				written += n
			}
			p += n
		}
		off = de
	}
	return read, written, dst.Sync()
}

// StatIdentity returns the [Identity] of path.
func StatIdentity(path string) (Identity, error) {
	ino, size, mtime, err := statFile(path)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Path: path, SizeBytes: size, Inode: ino, MtimeNs: mtime}, nil
}

// ReadManifest loads a manifest file.
func ReadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, nil
}

// WriteFileAtomic writes data to path via a fsynced temp file and rename.
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := writeFull(f, data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func readFullAt(f *os.File, b []byte, off int64) error {
	for done := 0; done < len(b); {
		n, err := f.ReadAt(b[done:], off+int64(done))
		done += n
		if done == len(b) {
			return nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func writeFullAt(f *os.File, b []byte, off int64) error {
	for done := 0; done < len(b); {
		n, err := f.WriteAt(b[done:], off+int64(done))
		done += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeFull(w io.Writer, b []byte) error {
	for done := 0; done < len(b); {
		n, err := w.Write(b[done:])
		done += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
