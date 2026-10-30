// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

// Regression tests for tryDownloadPmemFile resume support: an interrupted
// download must be resumed via HTTP Range instead of restarting from zero.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func setupResumeProbe(t *testing.T) (context.Context, string) {
	t.Helper()
	paths, err := pmem.ResolvePaths(t.TempDir(), filepath.Join(t.TempDir(), "images"), filepath.Join(t.TempDir(), "vmlinux"))
	if err != nil {
		t.Fatal(err)
	}
	previous := pmem.CurrentPaths()
	pmem.InitPaths(paths)
	t.Cleanup(func() { pmem.InitPaths(previous) })
	return context.Background(), pmem.GetRawImageFilePath("cubebox", "rfs-resume")
}

func makeSpec(url, sha string) context.Context {
	return constants.WithImageSpec(context.Background(), &cubeimages.ImageSpec{
		Annotations: map[string]string{
			constants.MasterAnnotationRootfsArtifactURL:    url,
			constants.MasterAnnotationRootfsArtifactSHA256: sha,
		},
	})
}

func readHeaderLog(t *testing.T, mu *sync.Mutex, ranges *[]string) []string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), *ranges...)
}

func TestTryDownloadPmemFileResumesAfterInterruption(t *testing.T) {
	ctx, imagePath := setupResumeProbe(t)
	full := bytes.Repeat([]byte("R"), 4096)
	sum := sha256.Sum256(full)
	wantSHA := hex.EncodeToString(sum[:])

	var mu sync.Mutex
	var ranges []string
	call := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		call++
		n := call
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write(full[:2048])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler) // simulate a mid-transfer disconnect
		}
		http.ServeContent(w, r, "artifact.ext4", time.Now(), bytes.NewReader(full))
	}))
	defer server.Close()

	spec := constants.GetImageSpec(makeSpec(server.URL, wantSHA))

	// First attempt: server cuts the connection mid-transfer.
	if err := tryDownloadPmemFile(ctx, imagePath, spec); err == nil {
		t.Fatal("first attempt should fail when the connection is aborted")
	}
	st, err := os.Stat(imagePath + ".download")
	if err != nil {
		t.Fatalf("partial .download should survive a failed attempt: %v", err)
	}
	if st.Size() != 2048 {
		t.Fatalf("partial size = %d, want 2048", st.Size())
	}

	// Second attempt: must resume from the partial via a Range request.
	if err := tryDownloadPmemFile(ctx, imagePath, spec); err != nil {
		t.Fatalf("resumed download failed: %v", err)
	}
	headers := readHeaderLog(t, &mu, &ranges)
	if len(headers) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(headers))
	}
	if headers[0] != "" {
		t.Fatalf("first request should not send Range, got %q", headers[0])
	}
	if headers[1] != "bytes=2048-" {
		t.Fatalf("second request should resume with Range, got %q", headers[1])
	}
	got, err := os.ReadFile(imagePath)
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("final file mismatch: err=%v len=%d", err, len(got))
	}
	if _, err := os.Stat(imagePath + ".download"); !os.IsNotExist(err) {
		t.Fatalf("tmp file should be gone after rename: %v", err)
	}
}

func TestTryDownloadPmemFileFallsBackWhenServerIgnoresRange(t *testing.T) {
	ctx, imagePath := setupResumeProbe(t)
	full := bytes.Repeat([]byte("F"), 4096)
	sum := sha256.Sum256(full)
	wantSHA := hex.EncodeToString(sum[:])

	var mu sync.Mutex
	var ranges []string

	// Server always answers with the full body (200), ignoring Range.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		_, _ = w.Write(full)
	}))
	defer server.Close()

	// A stale partial that must be discarded on the 200 fallback.
	if err := os.MkdirAll(filepath.Dir(imagePath+".download"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath+".download", bytes.Repeat([]byte("Z"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := constants.GetImageSpec(makeSpec(server.URL, wantSHA))
	if err := tryDownloadPmemFile(ctx, imagePath, spec); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	headers := readHeaderLog(t, &mu, &ranges)
	if len(headers) != 1 || headers[0] == "" {
		t.Fatalf("Range should still be attempted, headers=%v", headers)
	}
	got, err := os.ReadFile(imagePath)
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("final file must be the full content: err=%v len=%d", err, len(got))
	}
}

func TestTryDownloadPmemFileVerifiesCompletePartialOn416(t *testing.T) {
	ctx, imagePath := setupResumeProbe(t)
	full := bytes.Repeat([]byte("C"), 4096)
	sum := sha256.Sum256(full)
	wantSHA := hex.EncodeToString(sum[:])

	transfers := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-", len(full)) {
			// Partial already reaches EOF: previous attempt died right
			// before the rename.
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		transfers++
		_, _ = w.Write(full)
	}))
	defer server.Close()

	if err := os.MkdirAll(filepath.Dir(imagePath+".download"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath+".download", full, 0o644); err != nil {
		t.Fatal(err)
	}

	spec := constants.GetImageSpec(makeSpec(server.URL, wantSHA))
	if err := tryDownloadPmemFile(ctx, imagePath, spec); err != nil {
		t.Fatalf("416 verify-only path failed: %v", err)
	}
	if transfers != 0 {
		t.Fatalf("no body transfer should happen on the 416 path")
	}
	got, err := os.ReadFile(imagePath)
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("final file mismatch: err=%v len=%d", err, len(got))
	}
}

func TestTryDownloadPmemFileDropsCorruptPartialOnMismatch(t *testing.T) {
	ctx, imagePath := setupResumeProbe(t)
	full := bytes.Repeat([]byte("C"), 4096)
	sum := sha256.Sum256(full)
	wantSHA := hex.EncodeToString(sum[:])

	// The partial already reaches EOF (416 verify-only) but its content is
	// corrupt: the digest gate must reject it, drop the partial, and leave
	// no final file behind.
	corrupt := append([]byte(nil), full...)
	corrupt[0] ^= 0xFF

	transfers := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-", len(corrupt)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		transfers++
		_, _ = w.Write(full)
	}))
	defer server.Close()

	if err := os.MkdirAll(filepath.Dir(imagePath+".download"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath+".download", corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	spec := constants.GetImageSpec(makeSpec(server.URL, wantSHA))
	err := tryDownloadPmemFile(ctx, imagePath, spec)
	if err == nil {
		t.Fatal("corrupt partial must fail the SHA256 gate")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("failure should come from the digest gate, got %v", err)
	}
	if _, statErr := os.Stat(imagePath + ".download"); !os.IsNotExist(statErr) {
		t.Fatalf("corrupt partial should be dropped: %v", statErr)
	}
	if _, statErr := os.Stat(imagePath); !os.IsNotExist(statErr) {
		t.Fatalf("no final file should be published from a corrupt partial: %v", statErr)
	}
	if transfers != 0 {
		t.Fatalf("no body transfer should happen on the 416 path")
	}
}

func TestTryDownloadPmemFileResumesWithSparseHole(t *testing.T) {
	ctx, imagePath := setupResumeProbe(t)

	// payload: 1KB data + 1MiB zeros + 1KB data. The 1MiB zero run equals
	// sparseHoleThreshold -- the smallest run sparseCopy's zero-seek will
	// skip -- so this is the only payload shape that exercises resume and
	// sparse writing together. Without this, an implementation that
	// materializes zeros (e.g. O_APPEND, which defeats sparseCopy's
	// relative seeks) passes every other test silently.
	head := bytes.Repeat([]byte("A"), 1024)
	tail := bytes.Repeat([]byte("B"), 1024)
	full := append(append(append([]byte(nil), head...), make([]byte, 1024*1024)...), tail...)
	sum := sha256.Sum256(full)
	wantSHA := hex.EncodeToString(sum[:])

	// Skip the sparse assertion where the filesystem cannot hold holes.
	probe, err := os.CreateTemp(t.TempDir(), "sparse-probe")
	if err != nil {
		t.Fatal(err)
	}
	probeName := probe.Name()
	_, _ = probe.Write([]byte("x"))
	if _, err := probe.Seek(1024*1024, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_, _ = probe.Write([]byte("y"))
	_ = probe.Close()
	if st, statErr := os.Stat(probeName); statErr == nil {
		if blocks, ok := st.Sys().(*syscall.Stat_t); ok && blocks.Blocks*512 >= st.Size() {
			t.Skip("filesystem does not support sparse files; sparse-write assertion would be meaningless")
		}
	}

	var mu sync.Mutex
	var ranges []string
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		call++
		n := call
		mu.Unlock()
		if n == 1 {
			// Cut mid-hole: the connection dies inside the 1MiB zero run.
			w.Header().Set("Content-Length", fmt.Sprint(len(full)))
			_, _ = w.Write(full[:len(head)+512*1024])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "artifact.ext4", time.Now(), bytes.NewReader(full))
	}))
	defer server.Close()

	spec := constants.GetImageSpec(makeSpec(server.URL, wantSHA))

	// attempt 1: dies mid-hole. The on-disk prefix can be shorter than the
	// bytes actually transferred (pending zero runs are not flushed on
	// error) -- fine, because the missing region is all zeros and the
	// retry re-fetches it.
	if err := tryDownloadPmemFile(ctx, imagePath, spec); err == nil {
		t.Fatal("first attempt should fail on the aborted connection")
	}
	st, err := os.Stat(imagePath + ".download")
	if err != nil {
		t.Fatalf("partial .download should survive: %v", err)
	}

	// attempt 2: resume from the physical prefix.
	if err := tryDownloadPmemFile(ctx, imagePath, spec); err != nil {
		t.Fatalf("resumed download failed: %v", err)
	}
	headers := readHeaderLog(t, &mu, &ranges)
	if len(headers) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(headers))
	}
	// The resume offset must equal the physical prefix size: whatever the
	// zero-flush policy is, the Range header and the partial on disk must
	// agree -- a mismatch means re-fetched bytes land at the wrong offset.
	if headers[1] != fmt.Sprintf("bytes=%d-", st.Size()) {
		t.Fatalf("resume must continue from the physical prefix: got %q, partial size %d", headers[1], st.Size())
	}

	got, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("final file mismatch: got %d bytes, want %d", len(got), len(full))
	}
	if _, statErr := os.Stat(imagePath + ".download"); !os.IsNotExist(statErr) {
		t.Fatalf("tmp file should be gone after rename: %v", statErr)
	}

	// Sparse assertion: the 1MiB zero run must be a hole, not materialized
	// zeros. An implementation that writes the zeros out (O_APPEND-style)
	// fills the blocks and fails here.
	fst, err := os.Stat(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if blocks, ok := fst.Sys().(*syscall.Stat_t); ok && blocks.Blocks*512 >= fst.Size() {
		t.Fatalf("1MiB zero run should be a sparse hole: blocks*512=%d, size=%d", blocks.Blocks*512, fst.Size())
	}
}
