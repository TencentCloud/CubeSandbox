// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestSelectEnvdUploadPayloadRejectsPathWithoutInjectFlag(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--envd-path", "/tmp/envd"})
	payload, err := selectEnvdUploadPayload(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if payload != nil {
		t.Fatalf("payload=%+v, want nil", payload)
	}
	if !strings.Contains(err.Error(), "--envd-path requires --enable-inject-envd") {
		t.Fatalf("error=%q", err.Error())
	}
}

func TestSelectEnvdUploadPayloadUsesLocalEnvdPath(t *testing.T) {
	path := writeCLIEnvdFixture(t, []byte{0x7f, 'E', 'L', 'F', 'o', 'k'})
	ctx := newCreateFromImageContext(t, []string{"--enable-inject-envd", "--envd-path", path})
	payload, err := selectEnvdUploadPayload(ctx)
	if err != nil {
		t.Fatalf("selectEnvdUploadPayload error=%v", err)
	}
	if payload == nil {
		t.Fatal("expected payload")
	}
	if string(payload.Data) != string([]byte{0x7f, 'E', 'L', 'F', 'o', 'k'}) {
		t.Fatalf("payload data=%v", payload.Data)
	}
	if payload.Source != path {
		t.Fatalf("source=%q, want %q", payload.Source, path)
	}
}

func TestBuildCreateFromImageMultipartBodyIncludesRequestAndEnvd(t *testing.T) {
	payload := &envdUploadPayload{Data: []byte{0x7f, 'E', 'L', 'F', 'x'}, Source: "test"}
	req := map[string]string{"source_image_ref": "ubuntu:22.04"}
	body, contentType, err := buildCreateFromImageMultipartBody(req, payload)
	if err != nil {
		t.Fatalf("buildCreateFromImageMultipartBody error=%v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("contentType=%q", contentType)
	}
	reader := multipart.NewReader(body, strings.TrimPrefix(contentType, "multipart/form-data; boundary="))
	form, err := reader.ReadForm(16 * 1024 * 1024)
	if err != nil {
		t.Fatalf("ReadForm error=%v", err)
	}
	if got := form.Value["request"]; len(got) != 1 || !strings.Contains(got[0], "ubuntu:22.04") {
		t.Fatalf("request field=%v", got)
	}
	files := form.File["envd"]
	if len(files) != 1 {
		t.Fatalf("envd files=%d, want 1", len(files))
	}
	f, err := files[0].Open()
	if err != nil {
		t.Fatalf("open envd part: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read envd part: %v", err)
	}
	if string(data) != string(payload.Data) {
		t.Fatalf("envd data=%v", data)
	}
}

func TestSelectEnvdUploadPayloadRejectsInvalidLocalEnvd(t *testing.T) {
	path := writeCLIEnvdFixture(t, []byte("not-elf"))
	ctx := newCreateFromImageContext(t, []string{"--enable-inject-envd", "--envd-path", path})
	payload, err := selectEnvdUploadPayload(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if payload != nil {
		t.Fatalf("payload=%+v, want nil", payload)
	}
	if !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("error=%q", err.Error())
	}
}

func writeCLIEnvdFixture(t *testing.T, data []byte) string {
	t.Helper()
	path := t.TempDir() + "/envd"
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestDoHttpReqWithContentTypeUsesProvidedContentType(t *testing.T) {
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ret":{"retCode":200,"retMsg":"success"}}`))
	}))
	defer server.Close()

	ctx := newCreateFromImageContext(t, nil)
	var rsp struct {
		Ret *types.Ret `json:"ret"`
	}
	err := doHttpReqWithContentType(ctx, server.URL, http.MethodPost, "req-1", strings.NewReader("body"), "multipart/form-data; boundary=test", &rsp)
	if err != nil {
		t.Fatalf("doHttpReqWithContentType error=%v", err)
	}
	if gotContentType != "multipart/form-data; boundary=test" {
		t.Fatalf("Content-Type=%q", gotContentType)
	}
}
