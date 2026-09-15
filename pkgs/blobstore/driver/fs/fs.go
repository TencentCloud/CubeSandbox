// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package fs stores blobs on a local or shared filesystem directory.
package fs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/signer"
)

func init() {
	blobstore.Register(&driver{})
}

const (
	DriverName = "fs"

	dirData  = "_data"
	dirMeta  = "_meta"
	dirTmp   = "_tmp"
	dirState = "_state"

	extraRoot             = "root"
	extraPublicBaseURL    = "public_base_url"
	extraSigningKey       = "signing_key"
	extraSignTTL          = "sign_ttl"
	extraReserveBytes     = "reserve_bytes"
	extraSync             = "sync"
	extraRequirePublicURL = "require_public_url"
	extraShared           = "shared"
	extraMountPath        = "mount_path"

	defaultPartTTL = 2 * time.Hour
)

// Options is the typed configuration for the fs driver.
type Options struct {
	Root             string
	PublicBaseURL    string
	SigningKey       []byte
	SignTTL          time.Duration
	ReserveBytes     int64
	Sync             SyncPolicy
	RequirePublicURL bool
	// Shared is a deploy-topology fact (RWX / NFS). When false the driver
	// warns on overlapping writers; the chart, not Prepare, refuses extra replicas.
	Shared    bool
	MountPath string
}

// SyncPolicy controls fsync on Put.
type SyncPolicy string

const (
	SyncAlways SyncPolicy = "always"
	SyncNone   SyncPolicy = "none"
)

// Config converts typed options into a blobstore.Config.
func (o Options) Config(namespace, prefix string) blobstore.Config {
	extra := map[string]string{
		extraRoot:             o.Root,
		extraPublicBaseURL:    o.PublicBaseURL,
		extraSignTTL:          o.SignTTL.String(),
		extraReserveBytes:     strconv.FormatInt(o.ReserveBytes, 10),
		extraSync:             string(o.Sync),
		extraRequirePublicURL: strconv.FormatBool(o.RequirePublicURL),
		extraShared:           strconv.FormatBool(o.Shared),
		extraMountPath:        o.MountPath,
	}
	if len(o.SigningKey) > 0 {
		extra[extraSigningKey] = hex.EncodeToString(o.SigningKey)
	}
	ns := namespace
	if ns == "" {
		ns = o.Root
	}
	return blobstore.Config{
		Driver:    DriverName,
		Namespace: ns,
		Prefix:    prefix,
		Extra:     extra,
	}
}

type driver struct{}

func (d *driver) Name() string { return DriverName }

func (d *driver) Open(_ context.Context, cfg blobstore.Config) (blobstore.Store, error) {
	return open(cfg)
}

type store struct {
	root       string
	prefix     string
	publicBase string
	mountPath  string
	signTTL    time.Duration
	reserve    int64
	sync       SyncPolicy
	requirePub bool
	shared     bool
	retention  []blobstore.RetentionRule
	signer     *signer.Signer
	instanceID string
}

type metaFile struct {
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

func open(cfg blobstore.Config) (*store, error) {
	root := strings.TrimSpace(extra(cfg, extraRoot))
	if root == "" {
		root = strings.TrimSpace(cfg.Namespace)
	}
	if root == "" {
		return nil, fmt.Errorf("blobstore/fs: root directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blobstore/fs: abs root: %w", err)
	}
	syncPol := SyncPolicy(strings.ToLower(strings.TrimSpace(extra(cfg, extraSync))))
	if syncPol == "" {
		syncPol = SyncAlways
	}
	ttl := 5 * time.Minute
	if raw := extra(cfg, extraSignTTL); raw != "" && raw != "0s" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			ttl = d
		}
	}
	var reserve int64
	if raw := extra(cfg, extraReserveBytes); raw != "" {
		reserve, _ = strconv.ParseInt(raw, 10, 64)
	}
	publicBase := strings.TrimSpace(extra(cfg, extraPublicBaseURL))
	mount := strings.TrimSpace(extra(cfg, extraMountPath))
	if publicBase != "" && mount == "" {
		return nil, fmt.Errorf("blobstore/fs: mount_path is required when public_base_url is set")
	}
	key, err := parseSigningKey(extra(cfg, extraSigningKey))
	if err != nil {
		return nil, err
	}
	s := &store{
		root:       abs,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		publicBase: publicBase,
		mountPath:  mount,
		signTTL:    ttl,
		reserve:    reserve,
		sync:       syncPol,
		requirePub: extra(cfg, extraRequirePublicURL) == "true",
		shared:     extra(cfg, extraShared) == "true",
		retention:  cfg.Retention,
		instanceID: newInstanceID(),
	}
	if len(key) > 0 {
		s.signer = &signer.Signer{Key: key}
	}
	return s, nil
}

func extra(cfg blobstore.Config, k string) string {
	if cfg.Extra == nil {
		return ""
	}
	return cfg.Extra[k]
}

func parseSigningKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) > 0 {
		return b, nil
	}
	return nil, fmt.Errorf("blobstore/fs: signing_key must be hex")
}

func newInstanceID() string {
	host, _ := os.Hostname()
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%x", host, os.Getpid(), b[:])
}

func (s *store) Capabilities() blobstore.Capabilities {
	return blobstore.Capabilities{
		DirectURL:      s.canSign(),
		Shared:         s.shared,
		RangeRead:      true,
		ConditionalPut: true,
	}
}

func (s *store) canSign() bool {
	return s.signer != nil && len(s.signer.Key) > 0 && publicURLOK(s.publicBase) && s.mountPath != ""
}

func publicURLOK(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

func (s *store) Prepare(context.Context) error {
	for _, d := range []string{dirData, dirMeta, dirTmp, dirState} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			return fmt.Errorf("blobstore/fs: mkdir %s: %w", d, err)
		}
	}
	if err := s.probeRename(); err != nil {
		return err
	}
	if err := s.selfCheck(); err != nil {
		return err
	}
	if err := s.ensureSigner(); err != nil {
		return err
	}
	if s.requirePub && !publicURLOK(s.publicBase) {
		return fmt.Errorf("blobstore/fs: public_base_url %q is empty or not reachable from compute nodes", s.publicBase)
	}
	s.writeInstanceMark()
	s.warnMultiInstance()
	return nil
}

func (s *store) probeRename() error {
	src := filepath.Join(s.root, dirTmp, ".probe-"+s.instanceID)
	dstDir := filepath.Join(s.root, dirData, ".probe")
	dst := filepath.Join(dstDir, s.instanceID)
	if err := os.WriteFile(src, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("blobstore/fs: probe write: %w", err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		_ = os.Remove(src)
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Remove(src)
		return fmt.Errorf("blobstore/fs: probe rename (tmp and data must share a mount): %w", err)
	}
	_ = os.Remove(dst)
	_ = os.Remove(dstDir)
	return nil
}

func (s *store) selfCheck() error {
	p := filepath.Join(s.root, dirData, ".health-"+s.instanceID)
	if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("blobstore/fs: health write: %w", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("blobstore/fs: health read: %w", err)
	}
	if string(b) != "ok" {
		return fmt.Errorf("blobstore/fs: health read: unexpected content")
	}
	return os.Remove(p)
}

func (s *store) ensureSigner() error {
	if s.signer != nil && len(s.signer.Key) > 0 {
		return nil
	}
	path := filepath.Join(s.root, dirState, "signer.key")
	if key, err := readSignerKey(path); err == nil {
		s.signer = &signer.Signer{Key: key}
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("blobstore/fs: generate signing key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			raw, rerr := readSignerKeyRetry(path)
			if rerr != nil {
				return rerr
			}
			s.signer = &signer.Signer{Key: raw}
			return nil
		}
		return fmt.Errorf("blobstore/fs: write signing key: %w", err)
	}
	_, werr := f.Write(key)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("blobstore/fs: write signing key: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("blobstore/fs: write signing key: %w", cerr)
	}
	s.signer = &signer.Signer{Key: key}
	return nil
}

func readSignerKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 16 {
		return nil, errShortSignerKey
	}
	return raw, nil
}

var errShortSignerKey = errors.New("blobstore/fs: signing key too short")

func readSignerKeyRetry(path string) ([]byte, error) {
	var last error
	for i := 0; i < 20; i++ {
		raw, err := readSignerKey(path)
		if err == nil {
			return raw, nil
		}
		last = err
		time.Sleep(5 * time.Millisecond)
	}
	return nil, fmt.Errorf("blobstore/fs: read signing key: %w", last)
}

func (s *store) writeInstanceMark() {
	dir := filepath.Join(s.root, dirState, "instances")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, s.instanceID), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

func (s *store) warnMultiInstance() {
	if s.shared {
		return
	}
	dir := filepath.Join(s.root, dirState, "instances")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-2 * time.Minute)
	var live []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue
		}
		live = append(live, e.Name())
	}
	if len(live) > 1 {
		slog.Warn("blobstore/fs: multiple live writers on a non-shared directory",
			"root", s.root, "instances", live)
	}
}

func (s *store) confine(kind, key string) (string, error) {
	full := blobstore.JoinKey(s.prefix, key)
	if err := blobstore.ValidateKey(full); err != nil {
		return "", err
	}
	root := filepath.Join(s.root, kind)
	dest := filepath.Join(root, filepath.FromSlash(full))
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absDest)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", blobstore.ErrInvalidKey
	}
	return absDest, nil
}

func (s *store) dataPath(key string) (string, error) { return s.confine(dirData, key) }
func (s *store) metaPath(key string) (string, error) {
	p, err := s.confine(dirMeta, key)
	if err != nil {
		return "", err
	}
	return p + ".json", nil
}

func (s *store) Put(ctx context.Context, key string, r io.Reader, opts blobstore.PutOptions) (blobstore.ObjectInfo, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	dest, err := s.dataPath(key)
	if err != nil {
		return blobstore.ObjectInfo{}, err
	}
	metaDest, err := s.metaPath(key)
	if err != nil {
		return blobstore.ObjectInfo{}, err
	}
	if err := s.ensureSpace(opts.Size); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Join(s.root, dirTmp), 0o755); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, dirTmp), "put-*.part")
	if err != nil {
		return blobstore.ObjectInfo{}, fmt.Errorf("blobstore/fs: tmp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(r, h))
	if err != nil {
		return blobstore.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	if opts.Size > 0 && n != opts.Size {
		return blobstore.ObjectInfo{}, fmt.Errorf("blobstore/fs: wrote %d bytes, expected %d", n, opts.Size)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, sum) {
		return blobstore.ObjectInfo{}, fmt.Errorf("blobstore/fs: sha256 mismatch")
	}
	if s.sync != SyncNone {
		if err := tmp.Sync(); err != nil {
			return blobstore.ObjectInfo{}, err
		}
	}
	if err := tmp.Close(); err != nil {
		return blobstore.ObjectInfo{}, err
	}

	if opts.IfNotExists {
		if err := retryOnENOENT(dest, func() error { return os.Link(tmpName, dest) }); err != nil {
			if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
				info, statErr := s.Stat(ctx, key)
				if statErr != nil {
					return blobstore.ObjectInfo{}, blobstore.ErrAlreadyExists
				}
				return info, blobstore.ErrAlreadyExists
			}
			return blobstore.ObjectInfo{}, err
		}
		_ = os.Remove(tmpName)
	} else {
		if err := retryOnENOENT(dest, func() error { return os.Rename(tmpName, dest) }); err != nil {
			return blobstore.ObjectInfo{}, err
		}
	}
	cleanup = false
	if err := os.Chmod(dest, 0o644); err != nil {
		// best-effort; some bind mounts reject chmod
		_ = err
	}

	ct := opts.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	if err := s.writeMeta(metaDest, metaFile{SHA256: sum, ContentType: ct, Size: n}); err != nil {
		_ = os.Remove(dest)
		return blobstore.ObjectInfo{}, err
	}
	if s.sync != SyncNone {
		syncDir(filepath.Dir(dest))
		syncDir(filepath.Dir(metaDest))
	}
	st, err := os.Stat(dest)
	mod := time.Now()
	if err == nil {
		mod = st.ModTime()
	}
	return blobstore.ObjectInfo{
		Key:          key,
		Size:         n,
		SHA256:       sum,
		ETag:         sum,
		ContentType:  ct,
		LastModified: mod,
	}, nil
}

func (s *store) writeMeta(path string, m metaFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, dirTmp), "meta-*.part")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if s.sync != SyncNone {
		_ = tmp.Sync()
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return retryOnENOENT(path, func() error { return os.Rename(name, path) })
}

// retryOnENOENT MkdirAlls the parent, runs op, and retries once if a
// concurrent Delete/pruneEmpty removed the directory between the two.
func retryOnENOENT(path string, op func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	err := op()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return err
	}
	return op()
}

func (s *store) readMeta(key string) metaFile {
	p, err := s.metaPath(key)
	if err != nil {
		return metaFile{}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return metaFile{}
	}
	var m metaFile
	_ = json.Unmarshal(raw, &m)
	return m
}

func (s *store) Get(_ context.Context, key string, opts blobstore.GetOptions) (*blobstore.Object, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return nil, err
	}
	p, err := s.dataPath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, blobstore.ErrNotExist
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	m := s.readMeta(key)
	info := blobstore.ObjectInfo{
		Key:          key,
		Size:         st.Size(),
		SHA256:       m.SHA256,
		ETag:         m.SHA256,
		ContentType:  m.ContentType,
		LastModified: st.ModTime(),
	}
	if info.SHA256 == "" {
		info.ETag = ""
	}
	if opts.Range == nil {
		return &blobstore.Object{ObjectInfo: info, Body: f}, nil
	}
	start, end, err := clampRange(*opts.Range, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	info.Size = end - start + 1
	return &blobstore.Object{
		ObjectInfo:   info,
		Body:         &sectionCloser{SectionReader: io.NewSectionReader(f, start, info.Size), c: f},
		ContentRange: &blobstore.ByteRange{Start: start, End: end},
	}, nil
}

type sectionCloser struct {
	*io.SectionReader
	c io.Closer
}

func (s *sectionCloser) Close() error { return s.c.Close() }

func clampRange(r blobstore.ByteRange, size int64) (int64, int64, error) {
	if size <= 0 {
		return 0, 0, blobstore.ErrNotExist
	}
	end := r.End
	if end < 0 || end >= size {
		end = size - 1
	}
	if r.Start < 0 || r.Start > end {
		return 0, 0, blobstore.ErrInvalidKey
	}
	return r.Start, end, nil
}

func (s *store) Stat(_ context.Context, key string) (blobstore.ObjectInfo, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	p, err := s.dataPath(key)
	if err != nil {
		return blobstore.ObjectInfo{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return blobstore.ObjectInfo{}, blobstore.ErrNotExist
		}
		return blobstore.ObjectInfo{}, err
	}
	m := s.readMeta(key)
	return blobstore.ObjectInfo{
		Key:          key,
		Size:         st.Size(),
		SHA256:       m.SHA256,
		ETag:         m.SHA256,
		ContentType:  m.ContentType,
		LastModified: st.ModTime(),
	}, nil
}

func (s *store) Delete(_ context.Context, key string) error {
	if err := blobstore.ValidateKey(key); err != nil {
		return err
	}
	p, err := s.dataPath(key)
	if err != nil {
		return err
	}
	mp, err := s.metaPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(mp)
	pruneEmpty(filepath.Dir(p), filepath.Join(s.root, dirData))
	pruneEmpty(filepath.Dir(mp), filepath.Join(s.root, dirMeta))
	return nil
}

func pruneEmpty(dir, stop string) {
	stop, _ = filepath.Abs(stop)
	for {
		abs, err := filepath.Abs(dir)
		if err != nil || abs == stop || !strings.HasPrefix(abs, stop) {
			return
		}
		ents, err := os.ReadDir(abs)
		if err != nil || len(ents) > 0 {
			return
		}
		if err := os.Remove(abs); err != nil {
			return
		}
		dir = filepath.Dir(abs)
	}
}

func (s *store) List(_ context.Context, prefix string, fn func(blobstore.ObjectInfo) error) error {
	if err := blobstore.ValidatePrefix(prefix); err != nil {
		return err
	}
	fullPrefix := blobstore.JoinKey(s.prefix, prefix)
	dataRoot := filepath.Join(s.root, dirData)
	start := dataRoot
	if i := strings.LastIndex(fullPrefix, "/"); i >= 0 {
		start = filepath.Join(dataRoot, filepath.FromSlash(fullPrefix[:i]))
	}
	if _, err := os.Stat(start); os.IsNotExist(err) {
		return nil
	}
	prefixSlash := ""
	if s.prefix != "" {
		prefixSlash = s.prefix + "/"
	}
	return filepath.WalkDir(start, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dataRoot, path)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if fullPrefix != "" && !strings.HasPrefix(key, fullPrefix) {
			return nil
		}
		userKey := key
		if prefixSlash != "" {
			userKey = strings.TrimPrefix(key, prefixSlash)
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(blobstore.ObjectInfo{
			Key:          userKey,
			Size:         st.Size(),
			LastModified: st.ModTime(),
		})
	})
}

func (s *store) SignedGetURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return "", err
	}
	if !s.canSign() {
		return "", blobstore.ErrUnsupported
	}
	if _, err := s.Stat(context.Background(), key); err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = s.signTTL
	}
	return s.signer.URL(s.publicBase, s.mountPath, key, ttl)
}

func (s *store) GC(_ context.Context, opts blobstore.GCOptions) (blobstore.GCResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var result blobstore.GCResult
	result.IncompleteAborted += s.gcParts(now)
	for _, rule := range s.retention {
		if rule.ExpireAfter <= 0 {
			continue
		}
		_ = s.List(context.Background(), rule.Prefix, func(info blobstore.ObjectInfo) error {
			if opts.Prefix != "" && !strings.HasPrefix(info.Key, opts.Prefix) {
				return nil
			}
			if now.Sub(info.LastModified) < rule.ExpireAfter {
				return nil
			}
			if err := s.Delete(context.Background(), info.Key); err != nil {
				return nil
			}
			result.ExpiredObjects++
			return nil
		})
	}
	return result, nil
}

func (s *store) gcParts(now time.Time) int {
	ttl := defaultPartTTL
	for _, rule := range s.retention {
		if rule.AbortIncompleteAfter > 0 {
			ttl = rule.AbortIncompleteAfter
			break
		}
	}
	dir := filepath.Join(s.root, dirTmp)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < ttl {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

func (s *store) Close() error { return nil }

func (s *store) ensureSpace(size int64) error {
	need := s.reserve
	if size > 0 {
		need += size
	}
	if need <= 0 {
		return nil
	}
	free, err := freeBytes(s.root)
	if err != nil {
		return nil
	}
	if free < need {
		return blobstore.ErrNoSpace
	}
	return nil
}

func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Signer exposes the HMAC signer so callers can mount gateway.Handler.
func (s *store) Signer() *signer.Signer { return s.signer }
