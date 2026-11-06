// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package ext4image

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
)

func TestFetchHeaderTicketAndDownload(t *testing.T) {
	const body = "header-ticket-bytes"
	var downloads atomic.Int32
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if r.Method != "GET" || r.Header.Get(artifactticket.DownloadTokenHeader) != "download-token" || r.Header.Get(artifactticket.DownloadNodeIDHeader) != "node-1" || r.Header.Get("X-Cube-Artifact-Refresh-Token") != "" {
			t.Error("download credential envelope is invalid")
		}
		_, _ = w.Write([]byte(body))
	}))
	defer owner.Close()
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Cube-Artifact-Refresh-Token") != "refresh-token" {
			t.Error("Master did not receive refresh authorization")
		}
		_ = json.NewEncoder(w).Encode(artifactticket.Ticket{
			Source: artifactticket.SourceTemplateCenter, Method: "GET", URL: owner.URL,
			Headers:    map[string]string{artifactticket.DownloadTokenHeader: "download-token", artifactticket.DownloadNodeIDHeader: "node-1"},
			ServerTime: time.Now(), ExpiresAt: time.Now().Add(2 * time.Hour), ArtifactID: "artifact-1", Generation: 1, SizeBytes: int64(len(body)),
		})
	}))
	defer master.Close()
	origin, err := artifactticket.URLOrigin(owner.URL)
	if err != nil {
		t.Fatal(err)
	}
	source, err := fetchArtifactDownloadTicket(t.Context(), master.URL, "artifact-1", "node-1", "refresh-token", origin)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/rootfs.download"
	done, err := downloadArtifactAttempt(t.Context(), path, source, int64(len(body)))
	if err != nil || !done {
		t.Fatalf("download done=%v error=%v", done, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body || downloads.Load() != 1 {
		t.Fatalf("download bytes or request count invalid: err=%v count=%d", err, downloads.Load())
	}
}

func TestFetchTicketRejectsCredentialInjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ticket artifactticket.Ticket
	}{
		{"method", artifactticket.Ticket{Source: artifactticket.SourceS3, Method: "POST"}},
		{"S3 token", artifactticket.Ticket{Source: artifactticket.SourceS3, Token: "internal"}},
		{"S3 headers", artifactticket.Ticket{Source: artifactticket.SourceS3, Headers: map[string]string{artifactticket.DownloadTokenHeader: "internal"}}},
		{"TC forbidden header", artifactticket.Ticket{Source: artifactticket.SourceTemplateCenter, Token: "token", Headers: map[string]string{"Authorization": "secret"}}},
		{"TC token conflict", artifactticket.Ticket{Source: artifactticket.SourceTemplateCenter, Token: "old", Headers: map[string]string{artifactticket.DownloadTokenHeader: "new"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket := tc.ticket
			ticket.URL = "https://owner.example/file"
			ticket.ArtifactID, ticket.Generation = "artifact-1", 1
			ticket.ServerTime, ticket.ExpiresAt = time.Now(), time.Now().Add(time.Hour)
			master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(ticket)
			}))
			defer master.Close()
			_, err := fetchArtifactDownloadTicket(t.Context(), master.URL, "artifact-1", "node-1", "refresh", "https://owner.example")
			if err == nil || isRetryableArtifactDownloadError(err) {
				t.Fatalf("malformed credentials must fail permanently: %v", err)
			}
		})
	}
}

func TestArtifactDownloadRejectsCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		_, _ = w.Write([]byte("not authorized"))
	}))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusFound)
	}))
	defer origin.Close()
	_, err := downloadArtifactAttempt(t.Context(), t.TempDir()+"/artifact.download", &resolvedArtifactDownload{
		URL: origin.URL, Header: http.Header{artifactticket.DownloadTokenHeader: []string{"private-token"}},
	}, 14)
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("cross-origin redirect permitted: err=%v leaked=%d", err, leaked.Load())
	}
	_, err = fetchArtifactDownloadTicket(t.Context(), origin.URL, "artifact-1", "node-1", "private-refresh", foreign.URL)
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("ticket redirect permitted: err=%v leaked=%d", err, leaked.Load())
	}
}
