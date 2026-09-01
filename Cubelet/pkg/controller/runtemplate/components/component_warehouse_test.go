// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package components

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/warehouse"
)

type stubFetcher struct {
	calls int32
	fn    func(ctx context.Context, name, version string) error
}

func (s *stubFetcher) Fetch(ctx context.Context, name, version string) error {
	atomic.AddInt32(&s.calls, 1)
	if s.fn != nil {
		return s.fn(ctx, name, version)
	}
	return nil
}

func TestEnsure_LocalHitDoesNotFetch(t *testing.T) {
	manager, config, _ := setupTestManager(t)
	shimDir := path.Join(config.VersionedBaseDir, "cube-shim", "1.0.0", "bin")
	require.NoError(t, os.MkdirAll(shimDir, 0755))
	shimFile := path.Join(shimDir, "containerd-shim-cube-rs")
	require.NoError(t, os.WriteFile(shimFile, []byte("shim"), 0755))

	stub := &stubFetcher{fn: func(context.Context, string, string) error {
		t.Fatal("fetch should not run on local hit")
		return nil
	}}
	manager.SetFetcher(stub)
	got, err := manager.Ensure(context.Background(), "cube-shim", "1.0.0", "")
	require.NoError(t, err)
	assert.Equal(t, shimFile, got)
	assert.Equal(t, int32(0), atomic.LoadInt32(&stub.calls))
}

func TestEnsure_MissingWithoutFetcher(t *testing.T) {
	manager, _, _ := setupTestManager(t)
	_, err := manager.Ensure(context.Background(), "cube-shim", "9.9.9", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrComponentVersionMissing))
}

func TestEnsure_DownloadSuccess(t *testing.T) {
	manager, config, _ := setupTestManager(t)
	stub := &stubFetcher{fn: func(_ context.Context, name, version string) error {
		dir := path.Join(config.VersionedBaseDir, name, version, "bin")
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(path.Join(dir, "containerd-shim-cube-rs"), []byte("shim"), 0755))
		return nil
	}}
	manager.SetFetcher(stub)
	got, err := manager.Ensure(context.Background(), "cube-shim", "2.0.0", "")
	require.NoError(t, err)
	assert.FileExists(t, got)
	assert.Equal(t, int32(1), atomic.LoadInt32(&stub.calls))
}

func TestEnsure_WarehouseNotFound(t *testing.T) {
	manager, _, _ := setupTestManager(t)
	manager.SetFetcher(&stubFetcher{fn: func(context.Context, string, string) error {
		return warehouse.ErrNotFound
	}})
	_, err := manager.Ensure(context.Background(), "cube-shim", "missing", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, warehouse.ErrNotFound))
	assert.False(t, errors.Is(err, ErrComponentVersionMissing))
}

func TestEnsure_DownloadFailed(t *testing.T) {
	manager, _, _ := setupTestManager(t)
	manager.SetFetcher(&stubFetcher{fn: func(context.Context, string, string) error {
		return warehouse.ErrDownloadFailed
	}})
	_, err := manager.Ensure(context.Background(), "cube-shim", "bad", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, warehouse.ErrDownloadFailed))
}

func TestEnsure_ConcurrentSingleflight(t *testing.T) {
	manager, config, _ := setupTestManager(t)
	var blobs int32
	release := make(chan struct{})
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/internal/warehouse/blob"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url": "http://" + r.Host + "/s3/blob", "expiresIn": 300, "sizeBytes": 0, "checksum": "",
			})
		case strings.Contains(r.URL.Path, "/s3/"):
			if atomic.AddInt32(&blobs, 1) == 1 {
				close(started)
			}
			<-release
			writeShimTar(t, w)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	manager.SetFetcher(warehouse.NewFetcher(warehouse.NewClient(srv.URL, "node-1", "amd64", time.Minute), config.VersionedBaseDir))

	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := manager.Ensure(context.Background(), "cube-shim", "3.0.0", "")
			errCh <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		require.NoError(t, <-errCh)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&blobs))
}

func TestEnsure_HonorsContext(t *testing.T) {
	manager, config, _ := setupTestManager(t)
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/internal/warehouse/blob"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url": "http://" + r.Host + "/s3/blob", "expiresIn": 300, "sizeBytes": 0, "checksum": "",
			})
		case strings.Contains(r.URL.Path, "/s3/"):
			close(started)
			<-release
			writeShimTar(t, w)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	manager.SetFetcher(warehouse.NewFetcher(warehouse.NewClient(srv.URL, "node-1", "amd64", time.Minute), config.VersionedBaseDir))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.Ensure(ctx, "cube-shim", "ctx", "")
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Ensure did not return after cancel")
	}
	close(release)
	_, err := manager.Ensure(context.Background(), "cube-shim", "ctx", "")
	require.NoError(t, err)
}

func TestClient_NotFoundVsDownloadFailed(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		assert.Equal(t, "node-1", r.Header.Get("X-Cube-Node-ID"))
		switch r.URL.Query().Get("version") {
		case "missing":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"nope","code":"warehouse_not_found"}`))
		case "boom":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream","code":"warehouse_download_failed"}`))
		default:
			writeShimTar(t, w)
		}
	}))
	defer srv.Close()

	c := warehouse.NewClient(srv.URL, "node-1", "amd64", time.Second)
	_, err := c.DownloadBlob(context.Background(), "cube-shim", "missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, warehouse.ErrNotFound))

	_, err = c.DownloadBlob(context.Background(), "cube-shim", "boom")
	require.Error(t, err)
	assert.True(t, errors.Is(err, warehouse.ErrDownloadFailed))
}

func TestInstallBlobAndScan(t *testing.T) {
	base := t.TempDir()
	var buf bytes.Buffer
	writeShimTar(t, &buf)
	require.NoError(t, warehouse.InstallBlob(context.Background(), base, templatetypes.CubeComponentCubeShim, "v1", &buf, "", 0))
	assert.FileExists(t, filepath.Join(base, "cube-shim", "v1", "bin", "containerd-shim-cube-rs"))
	assert.FileExists(t, filepath.Join(base, "cube-shim", "v1", "bin", "cube-runtime"))
}

func writeShimTar(t *testing.T, w io.Writer) {
	t.Helper()
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, name := range []string{"bin/containerd-shim-cube-rs", "bin/cube-runtime"} {
		body := []byte("x")
		hdr := &tar.Header{Name: name, Mode: 0755, Size: int64(len(body))}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write(body)
		require.NoError(t, err)
	}
}
