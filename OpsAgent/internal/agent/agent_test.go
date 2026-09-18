// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/config"
)

func TestRequireToken(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		header   string
		wantCode int
	}{
		{"matching token", "s3cret", "s3cret", http.StatusOK},
		{"missing token", "s3cret", "", http.StatusUnauthorized},
		{"wrong token", "s3cret", "bad", http.StatusUnauthorized},
		{"unset fails closed", "", "s3cret", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{cfg: &config.Config{SharedToken: tc.token}}

			called := false
			next := func(w http.ResponseWriter, r *http.Request) { called = true }
			h := a.requireToken(next)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/config/quota", nil)
			if tc.header != "" {
				req.Header.Set(TokenHeader, tc.header)
			}
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantCode == http.StatusOK && !called {
				t.Fatal("handler not invoked on authorized request")
			}
			if tc.wantCode == http.StatusUnauthorized && called {
				t.Fatal("handler invoked on unauthorized request")
			}
		})
	}
}
