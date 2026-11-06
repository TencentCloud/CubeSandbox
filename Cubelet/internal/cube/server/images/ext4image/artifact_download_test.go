// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package ext4image

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
)

// TestFetchTicketRejectsMasterProxySource pins the direct-only contract: a
// ticket that would make Cubelet stream bytes through CubeMaster is refused
// before any download request, and the failure is fatal (no retry loop).
func TestFetchTicketRejectsMasterProxySource(t *testing.T) {
	var dataRequests atomic.Int32
	const base = "/internal/template/artifacts/artifact-1/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base+"download-ticket" {
			_ = json.NewEncoder(w).Encode(artifactticket.Ticket{Source: "master",
				URL: "http://" + r.Host + base + "ext4", Token: "download-only",
				ExpiresAt: time.Now().Add(time.Hour), ServerTime: time.Now(), ArtifactID: "artifact-1", Generation: 1, SizeBytes: 4})
			return
		}
		dataRequests.Add(1)
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()
	origin, err := artifactticket.URLOrigin(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetchArtifactDownloadTicket(t.Context(), server.URL+base+"download-ticket", "artifact-1", "node-1", "refresh", origin)
	if err == nil || !strings.Contains(err.Error(), "not a direct source") {
		t.Fatalf("error = %v, want direct-source rejection", err)
	}
	if isRetryableArtifactDownloadError(err) {
		t.Fatal("Master-proxied source must be a fatal (non-retryable) error")
	}
	if dataRequests.Load() != 0 {
		t.Fatalf("data requests = %d, want 0", dataRequests.Load())
	}
}

// TestFetchTemplateCenterTicketDownloadsDirectly: a TC-sourced ticket sends
// the per-node token and node id straight to the owning TC.
func TestFetchTemplateCenterTicketDownloadsDirectly(t *testing.T) {
	tc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/artifacts/artifact-1/ext4" ||
			r.Header.Get(artifactticket.DownloadTokenHeader) != "download-only" ||
			r.Header.Get(artifactticket.DownloadNodeIDHeader) != "node-1" ||
			r.Header.Get("X-Cube-Artifact-Refresh-Token") != "" {
			t.Errorf("invalid TC data request: path=%s headers=%v", r.URL.Path, r.Header)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("data"))
	}))
	defer tc.Close()
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(artifactticket.Ticket{Source: artifactticket.SourceTemplateCenter,
			URL: tc.URL + "/internal/artifacts/artifact-1/ext4", Token: "download-only",
			ExpiresAt: time.Now().Add(time.Hour), ServerTime: time.Now(), ArtifactID: "artifact-1", Generation: 1, SizeBytes: 4})
	}))
	defer master.Close()
	origin, err := artifactticket.URLOrigin(tc.URL)
	if err != nil {
		t.Fatal(err)
	}
	source, err := fetchArtifactDownloadTicket(t.Context(), master.URL+"/internal/template/artifacts/artifact-1/download-ticket", "artifact-1", "node-1", "refresh", origin)
	if err != nil {
		t.Fatal(err)
	}
	done, err := downloadArtifactAttempt(t.Context(), t.TempDir()+"/artifact.download", source, 4)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

func TestFetchS3TicketDoesNotForwardInternalHeaders(t *testing.T) {
	const body = "artifact-bytes"
	objectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Cube-Artifact-Token"); got != "" {
			t.Errorf("internal token leaked to S3: %q", got)
		}
		if got := r.Header.Get("X-Cube-Node-ID"); got != "" {
			t.Errorf("node id leaked to S3: %q", got)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	defer objectServer.Close()

	ticketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Cube-Node-ID"); got != "node-1" {
			t.Errorf("ticket node id = %q, want node-1", got)
		}
		if got := r.Header.Get("X-Cube-Artifact-Refresh-Token"); got != "refresh-1" {
			t.Errorf("ticket refresh token = %q", got)
		}
		_ = json.NewEncoder(w).Encode(artifactticket.Ticket{
			Source:     artifactticket.SourceS3,
			URL:        objectServer.URL,
			ExpiresAt:  time.Now().Add(time.Hour),
			ServerTime: time.Now(),
			ArtifactID: "artifact-1",
			Generation: 1,
			SizeBytes:  int64(len(body)),
		})
	}))
	defer ticketServer.Close()

	origin, err := artifactticket.URLOrigin(objectServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	source, err := fetchArtifactDownloadTicket(
		context.Background(), ticketServer.URL, "artifact-1", "node-1", "refresh-1", origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Header) != 0 {
		t.Fatalf("S3 source unexpectedly has internal headers: %v", source.Header)
	}
	tmpPath := t.TempDir() + "/artifact.download"
	done, err := downloadArtifactAttempt(context.Background(), tmpPath, source, int64(len(body)))
	if err != nil || !done {
		t.Fatalf("download = done %v, err %v", done, err)
	}
	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("downloaded body = %q, want %q", got, body)
	}
}

func TestDownloadArtifactRefreshesTicketAndResumes(t *testing.T) {
	data := []byte("0123456789abcdef")
	var requests atomic.Int32
	var sawRange atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		token := r.Header.Get("X-Cube-Artifact-Token")
		switch token {
		case "expired":
			w.WriteHeader(http.StatusUnauthorized)
		case "fresh-2":
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data[:len(data)/2])
		case "fresh-3":
			wantRange := fmt.Sprintf("bytes=%d-", len(data)/2)
			if got := r.Header.Get("Range"); got != wantRange {
				t.Errorf("resume range = %q, want %q", got, wantRange)
			}
			sawRange.Store(true)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", len(data)/2, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[len(data)/2:])
		default:
			t.Errorf("unexpected token %q", token)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()

	var resolutions atomic.Int32
	resolver := func(context.Context) (*resolvedArtifactDownload, error) {
		attempt := resolutions.Add(1)
		header := make(http.Header)
		switch attempt {
		case 1:
			header.Set("X-Cube-Artifact-Token", "expired")
		case 2:
			header.Set("X-Cube-Artifact-Token", "fresh-2")
		default:
			header.Set("X-Cube-Artifact-Token", "fresh-3")
		}
		return &resolvedArtifactDownload{
			URL:       server.URL,
			Header:    header,
			SizeBytes: int64(len(data)),
		}, nil
	}

	tmpPath := t.TempDir() + "/artifact.download"
	_, size, err := downloadArtifactWithResume(context.Background(), tmpPath, resolver, "", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || resolutions.Load() != 3 || requests.Load() != 3 || !sawRange.Load() {
		t.Fatalf("size=%d resolutions=%d requests=%d sawRange=%v", size, resolutions.Load(), requests.Load(), sawRange.Load())
	}
	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("downloaded data = %q, want %q", got, data)
	}
}

func TestFetchArtifactTicketRejectsExpiredOrWrongIdentity(t *testing.T) {
	tests := []struct {
		name   string
		ticket artifactticket.Ticket
	}{
		{
			name: "expired",
			ticket: artifactticket.Ticket{
				Source: artifactticket.SourceS3, URL: "https://objects.example/artifact",
				ExpiresAt: time.Now().Add(-time.Minute), ServerTime: time.Now(),
				ArtifactID: "artifact-1", Generation: 1,
			},
		},
		{
			name: "wrong artifact",
			ticket: artifactticket.Ticket{
				Source: artifactticket.SourceS3, URL: "https://objects.example/artifact",
				ExpiresAt: time.Now().Add(time.Hour), ServerTime: time.Now(),
				ArtifactID: "artifact-2", Generation: 1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tt.ticket)
			}))
			defer server.Close()
			if _, err := fetchArtifactDownloadTicket(
				context.Background(), server.URL, "artifact-1", "node-1", "refresh", "https://objects.example"); err == nil ||
				!strings.Contains(err.Error(), "identity or expiry") {
				t.Fatalf("error = %v, want identity/expiry rejection", err)
			}
		})
	}
}

func TestFetchArtifactTicketRejectsUnexpectedOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(artifactticket.Ticket{
			Source: artifactticket.SourceS3, URL: "https://evil.example/artifact",
			ExpiresAt: time.Now().Add(time.Hour), ServerTime: time.Now(),
			ArtifactID: "artifact-1", Generation: 1,
		})
	}))
	defer server.Close()
	if _, err := fetchArtifactDownloadTicket(
		context.Background(), server.URL, "artifact-1", "node-1", "refresh", "https://objects.example"); err == nil ||
		!strings.Contains(err.Error(), "authorized origin") {
		t.Fatalf("error = %v, want origin rejection", err)
	}
}

func TestDownloadArtifactStopsOnTicketUnauthorized(t *testing.T) {
	var resolutions atomic.Int32
	resolver := func(context.Context) (*resolvedArtifactDownload, error) {
		resolutions.Add(1)
		return nil, &artifactDownloadError{
			err:   fmt.Errorf("artifact ticket status code %d", http.StatusForbidden),
			class: artifactDownloadFatal,
		}
	}
	tmpPath := t.TempDir() + "/artifact.download"
	_, _, err := downloadArtifactWithResume(context.Background(), tmpPath, resolver, "", 8)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v, want ticket 403", err)
	}
	if resolutions.Load() != 1 {
		t.Fatalf("resolutions = %d, want 1 (fatal auth errors must not retry)", resolutions.Load())
	}
}

func TestDownloadArtifactRejectsMismatchedContentRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abcd"))
	}))
	defer server.Close()
	tmpPath := t.TempDir() + "/artifact.download"
	if err := os.WriteFile(tmpPath, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := downloadArtifactAttempt(context.Background(), tmpPath, &resolvedArtifactDownload{URL: server.URL}, 8)
	if err == nil || !strings.Contains(err.Error(), "does not match offset") {
		t.Fatalf("error = %v, want Content-Range offset rejection", err)
	}
}
