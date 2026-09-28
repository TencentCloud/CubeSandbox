// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

func TestFetcher_ConcurrentFetchDownloadsOnce(t *testing.T) {
	var blobs int32
	release := make(chan struct{})
	started := make(chan struct{})
	srv := newWarehouseS3Server(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&blobs, 1) == 1 {
			close(started)
		}
		<-release
		writeShimTar(t, w)
	})
	defer srv.Close()

	base := t.TempDir()
	f := NewFetcher(NewClient(srv.URL, "node-1", "amd64", time.Minute), base)

	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			errCh <- f.Fetch(context.Background(), templatetypes.CubeComponentCubeShim, "3.0.0")
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
	assert.FileExists(t, filepath.Join(base, "cube-shim", "3.0.0", "bin", "containerd-shim-cube-rs"))
}

func TestFetcher_SkipHTTPWhenDestDirExists(t *testing.T) {
	var blobs int32
	srv := newWarehouseS3Server(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&blobs, 1)
		http.Error(w, "should not download", http.StatusInternalServerError)
	})
	defer srv.Close()

	base := t.TempDir()
	dir := filepath.Join(base, "cube-shim", "1.0.0", "bin")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "containerd-shim-cube-rs"), []byte("shim"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cube-runtime"), []byte("rt"), 0o755))

	f := NewFetcher(NewClient(srv.URL, "node-1", "amd64", time.Second), base)
	require.NoError(t, f.Fetch(context.Background(), templatetypes.CubeComponentCubeShim, "1.0.0"))
	assert.Equal(t, int32(0), atomic.LoadInt32(&blobs))
}

func TestFetcher_CallerCancelDoesNotAbortInflight(t *testing.T) {
	var blobs int32
	started := make(chan struct{})
	release := make(chan struct{})
	srv := newWarehouseS3Server(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&blobs, 1)
		close(started)
		<-release
		writeShimTar(t, w)
	})
	defer srv.Close()

	base := t.TempDir()
	f := NewFetcher(NewClient(srv.URL, "node-1", "amd64", time.Minute), base)

	ensureCtx, cancel := context.WithCancel(context.Background())
	ensureErr := make(chan error, 1)
	preinstallErr := make(chan error, 1)
	go func() {
		ensureErr <- f.Fetch(ensureCtx, templatetypes.CubeComponentCubeShim, "ctx")
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}
	go func() {
		preinstallErr <- f.Fetch(context.Background(), templatetypes.CubeComponentCubeShim, "ctx")
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.ErrorIs(t, <-ensureErr, context.Canceled)

	close(release)
	require.NoError(t, <-preinstallErr)
	assert.Equal(t, int32(1), atomic.LoadInt32(&blobs))
	assert.FileExists(t, filepath.Join(base, "cube-shim", "ctx", "bin", "containerd-shim-cube-rs"))
}

func TestInstallBlobChecksumMismatch(t *testing.T) {
	var buf bytes.Buffer
	writeShimTar(t, &buf)
	err := InstallBlob(context.Background(), t.TempDir(), templatetypes.CubeComponentCubeShim, "v1", bytes.NewReader(buf.Bytes()), "sha256:deadbeef", int64(buf.Len()))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDownloadFailed)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

func TestInstallBlobChecksumOK(t *testing.T) {
	var buf bytes.Buffer
	writeShimTar(t, &buf)
	sum := sha256.Sum256(buf.Bytes())
	base := t.TempDir()
	require.NoError(t, InstallBlob(context.Background(), base, templatetypes.CubeComponentCubeShim, "v1", bytes.NewReader(buf.Bytes()), "sha256:"+hex.EncodeToString(sum[:]), int64(buf.Len())))
	assert.FileExists(t, filepath.Join(base, "cube-shim", "v1", "bin", "containerd-shim-cube-rs"))
}

func TestOpenBlobRejectsCrossHostRedirect(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("stolen"))
	}))
	defer evil.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/object", http.StatusFound)
	}))
	defer src.Close()
	c := NewClient("http://cube-ops.example", "node-1", "amd64", time.Second)
	_, err := c.OpenBlob(context.Background(), &BlobRef{URL: src.URL + "/bucket/key"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disallowed host")
}

func TestPreinstallJobBudgetFollowsClientTimeout(t *testing.T) {
	c := NewClient("http://cube-ops.example", "node-1", "amd64", 20*time.Minute)
	assert.Equal(t, 22*time.Minute, preinstallJobBudget(c))
	assert.Equal(t, 12*time.Minute, preinstallJobBudget(nil))
}

func TestOpenBlobSanitizesURLFromError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `<Error><Code>AccessDenied</Code><Message>nope</Message></Error>`, http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "node-1", "amd64", time.Second)
	signed := srv.URL + "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE%2F20260101&X-Amz-Signature=deadbeefcafebabe"
	_, err := c.OpenBlob(context.Background(), &BlobRef{URL: signed})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "X-Amz-Signature")
	assert.NotContains(t, err.Error(), "deadbeefcafebabe")
	assert.Contains(t, err.Error(), "AccessDenied")
}

func TestResolveBlobObjectMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"gone","code":"warehouse_not_found"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "node-1", "amd64", time.Second)
	_, err := c.ResolveBlob(context.Background(), "cube-shim", "missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
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

// newWarehouseS3Server is one httptest server that plays CubeOps (JSON ticket)
// and S3 (the bytes behind the ticket). onObject is the presigned GET handler.
func newWarehouseS3Server(t *testing.T, onObject http.HandlerFunc) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/internal/warehouse/blob"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url":       srv.URL + "/s3/blob",
				"expiresIn": 300,
				"sizeBytes": 0,
				"checksum":  "",
			})
		case strings.Contains(r.URL.Path, "/s3/"):
			onObject(w, r)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/inventory"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}
