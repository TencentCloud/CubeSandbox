// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"encoding/json"
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

func TestScanAndReport_SkipUnchangedAndDropDeleted(t *testing.T) {
	var puts int32
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/inventory") {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&puts, 1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		last.Store(append([]byte(nil), body...))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := t.TempDir()
	writeValidShim(t, base, "vA")
	writeValidShim(t, base, "vB")

	f := NewFetcher(NewClient(srv.URL, "node-1", "amd64", time.Minute), base)
	f.ScanAndReport(context.Background())
	require.Equal(t, int32(1), atomic.LoadInt32(&puts))
	assert.ElementsMatch(t, []string{"vA", "vB"}, inventoryVersions(t, last.Load().([]byte), "cube-shim"))

	f.ScanAndReport(context.Background())
	assert.Equal(t, int32(1), atomic.LoadInt32(&puts), "unchanged snapshot should skip PUT")

	require.NoError(t, os.RemoveAll(filepath.Join(base, "cube-shim", "vB")))
	f.ScanAndReport(context.Background())
	require.Equal(t, int32(2), atomic.LoadInt32(&puts))
	assert.ElementsMatch(t, []string{"vA"}, inventoryVersions(t, last.Load().([]byte), "cube-shim"))
}

func TestScanAndReport_FetchWaitsAndKeepsNewVersion(t *testing.T) {
	putStarted := make(chan struct{})
	releasePut := make(chan struct{})
	var puts int32
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/internal/warehouse/blob"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url": srvURL(r) + "/s3/blob", "expiresIn": 300, "sizeBytes": 0, "checksum": "",
			})
		case strings.Contains(r.URL.Path, "/s3/"):
			writeShimTar(t, w)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/inventory"):
			n := atomic.AddInt32(&puts, 1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			last.Store(append([]byte(nil), body...))
			if n == 1 {
				close(putStarted)
				<-releasePut
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base := t.TempDir()
	writeValidShim(t, base, "vA")
	f := NewFetcher(NewClient(srv.URL, "node-1", "amd64", time.Minute), base)

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		f.ScanAndReport(context.Background())
	}()
	select {
	case <-putStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("scan PUT did not start")
	}

	fetchErr := make(chan error, 1)
	go func() {
		fetchErr <- f.Fetch(context.Background(), templatetypes.CubeComponentCubeShim, "vB")
	}()
	time.Sleep(50 * time.Millisecond)
	close(releasePut)
	<-scanDone
	require.NoError(t, <-fetchErr)
	require.GreaterOrEqual(t, atomic.LoadInt32(&puts), int32(2))
	assert.ElementsMatch(t, []string{"vA", "vB"}, inventoryVersions(t, last.Load().([]byte), "cube-shim"))
}

func TestFetcher_InventoryPutFailureStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/internal/warehouse/blob"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url": srvURL(r) + "/s3/blob", "expiresIn": 300, "sizeBytes": 0, "checksum": "",
			})
		case strings.Contains(r.URL.Path, "/s3/"):
			writeShimTar(t, w)
		case r.Method == http.MethodPut:
			http.Error(w, "nope", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base := t.TempDir()
	f := NewFetcher(NewClient(srv.URL, "node-1", "amd64", time.Minute), base)
	err := f.Fetch(context.Background(), templatetypes.CubeComponentCubeShim, "v1")
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(base, "cube-shim", "v1", "bin", "containerd-shim-cube-rs"))
}

func TestAckJob_ExpiredParentContextStillSends(t *testing.T) {
	var gotStatus atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/ack") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotStatus.Store(body.Status)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "node-1", "amd64", time.Minute)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, client.AckJob(expired, "job-1", "failed", "deadline"))
	require.Nil(t, gotStatus.Load())

	ackJob(client, "job-1", "failed", "deadline")
	require.Equal(t, "failed", gotStatus.Load())
}

func writeValidShim(t *testing.T, base, version string) {
	t.Helper()
	dir := filepath.Join(base, "cube-shim", version, "bin")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "containerd-shim-cube-rs"), []byte("shim"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cube-runtime"), []byte("rt"), 0o755))
}

func inventoryVersions(t *testing.T, raw []byte, component string) []string {
	t.Helper()
	var wrap struct {
		Items []InventoryItem `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw, &wrap))
	var out []string
	for _, it := range wrap.Items {
		if it.Component == component {
			out = append(out, it.Version)
		}
	}
	return out
}

func srvURL(r *http.Request) string {
	return "http://" + r.Host
}
