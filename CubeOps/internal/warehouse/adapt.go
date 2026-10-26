// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/signer"
)

// Adapter wraps a blobstore.Store as the warehouse BlobStore.
type Adapter struct {
	Store blobstore.Store
}

// Adapt returns a BlobStore backed by st.
func Adapt(st blobstore.Store) BlobStore {
	if st == nil {
		return nil
	}
	return &Adapter{Store: st}
}

func (a *Adapter) Signer() *signer.Signer {
	type hasSigner interface {
		Signer() *signer.Signer
	}
	if a == nil || a.Store == nil {
		return nil
	}
	if s, ok := a.Store.(hasSigner); ok {
		return s.Signer()
	}
	return nil
}

func toInfo(info blobstore.ObjectInfo) ObjectInfo {
	return ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		SHA256:       info.SHA256,
		LastModified: info.LastModified,
	}
}

func mapNotExist(key string, err error) error {
	if blobstore.IsNotExist(err) {
		return objectNotFoundError{key: key}
	}
	return err
}

func (a *Adapter) Put(ctx context.Context, key string, r io.Reader, contentType string) (ObjectInfo, error) {
	opts := blobstore.PutOptions{ContentType: contentType, Size: -1}
	if strings.HasPrefix(key, blobsPrefix) {
		opts.IfNotExists = true
	}
	info, err := a.Store.Put(ctx, key, r, opts)
	if errors.Is(err, blobstore.ErrAlreadyExists) {
		return a.existingOnConflict(ctx, key, info)
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return toInfo(info), nil
}

func (a *Adapter) existingOnConflict(ctx context.Context, key string, info blobstore.ObjectInfo) (ObjectInfo, error) {
	out := toInfo(info)
	if out.Key == "" {
		st, err := a.Stat(ctx, key)
		if err != nil {
			return ObjectInfo{}, fmt.Errorf("object already exists but stat failed: %w", err)
		}
		out = st
	}
	if out.SHA256 != "" {
		return out, nil
	}
	sum, err := a.hashObject(ctx, key)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("object already exists but hash failed: %w", err)
	}
	out.SHA256 = sum
	if out.Key == "" {
		out.Key = key
	}
	return out, nil
}

func (a *Adapter) hashObject(ctx context.Context, key string) (string, error) {
	body, err := a.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (a *Adapter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := a.Store.Get(ctx, key, blobstore.GetOptions{})
	if err != nil {
		return nil, mapNotExist(key, err)
	}
	return obj.Body, nil
}

func (a *Adapter) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := a.Store.Stat(ctx, key)
	if err != nil {
		return ObjectInfo{}, mapNotExist(key, err)
	}
	return toInfo(info), nil
}

func (a *Adapter) Delete(ctx context.Context, key string) error {
	return a.Store.Delete(ctx, key)
}

func (a *Adapter) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := a.Store.SignedGetURL(ctx, key, ttl)
	if err != nil {
		return "", mapNotExist(key, err)
	}
	return u, nil
}

func (a *Adapter) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := a.Store.List(ctx, prefix, func(info blobstore.ObjectInfo) error {
		out = append(out, toInfo(info))
		return nil
	})
	return out, err
}

// ListIncompleteUploads is a compatibility stub. Production cleanup is
// Store.GC / bucket lifecycle, not Adapter MPU listing.
func (a *Adapter) ListIncompleteUploads(context.Context, string) ([]IncompleteUpload, error) {
	return nil, nil
}

// AbortMultipartUpload is a compatibility stub; see ListIncompleteUploads.
func (a *Adapter) AbortMultipartUpload(context.Context, string, string) error {
	return nil
}

func (a *Adapter) EnsureBucket(ctx context.Context) error {
	return a.Store.Prepare(ctx)
}

func (a *Adapter) EnsureLifecycle(ctx context.Context) error {
	return a.Store.Prepare(ctx)
}

func (a *Adapter) GC(ctx context.Context) error {
	_, err := a.Store.GC(ctx, blobstore.GCOptions{Prefix: Prefix})
	return err
}
