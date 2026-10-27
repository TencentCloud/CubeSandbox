// SPDX-License-Identifier: Apache-2.0
//

package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/build"
)

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	return r
}

func doArtifactDelete(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := setupRouter()
	r.POST("/tc/api/v1/artifact/delete", handleArtifactDelete)

	var payload []byte
	switch v := body.(type) {
	case string:
		payload = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		payload = b
	}

	req := httptest.NewRequest(http.MethodPost, "/tc/api/v1/artifact/delete", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleArtifactDeleteInvalidJSON(t *testing.T) {
	// Preserve and restore the package-level deleter.
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	w := doArtifactDelete(t, "{not-valid-json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleArtifactDeleteMissingField(t *testing.T) {
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	// artifact_id is binding:required, so an empty body fails validation -> 400.
	w := doArtifactDelete(t, map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing artifact_id, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleArtifactDeleteNoDeleter(t *testing.T) {
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	artifactDeleter = nil

	w := doArtifactDelete(t, map[string]string{"artifact_id": "rfs-x"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when deleter not initialized, got %d body=%s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if resp.Error == "" {
		t.Fatalf("expected error message in body")
	}
}

func TestHandleArtifactDeleteEmptyID(t *testing.T) {
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	// A deleter with nil db: empty artifact_id is rejected by Delete before the
	// db is touched, surfacing as 500 (handler does not special-case it).
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	w := doArtifactDelete(t, map[string]string{"artifact_id": "   "})
	// binding:required passes for a non-empty-but-whitespace string; Delete
	// trims and rejects it -> 500.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for whitespace artifact_id, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleArtifactDeleteNilDB(t *testing.T) {
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	// nil db: a well-formed artifact_id reaches Delete, which fails the nil-db
	// guard -> 500 (documents that the endpoint requires a db-backed deleter).
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	w := doArtifactDelete(t, map[string]string{"artifact_id": "rfs-valid-id"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for nil-db deleter, got %d body=%s", w.Code, w.Body.String())
	}
}

// doInternalDelete routes through RegisterInternalRoutes (the real wiring,
// including the shared-token middleware) rather than the bare handler.
func doInternalDelete(t *testing.T, tokenHeader string) *httptest.ResponseRecorder {
	t.Helper()
	r := setupRouter()
	RegisterInternalRoutes(r.Group(""))

	payload, err := json.Marshal(map[string]string{"artifact_id": "rfs-x"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tc/api/v1/artifact/delete", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if tokenHeader != "" {
		req.Header.Set(constants.TemplateCallbackTokenHeader, tokenHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// With the shared token configured, a request without the header (or with the
// wrong one) must be rejected with 401 before reaching the handler.
func TestInternalAPIRequiresTokenWhenConfigured(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "s3cr3t")
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	if w := doInternalDelete(t, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d body=%s", w.Code, w.Body.String())
	}
	if w := doInternalDelete(t, "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: expected 401, got %d body=%s", w.Code, w.Body.String())
	}
	// Correct token reaches the handler (which then fails on the nil-db
	// deleter -- any status other than 401 proves the gate passed).
	if w := doInternalDelete(t, "s3cr3t"); w.Code == http.StatusUnauthorized {
		t.Fatalf("correct token rejected: got 401 body=%s", w.Body.String())
	}
}

// With no token configured the endpoint fails closed: the chart, one-click
// installer and Terraform all generate the secret on both sides, so an unset
// token is a misconfiguration. Every call — with or without a header — gets
// 503 rather than anonymous access to build submit / artifact delete.
func TestInternalAPIClosedWhenTokenUnset(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	if w := doInternalDelete(t, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("token unset must fail closed: expected 503, got %d body=%s", w.Code, w.Body.String())
	}
	if w := doInternalDelete(t, "anything"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("token unset must fail closed even with a header: expected 503, got %d body=%s", w.Code, w.Body.String())
	}
}

// The explicit dev opt-in (CUBE_TEMPLATE_CALLBACK_INSECURE_NO_TOKEN=true)
// re-opens the endpoint for single-binary local runs — the only mode where
// token-less operation is acceptable.
func TestInternalAPIOpenWithInsecureDevOptIn(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	t.Setenv(constants.TemplateCallbackInsecureNoTokenEnv, "true")
	old := artifactDeleter
	defer func() { artifactDeleter = old }()
	artifactDeleter = build.NewArtifactDeleter(nil, nil)

	if w := doInternalDelete(t, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable {
		t.Fatalf("dev opt-in must pass the gate: got %d body=%s", w.Code, w.Body.String())
	}
}

// TestStoredArtifactFileName verifies TC names an uploaded artifact from the
// TC-controlled artifactID plus the uploaded file's suffix, so an erofs upload
// lands as <id>.erofs (matching BuildErofs) instead of being forced to .ext4.
// The base is always artifactID: a client filename can only change the suffix
// label, never inject a path or alter the directory the file lands in.
func TestStoredArtifactFileName(t *testing.T) {
	cases := []struct {
		artifactID string
		uploaded   string
		want       string
	}{
		{"rfs-1", "rfs-1.erofs", "rfs-1.erofs"},
		{"rfs-1", "rfs-1.ext4", "rfs-1.ext4"},
		{"rfs-1", "rfs-1.EROFS", "rfs-1.erofs"}, // suffix lowercased
		{"rfs-1", "rfs-1", "rfs-1.ext4"},        // no suffix -> legacy default
		{"rfs-1", "rfs-1.tar", "rfs-1.ext4"},    // unknown suffix -> legacy default
		{"rfs-1", "", "rfs-1.ext4"},             // empty -> legacy default
		// A path-like client filename can only contribute its suffix; the base
		// stays the TC-controlled artifactID, never the client's name, so this
		// can neither escape the store dir nor rename the file away from <id>.
		{"rfs-1", "../../etc/passwd.erofs", "rfs-1.erofs"},
		// An artifactID may contain dots (artifactIDShape allows them); the
		// format suffix is still the final extension.
		{"rfs.1", "rfs.1.erofs", "rfs.1.erofs"},
	}
	for _, c := range cases {
		if got := storedArtifactFileName(c.artifactID, c.uploaded); got != c.want {
			t.Fatalf("storedArtifactFileName(%q, %q) = %q, want %q", c.artifactID, c.uploaded, got, c.want)
		}
	}
}

// doArtifactUpload posts a multipart artifact upload to the bare handler
// (bypassing the shared-token middleware, like doArtifactDelete) with the given
// artifact_id and a single "file" part whose filename is filePartName. The store
// root is pinned via CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR so the test does not
// touch the real /data store.
func doArtifactUpload(t *testing.T, storeRoot, artifactID, filePartName string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("artifact_id", artifactID); err != nil {
		t.Fatalf("write artifact_id: %v", err)
	}
	part, err := mw.CreateFormFile("file", filePartName)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write file payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	r := setupRouter()
	r.POST("/tc/api/v1/artifact/upload", handleArtifactUpload)
	req := httptest.NewRequest(http.MethodPost, "/tc/api/v1/artifact/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestHandleArtifactUploadPreservesErofsSuffix is the regression guard for the
// erofs mislabel: an erofs upload must land on disk as <id>.erofs (matching the
// build path), not be forced to <id>.ext4. Before honoring the uploaded
// suffix, handleArtifactUploadFilePart hard-coded .ext4, so an erofs artifact
// migrated to TC was confusingly named <id>.ext4 on the TC node.
func TestHandleArtifactUploadPreservesErofsSuffix(t *testing.T) {
	storeRoot := t.TempDir()
	artifactID := "rfs-upload-erofs"
	w := doArtifactUpload(t, storeRoot, artifactID, artifactID+".erofs", []byte("erofs-bytes"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp ArtifactUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	wantPath := filepath.Join(storeRoot, artifactID, artifactID+".erofs")
	if resp.Ext4Path != wantPath {
		t.Fatalf("ext4_path = %q, want %q (erofs suffix preserved, not forced to .ext4)", resp.Ext4Path, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("erofs artifact not stored at %q: %v", wantPath, err)
	}
	// The suffix choice is exclusive: a legacy .ext4 file must NOT also exist.
	if _, err := os.Stat(filepath.Join(storeRoot, artifactID, artifactID+".ext4")); !os.IsNotExist(err) {
		t.Fatalf("legacy .ext4 file must not be created for an erofs upload, stat err=%v", err)
	}
}

// TestHandleArtifactUploadPreservesExt4Suffix verifies the legacy ext4 path is
// unchanged: an ext4 upload still lands as <id>.ext4 (backward compatible).
func TestHandleArtifactUploadPreservesExt4Suffix(t *testing.T) {
	storeRoot := t.TempDir()
	artifactID := "rfs-upload-ext4"
	w := doArtifactUpload(t, storeRoot, artifactID, artifactID+".ext4", []byte("ext4-bytes"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp ArtifactUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	wantPath := filepath.Join(storeRoot, artifactID, artifactID+".ext4")
	if resp.Ext4Path != wantPath {
		t.Fatalf("ext4_path = %q, want %q", resp.Ext4Path, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("ext4 artifact not stored at %q: %v", wantPath, err)
	}
}

// TestHandleArtifactUploadUnknownSuffixDefaultsToExt4 verifies an unknown (or
// missing) suffix falls back to the historical .ext4 storage name, so a legacy
// or odd upload keeps landing where it always did.
func TestHandleArtifactUploadUnknownSuffixDefaultsToExt4(t *testing.T) {
	storeRoot := t.TempDir()
	artifactID := "rfs-upload-unknown"
	w := doArtifactUpload(t, storeRoot, artifactID, artifactID+".tar", []byte("tar-bytes"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp ArtifactUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	wantPath := filepath.Join(storeRoot, artifactID, artifactID+".ext4")
	if resp.Ext4Path != wantPath {
		t.Fatalf("ext4_path = %q, want %q (unknown suffix falls back to .ext4)", resp.Ext4Path, wantPath)
	}
}
