// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestRegisterHandlersIncludesCADownloadRoute(t *testing.T) {
	s := &internalHttp{router: mux.NewRouter()}
	s.registerHandlers()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/cube/ca/cube-root-ca.crt", nil)
		var match mux.RouteMatch
		if !s.router.Match(req, &match) {
			t.Fatalf("%s /cube/ca/cube-root-ca.crt did not match any route", method)
		}
	}
}

func TestRegisterHandlersIncludesSandboxTerminalRoute(t *testing.T) {
	s := &internalHttp{router: mux.NewRouter()}
	s.registerHandlers()

	req := httptest.NewRequest(http.MethodGet, "/cube/sandbox/terminal", nil)
	var match mux.RouteMatch
	if !s.router.Match(req, &match) {
		t.Fatal("GET /cube/sandbox/terminal did not match any route")
	}
}
