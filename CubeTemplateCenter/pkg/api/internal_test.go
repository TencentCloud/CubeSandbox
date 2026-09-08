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
