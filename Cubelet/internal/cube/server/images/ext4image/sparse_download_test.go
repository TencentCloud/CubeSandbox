// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package ext4image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func TestDownloadSparseArtifactValidatesHashAndCleansFailures(t *testing.T) {
	data := make([]byte, 2*1024*1024+17)
	copy(data[65536:], []byte("rootfs header"))
	sum := sha256.Sum256(data)
	for _, mode := range []string{"success", "hash mismatch", "interrupted", "cancelled", "short body", "oversized body"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "interrupted" {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)+1))
				}
				if mode == "short body" || mode == "oversized body" {
					w.(http.Flusher).Flush()
				}
				if mode == "short body" {
					_, _ = w.Write(data[:len(data)-1])
					return
				}
				_, _ = w.Write(data)
				if mode == "oversized body" {
					_, _ = w.Write([]byte{1})
				}
			}))
			defer server.Close()
			expected := hex.EncodeToString(sum[:])
			if mode == "hash mismatch" {
				expected = "invalid"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			path := filepath.Join(t.TempDir(), "image.ext4")
			err := tryDownloadPmemFile(ctx, "cubebox", path, &cubeimages.ImageSpec{Annotations: map[string]string{
				constants.MasterAnnotationRootfsArtifactURL:       server.URL,
				constants.MasterAnnotationRootfsArtifactSHA256:    expected,
				constants.MasterAnnotationRootfsArtifactSizeBytes: fmt.Sprint(len(data)),
			}})
			if (err == nil) != (mode == "success") {
				t.Fatalf("download error=%v", err)
			}
			if (mode == "short body" || mode == "oversized body") && (err == nil || err.Error() != "artifact size mismatch") {
				t.Fatalf("size validation lost: %v", err)
			}
			if _, err := os.Stat(path + ".download"); !os.IsNotExist(err) {
				t.Fatalf("temporary artifact retained: %v", err)
			}
			if mode == "success" {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("downloaded content changed: %v", err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Blocks*512 >= int64(len(data))/2 {
					t.Fatalf("download was densely allocated: %d bytes", stat.Blocks*512)
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("failed artifact was published: %v", err)
			}
		})
	}
}
