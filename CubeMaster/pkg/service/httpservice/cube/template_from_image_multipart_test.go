// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestParseCreateTemplateFromImageRequestPreservesEmptyLifecycleCommand(t *testing.T) {
	for _, hook := range []string{"post_start", "pre_stop"} {
		t.Run(hook, func(t *testing.T) {
			body := `{"source_image_ref":"ubuntu:22.04","writable_layer_size":"1Gi","lifecycle":{"` + hook + `":[]}}`
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			req, _, err := parseCreateTemplateFromImageRequest(r)
			if err != nil {
				t.Fatal(err)
			}
			if req.Lifecycle == nil {
				t.Fatal("explicit empty lifecycle command lost during JSON decoding")
			}
			command, omitted := req.Lifecycle.GetPostStart(), req.Lifecycle.GetPreStop()
			if hook == "pre_stop" {
				command, omitted = omitted, command
			}
			if command == nil || len(command) != 0 || omitted != nil {
				t.Fatalf("expected an explicit empty %s command and an omitted other hook: %v", hook, req.Lifecycle)
			}
		})
	}
}

func TestParseCreateTemplateFromImageRequestRejectsJSONEnvdInjection(t *testing.T) {
	reqBody := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "ubuntu:22.04",
		WritableLayerSize: "20Gi",
		ContainerOverrides: &types.ContainerOverrides{Annotations: map[string]string{
			constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn,
		}},
	}
	raw, err := jsoniter.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", bytes.NewReader(raw))
	httpReq.Header.Set("Content-Type", "application/json")

	parsed, payload, err := parseCreateTemplateFromImageRequest(httpReq)
	if err == nil {
		t.Fatal("expected error")
	}
	if parsed != nil || payload != nil {
		t.Fatalf("parsed=%+v payload=%+v, want nil", parsed, payload)
	}
	if !strings.Contains(err.Error(), "multipart") {
		t.Fatalf("error=%q", err.Error())
	}
}

func TestParseCreateTemplateFromImageRequestMultipartWithEnvd(t *testing.T) {
	reqBody := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "ubuntu:22.04",
		WritableLayerSize: "20Gi",
		ContainerOverrides: &types.ContainerOverrides{Annotations: map[string]string{
			constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn,
		}},
	}
	httpReq := newMultipartTemplateFromImageRequest(t, reqBody, []byte{0x7f, 'E', 'L', 'F', 'o', 'k'})
	parsed, payload, err := parseCreateTemplateFromImageRequest(httpReq)
	if err != nil {
		t.Fatalf("parseCreateTemplateFromImageRequest error=%v", err)
	}
	if parsed == nil || parsed.SourceImageRef != "ubuntu:22.04" {
		t.Fatalf("parsed=%+v", parsed)
	}
	if payload == nil || string(payload.Data) != string([]byte{0x7f, 'E', 'L', 'F', 'o', 'k'}) {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestParseCreateTemplateFromImageRequestMultipartMissingEnvd(t *testing.T) {
	reqBody := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "ubuntu:22.04",
		WritableLayerSize: "20Gi",
		ContainerOverrides: &types.ContainerOverrides{Annotations: map[string]string{
			constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn,
		}},
	}
	httpReq := newMultipartTemplateFromImageRequest(t, reqBody, nil)
	parsed, payload, err := parseCreateTemplateFromImageRequest(httpReq)
	if err == nil {
		t.Fatal("expected error")
	}
	if parsed != nil || payload != nil {
		t.Fatalf("parsed=%+v payload=%+v, want nil", parsed, payload)
	}
	if !strings.Contains(err.Error(), "envd") {
		t.Fatalf("error=%q", err.Error())
	}
}

func TestParseCreateTemplateFromImageRequestMultipartRejectsInvalidEnvd(t *testing.T) {
	reqBody := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "ubuntu:22.04",
		WritableLayerSize: "20Gi",
		ContainerOverrides: &types.ContainerOverrides{Annotations: map[string]string{
			constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn,
		}},
	}
	httpReq := newMultipartTemplateFromImageRequest(t, reqBody, []byte("not-elf"))
	parsed, payload, err := parseCreateTemplateFromImageRequest(httpReq)
	if err == nil {
		t.Fatal("expected error")
	}
	if parsed != nil || payload != nil {
		t.Fatalf("parsed=%+v payload=%+v, want nil", parsed, payload)
	}
	if !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("error=%q", err.Error())
	}
}

func newMultipartTemplateFromImageRequest(t *testing.T, reqBody *types.CreateTemplateFromImageReq, envd []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	raw, err := jsoniter.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := writer.WriteField("request", string(raw)); err != nil {
		t.Fatalf("write request field: %v", err)
	}
	if envd != nil {
		part, err := writer.CreateFormFile("envd", "envd")
		if err != nil {
			t.Fatalf("create envd part: %v", err)
		}
		if _, err := part.Write(envd); err != nil {
			t.Fatalf("write envd part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", body)
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	return httpReq
}
