// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package blobstore is a driver-agnostic object store for immutable blobs.
// Callers blank-import a driver and then Open a Config.
package blobstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// Store is a namespace of immutable blobs addressed by key.
//
// Every method must be safe for concurrent use. Keys are slash-separated,
// validated by ValidateKey, and are NOT filesystem paths: drivers are free to
// map them however they like.
type Store interface {
	// Capabilities describes what this backend can actually do, so callers
	// can degrade explicitly instead of discovering a missing feature at
	// runtime.
	Capabilities() Capabilities

	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (ObjectInfo, error)
	Get(ctx context.Context, key string, opts GetOptions) (*Object, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error

	// List walks every object under prefix in lexical key order and calls fn
	// for each. Returning a non-nil error from fn stops the walk.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error

	// SignedGetURL mints a URL a third party can GET without proxying
	// through this process. Returns ErrUnsupported when
	// Capabilities().DirectURL is false.
	SignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error)

	// Prepare makes the namespace usable and must be idempotent.
	Prepare(ctx context.Context) error

	// GC reclaims backend-specific partial state and applies Retention
	// rules on backends with no native lifecycle support.
	GC(ctx context.Context, opts GCOptions) (GCResult, error)

	Close() error
}

// Capabilities describes optional backend features.
type Capabilities struct {
	// DirectURL reports whether SignedGetURL works. False means every read
	// must go through this process.
	DirectURL bool
	// Shared reports whether all replicas and nodes observe the same
	// namespace. False for a node-local directory. Multi-writer refusal is
	// a deploy-time concern (the Helm chart rejects replicas>1 without
	// ReadWriteMany); the fs driver only warns, because write-once instance
	// marks would otherwise fail a crash-restart inside the 2-minute window.
	Shared          bool
	RangeRead       bool
	ConditionalPut  bool
	NativeRetention bool
}

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	SHA256       string // hex, without the "sha256:" prefix
	ETag         string
	ContentType  string
	LastModified time.Time
}

// PutOptions controls a single Put.
type PutOptions struct {
	ContentType string
	// Size is the exact length, or -1 when streaming an unknown length.
	Size int64
	// IfNotExists turns Put into a create: an existing object makes it fail
	// with ErrAlreadyExists instead of being overwritten. Drivers that can
	// cheaply Stat the winner should return its ObjectInfo alongside the
	// error so callers do not need a second round trip.
	IfNotExists bool
	// SHA256 is recorded next to the object so Stat can return it. When
	// empty the driver computes it while writing when it can do so cheaply.
	SHA256 string
}

// GetOptions controls a single Get.
type GetOptions struct {
	// Range selects a byte window. Nil means the whole object.
	Range *ByteRange
}

// ByteRange is an inclusive HTTP-style byte range. End == -1 means through
// the last byte.
type ByteRange struct {
	Start int64
	End   int64
}

// Object is a readable blob plus its metadata. Body must be closed by the
// caller. When Body also implements io.ReadSeeker, HTTP handlers can pass it
// to http.ServeContent.
type Object struct {
	ObjectInfo
	Body io.ReadCloser
	// ContentRange is non-nil when a Range was requested and honoured.
	ContentRange *ByteRange
}

// RetentionRule is a declarative prefix policy. The s3 driver turns it into
// bucket lifecycle rules; the fs driver enforces it from GC.
type RetentionRule struct {
	Prefix               string
	ExpireAfter          time.Duration
	AbortIncompleteAfter time.Duration
}

// GCOptions controls a garbage-collection pass.
type GCOptions struct {
	// Prefix limits the walk. Empty means every Retention rule (and the
	// driver's leftover-part cleanup) is applied.
	Prefix string
	// Now overrides time.Now for tests. Zero means time.Now().
	Now time.Time
}

// GCResult reports what a GC pass reclaimed.
type GCResult struct {
	ExpiredObjects    int
	IncompleteAborted int
}

var (
	ErrNotExist      = errors.New("blobstore: object does not exist")
	ErrAlreadyExists = errors.New("blobstore: object already exists")
	ErrUnsupported   = errors.New("blobstore: capability not supported by backend")
	ErrInvalidKey    = errors.New("blobstore: invalid object key")
	ErrNoSpace       = errors.New("blobstore: insufficient free space")
)

// IsNotExist reports whether err is (or wraps) ErrNotExist.
func IsNotExist(err error) bool {
	return errors.Is(err, ErrNotExist)
}

// IsAlreadyExists reports whether err is (or wraps) ErrAlreadyExists.
func IsAlreadyExists(err error) bool {
	return errors.Is(err, ErrAlreadyExists)
}
