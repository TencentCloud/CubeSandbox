// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package tcclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// DeleteArtifact must POST the artifact_id to /tc/api/v1/artifact/delete and
// treat a 200 response as success. This is the piece that was entirely
// missing before: CubeMaster could Submit a build job but had no way to ask
// TC to delete the artifact's S3 object, which is how S3 objects leaked.
func TestDeleteArtifactSuccess(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"deleted","artifact_id":"art-1"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.DeleteArtifact(context.Background(), "art-1"); err != nil {
		t.Fatalf("DeleteArtifact() error = %v, want nil", err)
	}
	if gotPath != "/tc/api/v1/artifact/delete" {
		t.Fatalf("path = %q, want /tc/api/v1/artifact/delete", gotPath)
	}
	if gotBody["artifact_id"] != "art-1" {
		t.Fatalf("body artifact_id = %v, want art-1", gotBody["artifact_id"])
	}
}

// A non-200 response must surface as an error so the caller (Master's
// artifact lifecycle) knows to leave the row CLEANUP_PENDING instead of
// assuming the S3 object/row were removed.
func TestDeleteArtifactNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"tc busy"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.DeleteArtifact(context.Background(), "art-2"); err == nil {
		t.Fatal("DeleteArtifact() error = nil, want non-nil on 503")
	}
}

// Network-level failures (TC unreachable) must also surface as an error, not
// be swallowed as a silent success.
func TestDeleteArtifactUnreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1") // nothing listens here
	if err := c.DeleteArtifact(context.Background(), "art-3"); err == nil {
		t.Fatal("DeleteArtifact() error = nil, want non-nil when TC is unreachable")
	}
}
