// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// cubeAPIProxy is a reverse proxy that forwards requests to CubeAPI (port 3000).
// It is mounted under the authenticated router so all requests must pass JWT
// validation before being proxied. This closes the auth gap left when the
// /cubeapi/v1 admin mirror routes were removed during the CubeOps extraction.
var cubeAPIProxy *httputil.ReverseProxy

// InitCubeAPIProxy initialises the reverse proxy target. Call once at startup.
func InitCubeAPIProxy(cubeAPIURL string) {
	if cubeAPIURL == "" {
		cubeAPIURL = "http://127.0.0.1:3000"
	}
	target, err := url.Parse(cubeAPIURL)
	if err != nil {
		panic("invalid CubeAPI URL: " + err.Error())
	}
	cubeAPIProxy = httputil.NewSingleHostReverseProxy(target)
}

// CubeAPIProxy is an http.HandlerFunc that forwards the request to CubeAPI
// after stripping the /api/v1/sdk prefix.  Mount under the authed router.
func CubeAPIProxy(w http.ResponseWriter, r *http.Request) {
	if cubeAPIProxy == nil {
		http.Error(w, `{"error":"CubeAPI proxy not initialised"}`, http.StatusServiceUnavailable)
		return
	}
	// Strip the /api/v1/sdk prefix so CubeAPI sees the original SDK path.
	// e.g. /api/v1/sdk/sandboxes → /sandboxes
	path := r.URL.Path
	if idx := strings.Index(path, "/sdk/"); idx >= 0 {
		r.URL.Path = path[idx+4:] // keep leading "/"
	}
	r.Host = "" // let ReverseProxy set it from the target
	cubeAPIProxy.ServeHTTP(w, r)
}
