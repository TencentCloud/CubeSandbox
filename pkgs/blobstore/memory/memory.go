// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package memory is an in-memory blobstore.Store for tests.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func init() {
	blobstore.Register(&driver{})
}

type driver struct{}

func (d *driver) Name() string { return "memory" }

func (d *driver) Open(_ context.Context, cfg blobstore.Config) (blobstore.Store, error) {
	return New(cfg), nil
}

type memObject struct {
	data         []byte
	sha256       string
	etag         string
	contentType  string
	lastModified time.Time
}

func (o memObject) info(key string) blobstore.ObjectInfo {
	return blobstore.ObjectInfo{
		Key:          key,
		Size:         int64(len(o.data)),
		SHA256:       o.sha256,
		ETag:         o.etag,
		ContentType:  o.contentType,
		LastModified: o.lastModified,
	}
}

// Store is an in-memory Store.
type Store struct {
	mu        sync.Mutex
	objects   map[string]memObject
	prefix    string
	retention []blobstore.RetentionRule
	now       func() time.Time
}

// New returns an empty memory store.
func New(cfg blobstore.Config) *Store {
	return &Store{
		objects:   map[string]memObject{},
		prefix:    strings.Trim(cfg.Prefix, "/"),
		retention: cfg.Retention,
		now:       time.Now,
	}
}

// SetNow overrides the clock (tests).
func (s *Store) SetNow(now func() time.Time) {
	s.now = now
}

func (s *Store) full(key string) string {
	return blobstore.JoinKey(s.prefix, key)
}

func (s *Store) Capabilities() blobstore.Capabilities {
	return blobstore.Capabilities{
		DirectURL:      false,
		Shared:         true,
		RangeRead:      true,
		ConditionalPut: true,
	}
}

func (s *Store) Put(_ context.Context, key string, r io.Reader, opts blobstore.PutOptions) (blobstore.ObjectInfo, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return blobstore.ObjectInfo{}, err
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, hexSum) {
		return blobstore.ObjectInfo{}, blobstore.ErrInvalidKey
	}
	full := s.full(key)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.IfNotExists {
		if existing, ok := s.objects[full]; ok {
			return existing.info(key), blobstore.ErrAlreadyExists
		}
	}
	ct := opts.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	obj := memObject{
		data:         data,
		sha256:       hexSum,
		etag:         hexSum,
		contentType:  ct,
		lastModified: now,
	}
	s.objects[full] = obj
	return obj.info(key), nil
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func (s *Store) Get(_ context.Context, key string, opts blobstore.GetOptions) (*blobstore.Object, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	obj, ok := s.objects[s.full(key)]
	s.mu.Unlock()
	if !ok {
		return nil, blobstore.ErrNotExist
	}
	data := obj.data
	var cr *blobstore.ByteRange
	if opts.Range != nil {
		start, end, err := clampRange(*opts.Range, int64(len(data)))
		if err != nil {
			return nil, err
		}
		data = data[start : end+1]
		cr = &blobstore.ByteRange{Start: start, End: end}
	}
	info := obj.info(key)
	info.Size = int64(len(data))
	return &blobstore.Object{
		ObjectInfo:   info,
		Body:         readSeekCloser{bytes.NewReader(data)},
		ContentRange: cr,
	}, nil
}

func clampRange(r blobstore.ByteRange, size int64) (int64, int64, error) {
	if size == 0 {
		return 0, -1, blobstore.ErrNotExist
	}
	start := r.Start
	end := r.End
	if end < 0 || end >= size {
		end = size - 1
	}
	if start < 0 || start > end {
		return 0, 0, blobstore.ErrInvalidKey
	}
	return start, end, nil
}

func (s *Store) Stat(_ context.Context, key string) (blobstore.ObjectInfo, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[s.full(key)]
	if !ok {
		return blobstore.ObjectInfo{}, blobstore.ErrNotExist
	}
	return obj.info(key), nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	if err := blobstore.ValidateKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, s.full(key))
	return nil
}

func (s *Store) List(_ context.Context, prefix string, fn func(blobstore.ObjectInfo) error) error {
	if err := blobstore.ValidatePrefix(prefix); err != nil {
		return err
	}
	want := s.full(prefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		if want == "" || strings.HasPrefix(k, want) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	strip := s.prefix
	if strip != "" {
		strip += "/"
	}
	for _, k := range keys {
		userKey := k
		if strip != "" && strings.HasPrefix(k, strip) {
			userKey = strings.TrimPrefix(k, strip)
		}
		if err := fn(s.objects[k].info(userKey)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SignedGetURL(context.Context, string, time.Duration) (string, error) {
	return "", blobstore.ErrUnsupported
}

func (s *Store) Prepare(context.Context) error { return nil }

func (s *Store) GC(_ context.Context, opts blobstore.GCOptions) (blobstore.GCResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = s.now()
	}
	var result blobstore.GCResult
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rule := range s.retention {
		if rule.ExpireAfter <= 0 {
			continue
		}
		fullPrefix := s.full(rule.Prefix)
		if opts.Prefix != "" && !strings.HasPrefix(fullPrefix, s.full(opts.Prefix)) && !strings.HasPrefix(s.full(opts.Prefix), fullPrefix) {
			continue
		}
		for k, obj := range s.objects {
			if fullPrefix != "" && !strings.HasPrefix(k, fullPrefix) {
				continue
			}
			if now.Sub(obj.lastModified) < rule.ExpireAfter {
				continue
			}
			delete(s.objects, k)
			result.ExpiredObjects++
		}
	}
	return result, nil
}

func (s *Store) Close() error { return nil }

// PutRaw inserts an object without validation (test helper).
func (s *Store) PutRaw(key string, data []byte, t time.Time) {
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[s.full(key)] = memObject{
		data:         append([]byte(nil), data...),
		sha256:       hexSum,
		etag:         hexSum,
		contentType:  "application/octet-stream",
		lastModified: t,
	}
}
