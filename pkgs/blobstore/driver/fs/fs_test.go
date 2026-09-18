// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package fs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/gateway"
)

func testStore(t *testing.T, public string) *store {
	t.Helper()
	root := t.TempDir()
	st, err := open(Options{
		Root:          root,
		PublicBaseURL: public,
		MountPath:     "/internal/warehouse/object",
		SigningKey:    []byte("0123456789abcdef0123456789abcdef"),
		Sync:          SyncNone,
	}.Config(root, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPutGetStatDelete(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	info, err := s.Put(ctx, "warehouse/blobs/a/file.bin", bytes.NewReader([]byte("hello")), blobstore.PutOptions{Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 == "" || info.Size != 5 {
		t.Fatalf("%+v", info)
	}
	st, err := s.Stat(ctx, "warehouse/blobs/a/file.bin")
	if err != nil || st.Size != 5 {
		t.Fatalf("%+v %v", st, err)
	}
	obj, err := s.Get(ctx, "warehouse/blobs/a/file.bin", blobstore.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
	obj, err = s.Get(ctx, "warehouse/blobs/a/file.bin", blobstore.GetOptions{Range: &blobstore.ByteRange{Start: 1, End: 3}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if string(body) != "ell" {
		t.Fatalf("range %q", body)
	}
	if err := s.Delete(ctx, "warehouse/blobs/a/file.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "warehouse/blobs/a/file.bin"); !blobstore.IsNotExist(err) {
		t.Fatalf("want not exist, got %v", err)
	}
}

func TestIfNotExists(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	key := "warehouse/blobs/amd64/cube-shim/v1/component.tar.gz"
	_, err := s.Put(ctx, key, bytes.NewReader([]byte("one")), blobstore.PutOptions{IfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Put(ctx, key, bytes.NewReader([]byte("two")), blobstore.PutOptions{IfNotExists: true})
	if !errors.Is(err, blobstore.ErrAlreadyExists) {
		t.Fatalf("got %v", err)
	}
	if info.Key != key || info.Size != 3 {
		t.Fatalf("AlreadyExists info=%+v", info)
	}
	obj, err := s.Get(ctx, key, blobstore.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if string(body) != "one" {
		t.Fatalf("overwrote: %q", body)
	}
}

func TestIfNotExistsConcurrent(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	key := "warehouse/blobs/x/y/z.bin"
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := s.Put(ctx, key, bytes.NewReader([]byte{byte(n)}), blobstore.PutOptions{IfNotExists: true})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok, exist := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, blobstore.ErrAlreadyExists):
			exist++
		default:
			t.Fatalf("unexpected %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("winners %d exists %d", ok, exist)
	}
}

func TestListPrefixStringNotDirectory(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	for _, key := range []string{
		"warehouse/blobs/amd64/a.bin",
		"warehouse/blobs/arm64/b.bin",
	} {
		if _, err := s.Put(ctx, key, bytes.NewReader([]byte("x")), blobstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	if err := s.List(ctx, "warehouse/blobs/amd", func(info blobstore.ObjectInfo) error {
		keys = append(keys, info.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "warehouse/blobs/amd64/a.bin" {
		t.Fatalf("keys %v", keys)
	}
}

func TestRejectPathTraversal(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	_, err := s.Put(context.Background(), "../etc/passwd", bytes.NewReader([]byte("x")), blobstore.PutOptions{})
	if err == nil {
		t.Fatal("expected reject")
	}
}

func TestDeletePrunesEmptyDirs(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	key := "warehouse/blobs/amd64/cube-shim/v1/component.tar.gz"
	if _, err := s.Put(ctx, key, bytes.NewReader([]byte("x")), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(s.root, dirData)
	ents, err := os.ReadDir(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		t.Fatalf("leftover %s", e.Name())
	}
}

func TestGCPartsAndRetention(t *testing.T) {
	root := t.TempDir()
	st, err := open(blobstore.Config{
		Driver:    DriverName,
		Namespace: root,
		Retention: []blobstore.RetentionRule{{
			Prefix:               "tmp/",
			ExpireAfter:          time.Hour,
			AbortIncompleteAfter: time.Hour,
		}},
		Extra: map[string]string{extraRoot: root, extraSync: "none", extraPublicBaseURL: "http://10.0.0.1:3010", extraMountPath: "/object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.Put(ctx, "tmp/old.bin", bytes.NewReader([]byte("old")), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, "keep/new.bin", bytes.NewReader([]byte("new")), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(st.root, dirTmp, "stale.part")
	if err := os.WriteFile(part, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(part, old, old); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(st.root, dirData, "tmp", "old.bin")
	if err := os.Chtimes(data, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := st.GC(ctx, blobstore.GCOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExpiredObjects != 1 || res.IncompleteAborted != 1 {
		t.Fatalf("%+v", res)
	}
	if _, err := st.Stat(ctx, "keep/new.bin"); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRejectsLoopbackWhenRequired(t *testing.T) {
	root := t.TempDir()
	st, err := open(Options{
		Root:             root,
		PublicBaseURL:    "http://127.0.0.1:3010",
		MountPath:        "/object",
		RequirePublicURL: true,
		Sync:             SyncNone,
	}.Config(root, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Prepare(context.Background()); err == nil {
		t.Fatal("expected prepare error")
	}
}

func TestSignedURLAndGateway(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	key := "warehouse/blobs/gw.bin"
	if _, err := s.Put(ctx, key, bytes.NewReader([]byte("payload")), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.SignedGetURL(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h := &gateway.Handler{Store: s, Signer: s.Signer()}
	req := httptest.NewRequest(http.MethodGet, raw, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "payload" {
		t.Fatalf("code %d body %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, raw, nil)
	req.Header.Set("Range", "bytes=0-3")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "payl" {
		t.Fatalf("range code %d body %q", rec.Code, rec.Body.String())
	}
}

func TestPublicURLRequiresMountPath(t *testing.T) {
	root := t.TempDir()
	_, err := open(Options{
		Root:          root,
		PublicBaseURL: "http://10.0.0.1:3010",
		Sync:          SyncNone,
	}.Config(root, ""))
	if err == nil {
		t.Fatal("expected mount_path required")
	}
}

func TestPutDeleteConcurrent(t *testing.T) {
	s := testStore(t, "http://10.0.0.1:3010")
	ctx := context.Background()
	key := "warehouse/blobs/race/file.bin"
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.Put(ctx, key, bytes.NewReader([]byte("data")), blobstore.PutOptions{})
		}()
		go func() {
			defer wg.Done()
			_ = s.Delete(ctx, key)
		}()
	}
	wg.Wait()
	if _, err := s.Stat(ctx, key); err != nil && !blobstore.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestOpenViaRegistry(t *testing.T) {
	root := t.TempDir()
	st, err := blobstore.Open(context.Background(), Options{Root: root, Sync: SyncNone}.Config(root, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), "a.bin", bytes.NewReader([]byte("z")), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSignerConcurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, dirState), 0o755); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errc := make(chan error, n)
	keys := make(chan []byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := open(Options{
				Root:          root,
				PublicBaseURL: "http://10.0.0.1:3010",
				MountPath:     "/internal/warehouse/object",
				Sync:          SyncNone,
			}.Config(root, ""))
			if err != nil {
				errc <- err
				return
			}
			if err := st.ensureSigner(); err != nil {
				errc <- err
				return
			}
			if st.signer == nil || len(st.signer.Key) < 16 {
				errc <- errors.New("missing signing key")
				return
			}
			keys <- append([]byte(nil), st.signer.Key...)
		}()
	}
	wg.Wait()
	close(errc)
	close(keys)
	for err := range errc {
		t.Fatal(err)
	}
	var first []byte
	for k := range keys {
		if first == nil {
			first = k
			continue
		}
		if !bytes.Equal(first, k) {
			t.Fatalf("replicas diverged on signing key")
		}
	}
	onDisk, err := os.ReadFile(filepath.Join(root, dirState, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, onDisk) {
		t.Fatal("in-memory key != disk")
	}
}
