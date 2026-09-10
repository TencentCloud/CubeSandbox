package tcclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

// DeleteArtifact must POST the artifact_id to /tc/api/v1/artifact/delete and
// treat a 200 response as success.
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

// When the shared token env is set, every call to TC must carry it in the
// shared-token header so TC's auth middleware accepts the request; when the
// env is empty the header must be absent (matching a token-less TC).
func TestSharedTokenHeader(t *testing.T) {
	var gotDelete, gotSubmit, gotUpload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tc/api/v1/artifact/delete":
			gotDelete = r.Header.Get(constants.TemplateCallbackTokenHeader)
		case "/tc/api/v1/build":
			gotSubmit = r.Header.Get(constants.TemplateCallbackTokenHeader)
		case "/tc/api/v1/artifact/upload":
			gotUpload = r.Header.Get(constants.TemplateCallbackTokenHeader)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(UploadArtifactResponse{Status: "uploaded", ArtifactID: "art-1"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	artifactPath := filepath.Join(t.TempDir(), "art-1.ext4")
	if err := os.WriteFile(artifactPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write artifact file: %v", err)
	}

	t.Setenv(constants.TemplateCallbackTokenEnv, "s3cr3t")
	c := NewClient(srv.URL)
	if err := c.DeleteArtifact(context.Background(), "art-1"); err != nil {
		t.Fatalf("DeleteArtifact() error = %v", err)
	}
	if err := c.SubmitBuildJob(context.Background(), "job-1", nil, "", "", nil); err != nil {
		t.Fatalf("SubmitBuildJob() error = %v", err)
	}
	if _, err := c.UploadArtifact(context.Background(), "art-1", artifactPath); err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	if gotDelete != "s3cr3t" || gotSubmit != "s3cr3t" || gotUpload != "s3cr3t" {
		t.Fatalf("token header = %q/%q/%q, want s3cr3t on all three", gotDelete, gotSubmit, gotUpload)
	}

	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	gotDelete, gotSubmit, gotUpload = "", "", ""
	if err := c.DeleteArtifact(context.Background(), "art-1"); err != nil {
		t.Fatalf("DeleteArtifact() error = %v", err)
	}
	if err := c.SubmitBuildJob(context.Background(), "job-1", nil, "", "", nil); err != nil {
		t.Fatalf("SubmitBuildJob() error = %v", err)
	}
	if _, err := c.UploadArtifact(context.Background(), "art-1", artifactPath); err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	if gotDelete != "" || gotSubmit != "" || gotUpload != "" {
		t.Fatalf("token header must be absent when env unset, got %q/%q/%q", gotDelete, gotSubmit, gotUpload)
	}
}

func TestUploadArtifactStreamsMultipart(t *testing.T) {
	tmpDir := t.TempDir()
	artifactPath := filepath.Join(tmpDir, "rfs-test.ext4")
	payload := []byte("test-ext4-content")
	if err := os.WriteFile(artifactPath, payload, 0o644); err != nil {
		t.Fatalf("write artifact file: %v", err)
	}

	var gotArtifactID string
	var gotFileBytes []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tc/api/v1/artifact/upload" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		gotArtifactID = r.FormValue("artifact_id")
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer f.Close()
		gotFileBytes, err = io.ReadAll(f)
		if err != nil {
			t.Fatalf("read uploaded file: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(UploadArtifactResponse{
			Status:        "uploaded",
			ArtifactID:    gotArtifactID,
			Ext4Path:      artifactPath,
			Ext4SHA256:    "sha256",
			Ext4SizeBytes: int64(len(gotFileBytes)),
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	resp, err := client.UploadArtifact(context.Background(), "rfs-test", artifactPath)
	if err != nil {
		t.Fatalf("UploadArtifact failed: %v", err)
	}
	if gotArtifactID != "rfs-test" {
		t.Fatalf("artifact_id=%q, want rfs-test", gotArtifactID)
	}
	if string(gotFileBytes) != string(payload) {
		t.Fatalf("uploaded bytes=%q, want %q", string(gotFileBytes), string(payload))
	}
	if resp.Ext4SizeBytes != int64(len(payload)) {
		t.Fatalf("resp.Ext4SizeBytes=%d, want %d", resp.Ext4SizeBytes, len(payload))
	}
}
