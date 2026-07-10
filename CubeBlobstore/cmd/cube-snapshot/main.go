// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// cube-snapshot (Go port) -- chunked, deduplicated, zstd-compressed snapshot
// of a flat directory of regular files into an S3/COS bucket, and back.
//
//	Object layout:
//	  <uuid>/manifest.json
//	  <uuid>/chunks/<chunk-plaintext-sha256>
//	  (content-addressed: identical chunks dedupe to one stored object; the
//	  manifest's per-file extents carry the ordered sha list to reassemble)
//
//	- Each file is sliced into fixed-size chunks (--chunk-size, default
//	  4 MiB). All-zero chunks are detected and NOT uploaded; the manifest
//	  records them as zero extents. Adjacent same-kind chunks are merged
//	  into run-length extents.
//	- Every non-zero chunk is independently zstd-compressed (one frame per
//	  chunk). The compressed form is kept only when it saves at least 5%;
//	  otherwise the plaintext is uploaded as-is. The manifest records the
//	  per-chunk on-wire size (stored_size) and the plaintext SHA256; the
//	  restore side decides "compressed vs plaintext" by comparing
//	  stored_size against the plaintext chunk length.
//	- The manifest.json is uploaded last as a transaction marker.
//	- Restore recreates sparse files (truncate to size + skip zero runs).
//
// CLI (see usage()).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"github.com/klauspost/compress/zstd"
	"github.com/tencentcloud/CubeSandbox/CubeBlobstore/pkg/version"
)

// -------------------------------------------------------------------------
//                                Constants
// -------------------------------------------------------------------------

const (
	defaultChunkSize = 4 << 20 // 4 MiB
	minChunkSize     = 4096
	maxChunkSize     = 64 << 20 // 64 MiB
	defaultParallel  = 16
	maxParallel      = 64

	manifestVersion = 3
	defaultZstdLvl  = 3
	minZstdLvl      = 1
	maxZstdLvl      = 19
	// Keep the compressed form only when it shrinks the chunk to <= 95%
	// of its plaintext size (i.e. saves at least 5%).
	zstdKeepNum = 95
	zstdKeepDen = 100

	maxNameLen = 255
	maxPathLen = 4096

	defaultCfgPath = "/data/cubelet/cos.cfg"
)

// -------------------------------------------------------------------------
//                          Manifest data model (JSON)
// -------------------------------------------------------------------------

// jsonExtent mirrors the C run-length extent. For zero runs the sha256 /
// stored_size arrays are omitted (omitempty), exactly like the C encoder.
type jsonExtent struct {
	Kind       string   `json:"kind"` // "data" | "zero"
	ChunkStart uint32   `json:"chunk_start"`
	ChunkCount uint32   `json:"chunk_count"`
	SHA256     []string `json:"sha256,omitempty"`      // per-chunk plaintext sha (hex)
	StoredSize []int64  `json:"stored_size,omitempty"` // per-chunk on-wire bytes
}

type jsonFile struct {
	Path    string       `json:"path"`
	Size    int64        `json:"size"`
	Mode    int          `json:"mode"`
	Mtime   int64        `json:"mtime"`
	SHA256  string       `json:"sha256"` // whole-file plaintext sha (hex)
	Extents []jsonExtent `json:"extents"`
}

type jsonManifest struct {
	UUID        string     `json:"uuid"`
	Version     int        `json:"version"`
	ChunkSize   int64      `json:"chunk_size"`
	CreatedAt   int64      `json:"created_at"`
	Compression string     `json:"compression"`
	Files       []jsonFile `json:"files"`
}

// -------------------------------------------------------------------------
//                          Validation helpers
// -------------------------------------------------------------------------

func nameIsSafe(name string) bool {
	if name == "" || len(name) > maxNameLen {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c < 0x20 {
			return false
		}
	}
	return true
}

func uuidIsSafe(u string) bool {
	if u == "" || len(u) > 200 {
		return false
	}
	if u == "." || u == ".." {
		return false
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c == '/' || c == '\\' || c < 0x20 {
			return false
		}
	}
	return true
}

// abspathIsSafe validates a manifest-recorded absolute path: must start with
// '/', no control chars, no empty/'.'/'..' components, no trailing '/', each
// component <= maxNameLen. Matches the C abspath_is_safe() exactly.
func abspathIsSafe(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	if len(p) >= maxPathLen {
		return false
	}
	if p[len(p)-1] == '/' {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || len(seg) > maxNameLen {
			return false
		}
		if seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if c < 0x20 || c == 0x7f {
				return false
			}
		}
	}
	return true
}

// chunkObjectKey returns the content-addressed object key for a chunk whose
// plaintext SHA256 (hex) is shaHex:
//
//	<uuid>/chunks/<sha256>
//
// Content addressing keeps keys fixed-length regardless of how long or
// deeply-nested the source file path is, sidesteps path-encoding pitfalls,
// and lets identical chunks collapse to a single stored object. The manifest
// still records each file's absolute path plus the ordered per-chunk sha
// list, which is what restore uses to fetch chunks and reassemble the file.
func chunkObjectKey(uuid, shaHex string) string {
	return fmt.Sprintf("%s/chunks/%s", uuid, shaHex)
}

func isChunkZero(buf []byte) bool {
	for _, c := range buf {
		if c != 0 {
			return false
		}
	}
	return true
}

func sha256Hex(sum [sha256.Size]byte) string { return hex.EncodeToString(sum[:]) }

// -------------------------------------------------------------------------
//                          S3 / COS client wrapper
// -------------------------------------------------------------------------

type cosClient struct {
	cli    *s3.Client
	bucket string
}

var errNotFound = errors.New("not found")

func newCosClient(ctx context.Context, bucket, region, endpoint, id, key string) (*cosClient, error) {
	if !strings.Contains(endpoint, region) {
		return nil, fmt.Errorf("region %q must appear in endpoint %q", region, endpoint)
	}
	scheme := "https"
	ep := endpoint
	if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
		// endpoint already carries a scheme; honour it.
	} else {
		ep = scheme + "://" + ep
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(id, key, "")),
	)
	if err != nil {
		return nil, err
	}
	cli := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.Region = region
		o.BaseEndpoint = aws.String(ep)
		o.UsePathStyle = false // virtual-hosted: <bucket>.<endpoint>
	})
	return &cosClient{cli: cli, bucket: bucket}, nil
}

func (c *cosClient) put(ctx context.Context, key string, data []byte) error {
	_, err := c.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	return err
}

func (c *cosClient) get(ctx context.Context, key string) ([]byte, error) {
	out, err := c.cli.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (c *cosClient) head(ctx context.Context, key string) error {
	_, err := c.cli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return errNotFound
		}
		return err
	}
	return nil
}

func (c *cosClient) del(ctx context.Context, key string) error {
	_, err := c.cli.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	return err
}

type listPage struct {
	keys      []string
	sizes     []int64
	prefixes  []string
	truncated bool
	nextToken string
}

func (c *cosClient) listOnePage(ctx context.Context, prefix, delimiter, token string) (*listPage, error) {
	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		MaxKeys: aws.Int32(1000),
	}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	if delimiter != "" {
		in.Delimiter = aws.String(delimiter)
	}
	if token != "" {
		in.ContinuationToken = aws.String(token)
	}
	out, err := c.cli.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, err
	}
	p := &listPage{}
	for _, obj := range out.Contents {
		p.keys = append(p.keys, aws.ToString(obj.Key))
		p.sizes = append(p.sizes, aws.ToInt64(obj.Size))
	}
	for _, cp := range out.CommonPrefixes {
		p.prefixes = append(p.prefixes, aws.ToString(cp.Prefix))
	}
	p.truncated = aws.ToBool(out.IsTruncated)
	if p.truncated {
		p.nextToken = aws.ToString(out.NextContinuationToken)
	}
	return p, nil
}

func isNotFound(err error) bool {
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
//                          Bounded worker pool
// -------------------------------------------------------------------------

type pool struct {
	sem        chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	firstErr   error
	bytesDone  atomic.Uint64
	bytesTotal atomic.Uint64
	wireBytes  atomic.Uint64
	label      string
	start      time.Time
	stopCh     chan struct{}
	stopOnce   sync.Once
}

func newPool(n int, label string, total uint64) *pool {
	p := &pool{
		sem:    make(chan struct{}, n),
		label:  label,
		start:  time.Now(),
		stopCh: make(chan struct{}),
	}
	p.bytesTotal.Store(total)
	go p.progress()
	return p
}

func (p *pool) setErr(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.firstErr == nil {
		p.firstErr = err
	}
	p.mu.Unlock()
}

func (p *pool) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstErr
}

func (p *pool) resetErr() {
	p.mu.Lock()
	p.firstErr = nil
	p.mu.Unlock()
}

// submit runs fn on a worker goroutine, bounded by the semaphore.
func (p *pool) submit(fn func() error) {
	p.sem <- struct{}{}
	p.wg.Add(1)
	go func() {
		defer func() { <-p.sem; p.wg.Done() }()
		if err := fn(); err != nil {
			p.setErr(err)
		}
	}()
}

func (p *pool) drain() { p.wg.Wait() }

func (p *pool) destroy() {
	p.wg.Wait()
	p.stopOnce.Do(func() { close(p.stopCh) })
}

func (p *pool) progress() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			done := p.bytesDone.Load()
			total := p.bytesTotal.Load()
			el := time.Since(p.start).Seconds()
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r%s %.1f%% (%s / %s) %.0fs   ",
					p.label, 100*float64(done)/float64(total),
					humanBytes(done), humanBytes(total), el)
			} else {
				fmt.Fprintf(os.Stderr, "\r%s %s %.0fs   ",
					p.label, humanBytes(done), el)
			}
		}
	}
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// -------------------------------------------------------------------------
//                          zstd (shared codec)
// -------------------------------------------------------------------------

var zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))

func newZstdEncoder(level int) (*zstd.Encoder, error) {
	return zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1))
}

// -------------------------------------------------------------------------
//                          save (upload)
// -------------------------------------------------------------------------

type scannedFile struct {
	abspath string
	size    int64
}

func doSave(ctx context.Context, c *cosClient, dirs []string, uuid string,
	chunkSize int64, parallel int, overwrite bool, zstdLevel int, compress bool) error {

	manifestKey := uuid + "/manifest.json"
	if len(dirs) == 0 {
		return errors.New("at least one --dir is required")
	}

	// Resolve each --dir to its realpath, reject "/", dedup.
	realDirs := make([]string, 0, len(dirs))
	seenDir := map[string]bool{}
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return fmt.Errorf("abs %s: %w", d, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return fmt.Errorf("realpath %s: %w", d, err)
		}
		if real == "/" {
			return errors.New("--dir '/' is not a valid source root")
		}
		fi, err := os.Stat(real)
		if err != nil {
			return fmt.Errorf("stat %s: %w", real, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", real)
		}
		if seenDir[real] {
			return fmt.Errorf("duplicate --dir resolves to %q", real)
		}
		seenDir[real] = true
		realDirs = append(realDirs, real)
	}

	// Flat-only scan of every --dir into a global unique list.
	var files []scannedFile
	seenPath := map[string]bool{}
	var totalBytes uint64
	for _, root := range realDirs {
		entries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Errorf("opendir %s: %w", root, err)
		}
		for _, e := range entries {
			name := e.Name()
			if name == "." || name == ".." {
				continue
			}
			p := filepath.Join(root, name)
			fi, err := e.Info() // lstat semantics: does not follow symlinks
			if err != nil {
				return fmt.Errorf("lstat %s: %w", p, err)
			}
			mode := fi.Mode()
			if mode.IsDir() {
				return fmt.Errorf("subdirectory not allowed: %q (flat directory only)", p)
			}
			if mode&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink not allowed: %q", p)
			}
			if !mode.IsRegular() {
				return fmt.Errorf("not a regular file: %q", p)
			}
			if !nameIsSafe(name) {
				return fmt.Errorf("unsafe file name %q", name)
			}
			if !abspathIsSafe(p) {
				return fmt.Errorf("unsafe abspath %q", p)
			}
			if seenPath[p] {
				return fmt.Errorf("duplicate abspath %q", p)
			}
			seenPath[p] = true
			files = append(files, scannedFile{abspath: p, size: fi.Size()})
			totalBytes += uint64(fi.Size())
		}
	}
	if len(files) == 0 {
		return errors.New("no files found under the supplied --dir(s); refusing to create an empty snapshot")
	}

	if !overwrite {
		err := c.head(ctx, manifestKey)
		if err == nil {
			return fmt.Errorf("uuid %q already exists; pass --overwrite to replace", uuid)
		}
		if !errors.Is(err, errNotFound) {
			return fmt.Errorf("HEAD %s failed: %w", manifestKey, err)
		}
	}

	m := jsonManifest{
		UUID:        uuid,
		Version:     manifestVersion,
		ChunkSize:   chunkSize,
		CreatedAt:   time.Now().Unix(),
		Compression: "zstd",
	}

	p := newPool(parallel, "save", totalBytes)
	defer p.destroy()

	for i := range files {
		fm, err := uploadOneFile(ctx, c, files[i].abspath, uuid, chunkSize, p, zstdLevel, compress)
		if err != nil {
			p.drain()
			return err
		}
		m.Files = append(m.Files, fm)
		if e := p.err(); e != nil {
			p.drain()
			return e
		}
	}

	p.drain()
	if e := p.err(); e != nil {
		return fmt.Errorf("save failed mid-flight: %w", e)
	}

	// Commit the manifest last (transaction marker).
	body, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	if err := c.put(ctx, manifestKey, body); err != nil {
		return fmt.Errorf("PUT manifest %s failed: %w", manifestKey, err)
	}
	fmt.Fprintf(os.Stderr, "\nINFO: save OK: uuid=%s files=%d data=%s wire=%s\n",
		uuid, len(m.Files), humanBytes(totalBytes), humanBytes(p.wireBytes.Load()))
	return nil
}

// uploadOneFile chunks + uploads a single regular file and returns its
// manifest entry. Chunk PUTs are dispatched onto the pool.
func uploadOneFile(ctx context.Context, c *cosClient, abspath, uuid string,
	chunkSize int64, p *pool, zstdLevel int, compress bool) (jsonFile, error) {

	var fm jsonFile
	if !abspathIsSafe(abspath) {
		return fm, fmt.Errorf("unsafe abspath %q", abspath)
	}
	fi, err := os.Lstat(abspath)
	if err != nil {
		return fm, fmt.Errorf("lstat %s: %w", abspath, err)
	}
	if fi.IsDir() {
		return fm, fmt.Errorf("subdirectory not allowed: %q", abspath)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fm, fmt.Errorf("symlink not allowed: %q", abspath)
	}
	if !fi.Mode().IsRegular() {
		return fm, fmt.Errorf("not a regular file: %q", abspath)
	}
	f, err := os.Open(abspath)
	if err != nil {
		return fm, fmt.Errorf("open %s: %w", abspath, err)
	}
	defer f.Close()

	fm.Path = abspath
	fm.Size = fi.Size()
	fm.Mode = int(fi.Mode().Perm())
	fm.Mtime = fi.ModTime().Unix()

	var enc *zstd.Encoder
	if compress {
		enc, err = newZstdEncoder(zstdLevel)
		if err != nil {
			return fm, err
		}
		defer enc.Close()
	}

	fileHash := sha256.New()
	buf := make([]byte, chunkSize)
	var chunkIdx uint32
	var totalRead int64
	// extents accumulator with run-length merge, mirroring append_chunk_to_extents.
	appendChunk := func(kind string, idx uint32, sha string, stored int64) {
		if n := len(fm.Extents); n > 0 {
			last := &fm.Extents[n-1]
			if last.Kind == kind && last.ChunkStart+last.ChunkCount == idx {
				if kind == "data" {
					last.SHA256 = append(last.SHA256, sha)
					last.StoredSize = append(last.StoredSize, stored)
				}
				last.ChunkCount++
				return
			}
		}
		e := jsonExtent{Kind: kind, ChunkStart: idx, ChunkCount: 1}
		if kind == "data" {
			e.SHA256 = []string{sha}
			e.StoredSize = []int64{stored}
		}
		fm.Extents = append(fm.Extents, e)
	}

	for {
		n, rerr := io.ReadFull(f, buf)
		if n == 0 {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			if rerr != nil {
				return fm, fmt.Errorf("read %s: %w", abspath, rerr)
			}
			break
		}
		chunk := buf[:n]
		fileHash.Write(chunk)
		totalRead += int64(n)

		if isChunkZero(chunk) {
			appendChunk("zero", chunkIdx, "", 0)
			p.bytesDone.Add(uint64(n))
		} else {
			var sum [sha256.Size]byte
			sum = sha256.Sum256(chunk)
			shaHex := sha256Hex(sum)

			// Copy plaintext for the (possible) upload; buf is reused.
			wire := make([]byte, n)
			copy(wire, chunk)
			plainLen := n

			if compress {
				comp := enc.EncodeAll(chunk, make([]byte, 0, n))
				keepThr := int(int64(n) * zstdKeepNum / zstdKeepDen)
				if len(comp) <= keepThr {
					wire = comp
				}
			}
			key := chunkObjectKey(uuid, shaHex)
			appendChunk("data", chunkIdx, shaHex, int64(len(wire)))

			wireBuf := wire
			pl := uint64(plainLen)
			p.submit(func() error {
				if err := c.put(ctx, key, wireBuf); err != nil {
					return fmt.Errorf("PUT %s failed: %w", key, err)
				}
				p.bytesDone.Add(pl)
				p.wireBytes.Add(uint64(len(wireBuf)))
				return nil
			})
		}

		chunkIdx++
		if int64(n) < chunkSize {
			break // short read == EOF
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
	}

	if totalRead != fm.Size {
		fmt.Fprintf(os.Stderr, "WARN: file %s size changed during upload (stat=%d, read=%d)\n",
			abspath, fm.Size, totalRead)
		fm.Size = totalRead
	}
	var fsum [sha256.Size]byte
	copy(fsum[:], fileHash.Sum(nil))
	fm.SHA256 = sha256Hex(fsum)
	return fm, nil
}

// -------------------------------------------------------------------------
//                          restore (download)
// -------------------------------------------------------------------------

func doRestore(ctx context.Context, c *cosClient, uuid string, parallel int,
	verify, doFsync bool, saveManifestTo string) error {

	manifestKey := uuid + "/manifest.json"
	mbuf, err := c.get(ctx, manifestKey)
	if err != nil {
		return fmt.Errorf("GET manifest %s failed: %w", manifestKey, err)
	}
	var m jsonManifest
	if err := json.Unmarshal(mbuf, &m); err != nil {
		return fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if m.Version != manifestVersion {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if m.ChunkSize < minChunkSize || m.ChunkSize > maxChunkSize {
		return fmt.Errorf("manifest chunk_size %d outside [%d, %d]", m.ChunkSize, minChunkSize, maxChunkSize)
	}
	if m.UUID != uuid {
		return fmt.Errorf("manifest uuid %q does not match requested %q", m.UUID, uuid)
	}

	if saveManifestTo != "" {
		if err := os.WriteFile(saveManifestTo, mbuf, 0644); err != nil {
			return fmt.Errorf("write %s: %w", saveManifestTo, err)
		}
	}

	// Pre-flight target-side path checks before any chunk IO.
	seen := map[string]bool{}
	var totalBytes uint64
	for i := range m.Files {
		p := m.Files[i].Path
		if !abspathIsSafe(p) {
			return fmt.Errorf("manifest contains unsafe path %q", p)
		}
		parent := filepath.Dir(p)
		pst, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("parent directory %q does not exist (required for %q): %w", parent, p, err)
		}
		if pst.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent %q is a symlink; refusing to follow (for %q)", parent, p)
		}
		if !pst.IsDir() {
			return fmt.Errorf("parent %q is not a directory (for %q)", parent, p)
		}
		if tst, err := os.Lstat(p); err == nil {
			if tst.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("target %q is a pre-existing symlink; refusing to overwrite", p)
			}
			if !tst.Mode().IsRegular() {
				return fmt.Errorf("target %q exists and is not a regular file", p)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("lstat target %q: %w", p, err)
		}
		if seen[p] {
			return fmt.Errorf("manifest has duplicate path %q", p)
		}
		seen[p] = true
		totalBytes += uint64(m.Files[i].Size)
	}

	p := newPool(parallel, "restore", totalBytes)
	defer p.destroy()

	var verified, skipped int
	for i := range m.Files {
		fm := &m.Files[i]
		fd, err := downloadOneFile(ctx, c, &m, fm, p)
		if err != nil {
			return err
		}
		p.drain()
		if e := p.err(); e != nil {
			fd.Close()
			return e
		}

		if verify || fm.Size <= m.ChunkSize {
			if err := verifyFileSHA(fd, fm); err != nil {
				fd.Close()
				return fmt.Errorf("file %s sha256 verification failed: %w", fm.Path, err)
			}
			verified++
		} else {
			skipped++
		}
		if doFsync {
			if err := fd.Sync(); err != nil {
				fd.Close()
				return err
			}
		}
		fd.Close()
		// Clear the per-pool error sentinel so the next file starts clean.
		p.resetErr()
	}

	fmt.Fprintf(os.Stderr, "\nINFO: restore OK: uuid=%s files=%d restored=%s wire=%s (verified=%d skipped=%d)\n",
		uuid, len(m.Files), humanBytes(totalBytes), humanBytes(p.wireBytes.Load()), verified, skipped)
	return nil
}

// downloadOneFile creates the sparse target file and dispatches chunk GETs.
// Returns the open *os.File (caller closes after drain/verify/fsync).
func downloadOneFile(ctx context.Context, c *cosClient, m *jsonManifest, fm *jsonFile, p *pool) (*os.File, error) {
	if !abspathIsSafe(fm.Path) {
		return nil, fmt.Errorf("manifest contains unsafe path %q", fm.Path)
	}
	// O_RDWR (not O_WRONLY): the same fd is re-read after the download
	// drains to recompute the whole-file sha256 in verifyFileSHA(); a
	// read on a write-only fd returns EBADF. O_NOFOLLOW is defence in
	// depth (the pre-flight already rejects pre-existing symlinks).
	f, err := os.OpenFile(fm.Path,
		os.O_RDWR|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW,
		os.FileMode(fm.Mode)&os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", fm.Path, err)
	}
	if err := f.Truncate(fm.Size); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncate %s to %d: %w", fm.Path, fm.Size, err)
	}

	chunkSize := m.ChunkSize
	for _, e := range fm.Extents {
		if e.Kind == "zero" {
			// truncate already zero-filled; just credit progress.
			start := int64(e.ChunkStart) * chunkSize
			end := int64(e.ChunkStart+e.ChunkCount) * chunkSize
			if end > fm.Size {
				end = fm.Size
			}
			if start < end {
				p.bytesDone.Add(uint64(end - start))
			}
			continue
		}
		for k := uint32(0); k < e.ChunkCount; k++ {
			cidx := e.ChunkStart + k
			offset := int64(cidx) * chunkSize
			if offset >= fm.Size {
				break
			}
			plainLen := chunkSize
			if offset+plainLen > fm.Size {
				plainLen = fm.Size - offset
			}
			storedLen := e.StoredSize[k]
			expSha := e.SHA256[k]
			key := chunkObjectKey(m.UUID, expSha)
			off := offset
			pl := plainLen
			p.submit(func() error {
				return downloadChunk(ctx, c, f, key, storedLen, pl, off, expSha, p)
			})
		}
	}
	return f, nil
}

func downloadChunk(ctx context.Context, c *cosClient, f *os.File, key string,
	storedLen, plainLen, offset int64, expSha string, p *pool) error {

	data, err := c.get(ctx, key)
	if err != nil {
		return fmt.Errorf("GET %s failed: %w", key, err)
	}
	if int64(len(data)) != storedLen {
		return fmt.Errorf("chunk %s size mismatch: want %d, got %d", key, storedLen, len(data))
	}

	var plain []byte
	if storedLen == plainLen {
		plain = data
	} else {
		plain, err = zstdDec.DecodeAll(data, make([]byte, 0, plainLen))
		if err != nil {
			return fmt.Errorf("zstd decompress %s failed: %w", key, err)
		}
		if int64(len(plain)) != plainLen {
			return fmt.Errorf("zstd decompress %s produced %d, expected %d", key, len(plain), plainLen)
		}
	}

	sum := sha256.Sum256(plain)
	if sha256Hex(sum) != expSha {
		return fmt.Errorf("chunk %s sha256 mismatch", key)
	}
	if _, err := f.WriteAt(plain, offset); err != nil {
		return fmt.Errorf("pwrite for %s: %w", key, err)
	}
	p.bytesDone.Add(uint64(plainLen))
	p.wireBytes.Add(uint64(storedLen))
	return nil
}

func verifyFileSHA(f *os.File, fm *jsonFile) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	if sha256Hex(sum) != fm.SHA256 {
		return errors.New("whole-file sha256 mismatch")
	}
	return nil
}

// -------------------------------------------------------------------------
//                          ls
// -------------------------------------------------------------------------

func doList(ctx context.Context, c *cosClient, prefixFilter, exactUUID string, verify bool) error {
	effPrefix := prefixFilter
	if exactUUID != "" {
		effPrefix = exactUUID
	}
	var token string
	var total int
	for {
		page, err := c.listOnePage(ctx, effPrefix, "/", token)
		if err != nil {
			return fmt.Errorf("list failed: %w", err)
		}
		for _, cp := range page.prefixes {
			if len(cp) < 2 || cp[len(cp)-1] != '/' {
				continue
			}
			uuid := cp[:len(cp)-1]
			if !uuidIsSafe(uuid) {
				continue
			}
			if exactUUID != "" && uuid != exactUUID {
				continue
			}
			if verify {
				if err := c.head(ctx, uuid+"/manifest.json"); err != nil {
					continue
				}
			}
			fmt.Println(uuid)
			total++
		}
		if !page.truncated {
			break
		}
		token = page.nextToken
	}
	if exactUUID != "" && total == 0 {
		fmt.Fprintf(os.Stderr, "INFO: uuid %q not found\n", exactUUID)
		return errNotFound
	}
	fmt.Fprintf(os.Stderr, "INFO: %d uuid(s) listed\n", total)
	return nil
}

// -------------------------------------------------------------------------
//                          rm
// -------------------------------------------------------------------------

func doDelete(ctx context.Context, c *cosClient, uuid string, parallel int) error {
	manifestKey := uuid + "/manifest.json"
	rcHead := c.head(ctx, manifestKey)
	manifestExisted := rcHead == nil
	if !manifestExisted && !errors.Is(rcHead, errNotFound) {
		return fmt.Errorf("HEAD %s failed: %w", manifestKey, rcHead)
	}

	// Step 1: delete manifest first so the uuid becomes externally invisible.
	if manifestExisted {
		if err := c.del(ctx, manifestKey); err != nil {
			return fmt.Errorf("DELETE manifest %s failed: %w", manifestKey, err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "WARN: no manifest for uuid %q; deleting orphan chunks if any\n", uuid)
	}

	// Step 2: enumerate everything under "<uuid>/" and delete in parallel.
	prefix := uuid + "/"
	p := newPool(parallel, "rm", 0)
	defer p.destroy()

	var token string
	var total int
	var totalBytes uint64
	for {
		page, err := c.listOnePage(ctx, prefix, "", token)
		if err != nil {
			p.drain()
			return fmt.Errorf("list under %q failed: %w", prefix, err)
		}
		for i, key := range page.keys {
			totalBytes += uint64(page.sizes[i])
			p.bytesTotal.Add(uint64(page.sizes[i]))
			k := key
			sz := uint64(page.sizes[i])
			p.submit(func() error {
				if err := c.del(ctx, k); err != nil {
					return fmt.Errorf("DELETE %s failed: %w", k, err)
				}
				p.bytesDone.Add(sz)
				return nil
			})
			total++
		}
		if !page.truncated {
			break
		}
		token = page.nextToken
	}
	p.drain()
	if e := p.err(); e != nil {
		return fmt.Errorf("rm partially failed for uuid %q: %w", uuid, e)
	}
	fmt.Fprintf(os.Stderr, "\nINFO: rm OK: uuid=%s objects=%d bytes=%s\n",
		uuid, total+boolToInt(manifestExisted), humanBytes(totalBytes))
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// -------------------------------------------------------------------------
//                          cos.cfg parser
// -------------------------------------------------------------------------

type cosCfg struct {
	secretID, secretKey, region, endpoint, bucket string
}

func parseCfgScalar(val string) string {
	val = strings.TrimLeft(val, " \t")
	if val == "" {
		return ""
	}
	if val[0] == '[' {
		val = strings.TrimLeft(val[1:], " \t")
		if val == "" || val[0] == ']' {
			return ""
		}
		return parseCfgScalar(val)
	}
	if val[0] == '"' || val[0] == '\'' {
		q := val[0]
		if idx := strings.IndexByte(val[1:], q); idx >= 0 {
			return val[1 : 1+idx]
		}
		return ""
	}
	end := len(val)
	for i := 0; i < len(val); i++ {
		c := val[i]
		if c == ' ' || c == '\t' || c == ',' || c == ']' || c == '\n' || c == '\r' {
			end = i
			break
		}
	}
	return val[:end]
}

// parseCosCfg loads [cos_config] scalars. Returns (cfg, found, err). A
// missing file yields found=false, err=nil (matches C -ENOENT probe).
func parseCosCfg(path string) (cosCfg, bool, error) {
	var out cosCfg
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, false, nil
		}
		return out, false, err
	}
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || s[0] == '#' {
			continue
		}
		if s[0] == '[' {
			if rb := strings.IndexByte(s, ']'); rb >= 0 {
				inSection = strings.TrimSpace(s[1:rb]) == "cos_config"
			}
			continue
		}
		if !inSection {
			continue
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(s[:eq])
		val := parseCfgScalar(strings.TrimSpace(s[eq+1:]))
		if val == "" {
			continue
		}
		switch key {
		case "secretid", "secret_id":
			out.secretID = val
		case "secretkey", "secret_key":
			out.secretKey = val
		case "region":
			out.region = val
		case "cos_endpoint", "endpoint":
			out.endpoint = val
		case "cos_bucket_name", "bucket":
			out.bucket = val
		}
	}
	return out, true, nil
}

// -------------------------------------------------------------------------
//                          CLI / main
// -------------------------------------------------------------------------

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  Connection options (shared; CLI > env > config):
    --bucket <name> --region <r> --endpoint <ep>
    --secret-id <id> --secret-key <key>
    --config <path>   load unset values from a cubelet-style cos.cfg
                      (default: `+defaultCfgPath+`)

  cube-snapshot save    --uuid <id> --dir <path> [--dir <path> ...]
                         [--chunk-size <bytes>] [--parallel <N>]
                         [--zstd-level <N>] [--no-compress] [--overwrite]
  cube-snapshot restore --uuid <id> [--parallel <N>] [--verify] [--fsync]
                         [--save-manifest-to <local_path>]
  cube-snapshot ls      [--uuid <id> | --prefix <p>] [--verify]
  cube-snapshot rm      --uuid <id> [--parallel <N>] [--force]

Object layout:
  <uuid>/manifest.json
  <uuid>/chunks/<chunk-plaintext-sha256>

Credentials: --secret-id/--secret-key, else env COS_SECRET_ID/COS_SECRET_KEY,
else [cos_config] in the config file (keys: secretid, secretkey, region,
cos_endpoint, cos_bucket_name).
`)
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		usage()
		return 1
	}
	sub := os.Args[1]
	switch sub {
	case "save", "restore", "ls", "rm":
	case "version", "--version":
		fmt.Println(version.String())
		return 0
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		usage()
		return 1
	}

	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var dirs stringSlice
	fs.Var(&dirs, "dir", "source directory (save; repeatable)")
	uuid := fs.String("uuid", "", "snapshot uuid")
	bucket := fs.String("bucket", "", "bucket name")
	region := fs.String("region", "", "region")
	endpoint := fs.String("endpoint", "", "COS endpoint")
	secretID := fs.String("secret-id", "", "secret id")
	secretKey := fs.String("secret-key", "", "secret key")
	chunkSize := fs.Int64("chunk-size", defaultChunkSize, "chunk size in bytes")
	parallel := fs.Int("parallel", defaultParallel, "concurrency")
	prefix := fs.String("prefix", "", "ls prefix filter")
	configPath := fs.String("config", "", "cos.cfg path")
	overwrite := fs.Bool("overwrite", false, "overwrite existing uuid (save)")
	verify := fs.Bool("verify", false, "verify whole-file sha256 / manifest presence")
	force := fs.Bool("force", false, "skip rm confirmation")
	zstdLevel := fs.Int("zstd-level", defaultZstdLvl, "zstd level (1-19)")
	noCompress := fs.Bool("no-compress", false, "disable zstd")
	doFsync := fs.Bool("fsync", false, "fsync each restored file")
	saveManifest := fs.String("save-manifest-to", "", "write manifest copy locally (restore)")

	if err := fs.Parse(os.Args[2:]); err != nil {
		return 1
	}

	isSave := sub == "save"
	isRestore := sub == "restore"
	isLs := sub == "ls"
	isRm := sub == "rm"

	// Resolve credentials/endpoint from config when unset (CLI > env > cfg).
	b, r, ep := *bucket, *region, *endpoint
	sid, skey := *secretID, *secretKey
	{
		var cfg cosCfg
		loaded := false
		if *configPath != "" {
			c, ok, err := parseCosCfg(*configPath)
			if err != nil || !ok {
				fmt.Fprintf(os.Stderr, "ERROR: failed to load --config %q: %v\n", *configPath, err)
				return 1
			}
			cfg, loaded = c, true
		} else {
			c, ok, err := parseCosCfg(defaultCfgPath)
			if ok && err == nil {
				cfg, loaded = c, true
				fmt.Fprintf(os.Stderr, "INFO: loaded default config %s\n", defaultCfgPath)
			} else if err != nil {
				fmt.Fprintf(os.Stderr, "WARN: ignoring default config %s: %v\n", defaultCfgPath, err)
			}
		}
		if loaded {
			if b == "" {
				b = cfg.bucket
			}
			if r == "" {
				r = cfg.region
			}
			if ep == "" {
				ep = cfg.endpoint
			}
			if sid == "" {
				sid = cfg.secretID
			}
			if skey == "" {
				skey = cfg.secretKey
			}
		}
	}

	// Per-subcommand argument validation.
	if isSave && len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: save requires at least one --dir")
		return 1
	}
	if isRestore && len(dirs) > 0 {
		fmt.Fprintln(os.Stderr, "ERROR: restore takes no --dir")
		return 1
	}
	if (isLs || isRm) && len(dirs) > 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --dir is only meaningful for save")
		return 1
	}
	if (isSave || isRm) && *uuid == "" {
		fmt.Fprintf(os.Stderr, "ERROR: --uuid is required for %s\n", sub)
		return 1
	}
	if b == "" || r == "" || ep == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --bucket / --region / --endpoint are required")
		return 1
	}
	if sid == "" {
		sid = os.Getenv("COS_SECRET_ID")
	}
	if skey == "" {
		skey = os.Getenv("COS_SECRET_KEY")
	}
	if sid == "" || skey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: credentials missing; pass --secret-id/--secret-key or set COS_SECRET_ID / COS_SECRET_KEY")
		return 1
	}
	if *chunkSize < minChunkSize || *chunkSize > maxChunkSize {
		fmt.Fprintf(os.Stderr, "ERROR: --chunk-size must be in [%d, %d]\n", minChunkSize, maxChunkSize)
		return 1
	}
	if *parallel < 1 || *parallel > maxParallel {
		fmt.Fprintf(os.Stderr, "ERROR: --parallel must be in [1, %d]\n", maxParallel)
		return 1
	}
	if *zstdLevel < minZstdLvl || *zstdLevel > maxZstdLvl {
		fmt.Fprintf(os.Stderr, "ERROR: --zstd-level must be in [%d, %d]\n", minZstdLvl, maxZstdLvl)
		return 1
	}
	if *saveManifest != "" && !isRestore {
		fmt.Fprintln(os.Stderr, "ERROR: --save-manifest-to is only valid for restore")
		return 1
	}
	if (isRestore || isRm) && *uuid == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --uuid is required")
		return 1
	}
	if *uuid != "" && !uuidIsSafe(*uuid) {
		fmt.Fprintf(os.Stderr, "ERROR: unsafe uuid %q\n", *uuid)
		return 1
	}
	if isLs && *uuid != "" && *prefix != "" {
		fmt.Fprintln(os.Stderr, "ERROR: ls: --uuid and --prefix are mutually exclusive")
		return 1
	}
	if isRm && !*force {
		if !confirmDestructive(*uuid) {
			return 1
		}
	}

	ctx := context.Background()
	c, err := newCosClient(ctx, b, r, ep, sid, skey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 1
	}

	switch {
	case isSave:
		err = doSave(ctx, c, dirs, *uuid, *chunkSize, *parallel, *overwrite, *zstdLevel, !*noCompress)
	case isRestore:
		err = doRestore(ctx, c, *uuid, *parallel, *verify, *doFsync, *saveManifest)
	case isLs:
		err = doList(ctx, c, *prefix, *uuid, *verify)
	case isRm:
		err = doDelete(ctx, c, *uuid, *parallel)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 1
	}
	return 0
}

func confirmDestructive(uuid string) bool {
	fi, _ := os.Stdin.Stat()
	if fi != nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		fmt.Fprintf(os.Stderr, "ERROR: refusing to delete uuid %q without --force because stdin is not a TTY\n", uuid)
		return false
	}
	fmt.Fprintf(os.Stderr, "This will permanently delete every object under %q/.\nType the uuid %q to confirm: ", uuid, uuid)
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return false
	}
	if strings.TrimSpace(line) != uuid {
		fmt.Fprintln(os.Stderr, "ERROR: confirmation mismatch, aborting")
		return false
	}
	return true
}
