// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

const storeErrorDetail = "dial tcp 10.0.0.5:3306: connect: connection refused"

func serveServiceError(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	writeServiceErrorNoID(c, err)
	return w
}

func TestWriteServiceErrorHides500Detail(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"internal service error", service.NewInternal("failed to get instance: " + storeErrorDetail)},
		{"zero-status service error", &service.Error{Message: "failed: " + storeErrorDetail}},
		{"plain error", errors.New(storeErrorDetail)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveServiceError(t, tc.err)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "dial tcp") {
				t.Errorf("500 body leaks store detail: %s", body)
			}
			if !strings.Contains(body, "internal server error") {
				t.Errorf("500 body = %s, want a generic message", body)
			}
		})
	}
}

func TestWriteServiceErrorKeepsUpstreamDetail(t *testing.T) {
	w := serveServiceError(t, service.NewBadGateway("cubemaster error 130500: capacity exhausted"))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "capacity exhausted") {
		t.Errorf("502 = %d %s, want the upstream message", w.Code, w.Body.String())
	}

	w = serveServiceError(t, &service.Error{Status: http.StatusServiceUnavailable, Code: "retry-after:5", Message: "sandbox is pausing"})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "sandbox is pausing") {
		t.Errorf("503 = %d %s, want the upstream message", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
}
