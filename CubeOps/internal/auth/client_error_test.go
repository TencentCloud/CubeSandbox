// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

func TestWriteClientErrorOnlyWrites4xxServiceErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		handled  bool
		wantCode int
		wantBody string
	}{
		{"plain error", errors.New("dial tcp 10.0.0.5:3306"), false, 0, ""},
		{"zero status", &service.Error{Message: "leak"}, false, 0, ""},
		{"internal", service.NewInternal("leak"), false, 0, ""},
		{"below 4xx", &service.Error{Status: 399, Message: "leak"}, false, 0, ""},
		{"bad request", service.NewBadRequest("bad"), true, http.StatusBadRequest, "bad"},
		{"not found", service.NewNotFound("gone"), true, http.StatusNotFound, "gone"},
		{"wrapped", fmt.Errorf("wrap: %w", service.NewBadRequest("bad")), true, http.StatusBadRequest, "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			if got := writeClientError(c, tc.err); got != tc.handled {
				t.Fatalf("writeClientError = %v, want %v", got, tc.handled)
			}
			body := w.Body.String()
			if !tc.handled {
				if body != "" {
					t.Errorf("unhandled error wrote a response: %s", body)
				}
				return
			}
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if !strings.Contains(body, tc.wantBody) || strings.Contains(body, "wrap:") {
				t.Errorf("body = %s, want only the service message %q", body, tc.wantBody)
			}
		})
	}
}
