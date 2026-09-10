// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

func TestHostReachableByOtherNodes(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"9.135.79.34:8089", true},
		{"cube-master.cube-system.svc:8089", true},
		{"master.example.com", true},

		// Wildcard bind addresses and loopback are meaningless to any other
		// node: a Cubelet told to download from them fails.
		{"0.0.0.0:8089", false},
		{"0.0.0.0", false},
		{"[::]:8089", false},
		{"localhost:8089", false},
		{"127.0.0.1:8089", false},
		{"[::1]:8089", false},
		{"", false},
	}
	for _, c := range cases {
		if got := templatecenter.HostReachableByOtherNodes(c.host); got != c.want {
			t.Fatalf("HostReachableByOtherNodes(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// A real, externally-usable Host is still honored when nothing is configured.
func TestRequestBaseURLKeepsUsableHost(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "9.135.79.34:8089"
	if got, want := requestBaseURL(r), "http://9.135.79.34:8089"; got != want {
		t.Fatalf("requestBaseURL() = %q, want %q", got, want)
	}
}

// A configured address always wins over the request Host, even when the caller
// reached CubeMaster through a different addr.
func TestRequestBaseURLPrefersConfiguredMasterAddrOverRequestHost(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://9.135.79.34:8089")
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "0.0.0.0:8089"
	if got, want := requestBaseURL(r), "http://9.135.79.34:8089"; got != want {
		t.Fatalf("requestBaseURL() = %q, want %q (configured addr must win)", got, want)
	}
}

func TestRequestBaseURLNilRequestDoesNotPanic(t *testing.T) {
	_ = requestBaseURL(nil)
}

func TestRequestBaseURLKeepsConfiguredLoopbackAddr(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://127.0.0.1:8089")
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "9.135.79.34:8089"
	if got, want := requestBaseURL(r), "http://127.0.0.1:8089"; got != want {
		t.Fatalf("requestBaseURL() = %q, want %q", got, want)
	}
}

func TestRequestBaseURLFallsBackToRequestHostWhenUnset(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "0.0.0.0:8089"
	if got, want := requestBaseURL(r), "http://0.0.0.0:8089"; got != want {
		t.Fatalf("requestBaseURL() = %q, want %q", got, want)
	}
}
