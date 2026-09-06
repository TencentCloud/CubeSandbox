// SPDX-License-Identifier: Apache-2.0
//

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
