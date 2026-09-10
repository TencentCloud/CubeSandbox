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

// The regression: a request that arrived on a wildcard Host (e.g. `curl
// http://0.0.0.0:8089`) used to make that address the artifact download base
// URL. It is persisted into rootfs_artifacts.master_node_ip and handed to
// every Cubelet, whose artifact download then fails while the build job
// itself looks healthy. A configured address must win over the Host header.
func TestRequestBaseURLPrefersConfiguredMasterAddrOverWildcardHost(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://9.135.79.34:8089")
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "0.0.0.0:8089"
	if got, want := requestBaseURL(r), "http://9.135.79.34:8089"; got != want {
		t.Fatalf("requestBaseURL() = %q, want %q (Host header must not win)", got, want)
	}
}

func TestRequestBaseURLNilRequestDoesNotPanic(t *testing.T) {
	// With no request and no usable configuration there is no externally
	// usable base URL; callers fall back to the artifact row / hostname hint.
	_ = requestBaseURL(nil)
}

// A loopback/wildcard CUBE_MASTER_ADDR must not become the Cubelet-facing
// download URL: one-click ships exactly that value, and remote nodes then
// download from their own loopback while the build job reports success.
func TestRequestBaseURLRejectsLoopbackConfiguredAddr(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://127.0.0.1:8089")
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "9.135.79.34:8089"
	if got, want := requestBaseURL(r), "http://9.135.79.34:8089"; got != want {
		t.Fatalf("requestBaseURL() = %q, want %q (loopback CUBE_MASTER_ADDR must be ignored)", got, want)
	}
}

// With nothing usable configured and a wildcard Host, the function must say
// "no usable base URL" instead of persisting an address no node can dial.
func TestRequestBaseURLEmptyWhenNothingUsable(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/cube/template/from-image", nil)
	r.Host = "0.0.0.0:8089"
	if got := requestBaseURL(r); got != "" {
		t.Fatalf("requestBaseURL() = %q, want empty (0.0.0.0 is not dialable from other nodes)", got)
	}
}
