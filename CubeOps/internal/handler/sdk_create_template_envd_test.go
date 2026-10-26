// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// doCreateTemplateMultipart issues a multipart/form-data POST to the create
// template endpoint with a "request" JSON part and (optionally) an "envd" file
// part. When envd is nil the file part is omitted.
func doCreateTemplateMultipart(t *testing.T, r *gin.Engine, requestJSON string, envd []byte) *httptest.ResponseRecorder {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	reqPart, err := w.CreateFormField("request")
	if err != nil {
		t.Fatalf("create request part: %v", err)
	}
	if _, err := reqPart.Write([]byte(requestJSON)); err != nil {
		t.Fatalf("write request part: %v", err)
	}
	if envd != nil {
		filePart, err := w.CreateFormFile("envd", "envd")
		if err != nil {
			t.Fatalf("create envd part: %v", err)
		}
		if _, err := filePart.Write(envd); err != nil {
			t.Fatalf("write envd part: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/v1/sdk/templates", buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// fakeELF returns a minimal byte slice that passes CubeMaster's ELF magic check
// (0x7f 'E' 'L' 'F'), padded to the requested length.
func fakeELF(n int) []byte {
	b := make([]byte, n)
	if n >= 4 {
		b[0], b[1], b[2], b[3] = 0x7f, 'E', 'L', 'F'
	}
	return b
}

// (a) a multipart request routes to CreateTemplateFromImageMultipart, and
// (b) the forwarded body carries the inject_envd annotation, and
// (c) the envd bytes are passed through unchanged.
func TestSDK_CreateTemplate_Multipart_InjectsAnnotationAndEnvd(t *testing.T) {
	envd := fakeELF(1024)
	var gotBody interface{}
	var gotEnvd []byte
	jsonCalled := false
	cm := &fakeCM{
		createTemplateFromImageMultipart: func(_ context.Context, body interface{}, b []byte) (json.RawMessage, error) {
			gotBody = body
			gotEnvd = b
			return raw(`{"ret": {"ret_code": 0}, "data": {"job": {"job_id": "j1"}}}`), nil
		},
		createTemplateFromImage: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			jsonCalled = true
			return nil, errFakeNotConfigured
		},
	}
	r := newSDKRouter(t, cm)

	rec := doCreateTemplateMultipart(t, r, `{"image":"reg/foo:1","enableInjectEnvd":true}`, envd)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if jsonCalled {
		t.Fatal("JSON path CreateTemplateFromImage was called for a multipart request")
	}
	if !bytes.Equal(gotEnvd, envd) {
		t.Errorf("forwarded envd differs from upload: got %d bytes, want %d", len(gotEnvd), len(envd))
	}
	// Verify container_overrides.annotations["cube.master.inject_envd"] == "true".
	m, ok := gotBody.(map[string]interface{})
	if !ok {
		t.Fatalf("forwarded body is %T, want map", gotBody)
	}
	co, ok := m["container_overrides"].(map[string]interface{})
	if !ok {
		t.Fatalf("container_overrides missing/wrong type: %#v", m["container_overrides"])
	}
	anns, ok := co["annotations"].(map[string]interface{})
	if !ok {
		t.Fatalf("annotations missing/wrong type: %#v", co["annotations"])
	}
	if anns["cube.master.inject_envd"] != "true" {
		t.Errorf("inject_envd annotation = %v, want \"true\"", anns["cube.master.inject_envd"])
	}
}

// (d) missing image in the "request" part → 400.
func TestSDK_CreateTemplate_Multipart_MissingImage(t *testing.T) {
	cm := &fakeCM{
		createTemplateFromImageMultipart: func(_ context.Context, _ interface{}, _ []byte) (json.RawMessage, error) {
			t.Fatal("client should not be called when image is missing")
			return nil, nil
		},
	}
	r := newSDKRouter(t, cm)

	rec := doCreateTemplateMultipart(t, r, `{"enableInjectEnvd":true}`, fakeELF(16))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (image required); body=%s", rec.Code, rec.Body.String())
	}
}

// (e) an oversized envd → 413.
func TestSDK_CreateTemplate_Multipart_EnvdTooLarge(t *testing.T) {
	cm := &fakeCM{
		createTemplateFromImageMultipart: func(_ context.Context, _ interface{}, _ []byte) (json.RawMessage, error) {
			t.Fatal("client should not be called for oversized envd")
			return nil, nil
		},
	}
	r := newSDKRouter(t, cm)

	rec := doCreateTemplateMultipart(t, r, `{"image":"reg/foo:1","enableInjectEnvd":true}`, fakeELF(maxEnvdPayloadBytes+1))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

// missing envd file part → 400.
func TestSDK_CreateTemplate_Multipart_MissingEnvdFile(t *testing.T) {
	cm := &fakeCM{
		createTemplateFromImageMultipart: func(_ context.Context, _ interface{}, _ []byte) (json.RawMessage, error) {
			t.Fatal("client should not be called when envd file is absent")
			return nil, nil
		},
	}
	r := newSDKRouter(t, cm)

	rec := doCreateTemplateMultipart(t, r, `{"image":"reg/foo:1"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (envd file required); body=%s", rec.Code, rec.Body.String())
	}
}

// (f) a plain JSON request still hits the JSON client method, not multipart —
// guards the no-regression invariant.
func TestSDK_CreateTemplate_JSON_StillUsesJSONPath(t *testing.T) {
	jsonCalled := false
	cm := &fakeCM{
		createTemplateFromImage: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			jsonCalled = true
			return raw(`{"ret": {"ret_code": 0}, "data": {"job": {"job_id": "j1"}}}`), nil
		},
		createTemplateFromImageMultipart: func(_ context.Context, _ interface{}, _ []byte) (json.RawMessage, error) {
			t.Fatal("multipart path called for a plain JSON request")
			return nil, nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/templates", `{"image":"reg/foo:1"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !jsonCalled {
		t.Error("JSON path CreateTemplateFromImage was not called")
	}
}
