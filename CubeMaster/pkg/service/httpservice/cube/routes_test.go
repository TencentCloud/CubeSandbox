// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// routeHandlerName returns the registered handler's function name for
// (method, path), or "" when no such route exists.
func routeHandlerName(r *gin.Engine, method, path string) string {
	for _, ri := range r.Routes() {
		if ri.Method == method && ri.Path == path {
			return ri.Handler
		}
	}
	return ""
}

func hasRoute(r *gin.Engine, method, path string) bool {
	return routeHandlerName(r, method, path) != ""
}

// newRouteInspectEngine builds a bare engine with the given registration
// function. Route registration only records handlers, so no config / DB /
// grpc plumbing is needed — handlers are never invoked.
func newRouteInspectEngine(t *testing.T, register func(g *gin.RouterGroup)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	register(r.Group("/cube"))
	return r
}

// TestCubeRoutesTemplateWritesStayOnCubeMaster guards the control-plane /
// data-plane split: every template route whose handler can reach a cubelet
// over the worker grpc pool (create/delete/redo) or that goes through the
// process-local template query caches (info/list read, alias write) MUST be
// served by CubeMaster itself, never proxied to CubeTemplateCenter. The
// build-status / from-image polls are local too: they read job rows
// CubeMaster owns, and proxying them made a successful create look broken
// when TC was unreachable (create 200, poll 502).
//
// Regression being guarded: DELETE /cube/template used to be proxied to TC,
// where the uninitialized grpc conn pool failed every delete with "worker
// grpc conn pool is not initialized"; and after the delete moved to
// CubeMaster, a TC-served GET kept answering from its stale in-memory cache
// for up to the 1-minute TTL because invalidateTemplateCaches only clears
// the writing process's caches.
func TestCubeRoutesTemplateWritesStayOnCubeMaster(t *testing.T) {
	r := newRouteInspectEngine(t, RegisterCubeRoutes)

	localRoutes := map[string]string{
		"POST /cube/template":                       "createTemplateGinHandler",
		"DELETE /cube/template":                     "deleteTemplateGinHandler",
		"GET /cube/template":                        "getTemplateGinHandler",
		"PUT /cube/template/:template_id/alias":     "setTemplateAliasGinHandler",
		"POST /cube/template/from-image":            "createTemplateFromImageGinHandler",
		"POST /cube/template/redo":                  "handleRedoTemplateAction",
		"GET /cube/template/build/:build_id/status": "handleTemplateBuildStatusAction",
		"GET /cube/template/from-image":             "getTemplateFromImageGinHandler",
	}
	for route, wantHandler := range localRoutes {
		method, path, _ := strings.Cut(route, " ")
		got := routeHandlerName(r, method, path)
		if got == "" {
			t.Errorf("%s: route not registered on CubeMaster", route)
			continue
		}
		if !strings.HasSuffix(got, "."+wantHandler) {
			t.Errorf("%s: handler = %s, want local %s", route, got, wantHandler)
		}
		if strings.Contains(got, "proxyToTemplateCenter") {
			t.Errorf("%s: must not be proxied to TC (runs cubelet RPCs or uses process-local caches)", route)
		}
	}
}

// TestCubeRoutesTemplateProxyRemainder pins the routes that are still proxied
// to CubeTemplateCenter: the compat matrix (plain DB read/write) and the
// artifact download (file serving / S3 redirect). Neither goes through the
// process-local template caches, so cross-process coherency does not apply.
func TestCubeRoutesTemplateProxyRemainder(t *testing.T) {
	r := newRouteInspectEngine(t, RegisterCubeRoutes)

	proxiedRoutes := []string{
		"GET /cube/template/compat",
		"POST /cube/template/compat",
		"GET /cube/template/artifact/download",
		"HEAD /cube/template/artifact/download",
	}
	for _, route := range proxiedRoutes {
		method, path, _ := strings.Cut(route, " ")
		got := routeHandlerName(r, method, path)
		if got == "" {
			t.Errorf("%s: route not registered on CubeMaster", route)
			continue
		}
		if !strings.Contains(got, "proxyToTemplateCenter") {
			t.Errorf("%s: handler = %s, want proxyToTemplateCenter", route, got)
		}
	}
}

// TestTemplateCenterRoutesExcludeWritesAndCachedReads guards the TC side of
// the split: the standalone CubeTemplateCenter process must not register any
// template write route (its grpc conn pool is never initialized, so a
// cubelet-touching handler would fail mid-flight) nor the cached info/list
// reads (its caches would never be invalidated by CubeMaster's writes).
func TestTemplateCenterRoutesExcludeWritesAndCachedReads(t *testing.T) {
	r := newRouteInspectEngine(t, RegisterTemplateRoutes)

	absent := []string{
		"POST /cube/template",
		"DELETE /cube/template",
		"GET /cube/template",
		"PUT /cube/template/:template_id/alias",
		"POST /cube/template/from-image",
		"POST /cube/template/redo",
	}
	for _, route := range absent {
		method, path, _ := strings.Cut(route, " ")
		if hasRoute(r, method, path) {
			t.Errorf("%s: must not be registered on CubeTemplateCenter", route)
		}
	}

	present := []string{
		"GET /cube/template/compat",
		"POST /cube/template/compat",
		"GET /cube/template/build/:build_id/status",
		"GET /cube/template/from-image",
		"GET /cube/template/artifact/download",
		"HEAD /cube/template/artifact/download",
	}
	for _, route := range present {
		method, path, _ := strings.Cut(route, " ")
		if !hasRoute(r, method, path) {
			t.Errorf("%s: expected to stay registered on CubeTemplateCenter", route)
		}
	}
}
