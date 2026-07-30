// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/webhook"
)

func TestServer_RegistersUnauthenticatedWebhookIngress(t *testing.T) {
	webhookConfig, err := webhook.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	runtime := webhook.NewRuntime(webhookConfig)
	srv := New(&config.Config{}, nil, WithWebhookRuntime(runtime))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/webhook/events/batch", nil)

	srv.buildRouter().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want unauthenticated handler response 400", recorder.Code)
	}
}
