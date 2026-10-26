// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/memory"
)

func TestAdapterPutAlreadyExistsHashesEmptyDigest(t *testing.T) {
	ctx := context.Background()
	inner := memory.New(blobstore.Config{})
	key := blobsPrefix + "amd64/cube-shim/v1/component.tar.gz"
	body := []byte("warehouse-blob")
	want := sha256.Sum256(body)
	if _, err := inner.Put(ctx, key, bytes.NewReader(body), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := Adapt(&stripDigestOnExists{Store: inner}).Put(ctx, key, bytes.NewReader([]byte("ignored")), "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("SHA256=%q want %s", got.SHA256, hex.EncodeToString(want[:]))
	}
}

func TestAdapterPutAlreadyExistsHashFailure(t *testing.T) {
	ctx := context.Background()
	inner := memory.New(blobstore.Config{})
	key := blobsPrefix + "amd64/cube-shim/v1/component.tar.gz"
	_, err := Adapt(&existsWithoutBody{Store: inner}).Put(ctx, key, bytes.NewReader(nil), "application/gzip")
	if err == nil {
		t.Fatal("expected hash failure, got nil")
	}
}

type stripDigestOnExists struct {
	blobstore.Store
}

func (s *stripDigestOnExists) Put(ctx context.Context, key string, r io.Reader, opts blobstore.PutOptions) (blobstore.ObjectInfo, error) {
	info, err := s.Store.Put(ctx, key, r, opts)
	if errors.Is(err, blobstore.ErrAlreadyExists) {
		info.SHA256 = ""
	}
	return info, err
}

type existsWithoutBody struct {
	blobstore.Store
}

func (s *existsWithoutBody) Put(_ context.Context, key string, _ io.Reader, _ blobstore.PutOptions) (blobstore.ObjectInfo, error) {
	return blobstore.ObjectInfo{Key: key}, blobstore.ErrAlreadyExists
}

func (s *existsWithoutBody) Get(context.Context, string, blobstore.GetOptions) (*blobstore.Object, error) {
	return nil, blobstore.ErrNotExist
}
