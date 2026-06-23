// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package meta

import (
	"strings"
	"testing"
)

func TestValidateNodeID(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "ipv4 node id", id: "10.0.0.10", wantErr: false},
		{name: "ipv6-like with colons", id: "fe80::1", wantErr: false},
		{name: "hostname style", id: "cube-edge-01", wantErr: false},
		{name: "alnum with underscore", id: "node_a1", wantErr: false},
		{name: "empty", id: "", wantErr: true},
		{name: "path traversal", id: "../etc/passwd", wantErr: true},
		{name: "contains slash", id: "a/b", wantErr: true},
		{name: "contains backslash", id: "a\\b", wantErr: true},
		{name: "whitespace", id: "node a", wantErr: true},
		{name: "double dot", id: "a..b", wantErr: true},
		{name: "too long", id: strings.Repeat("a", 256), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNodeID(tc.id)
			if tc.wantErr && err == nil {
				t.Fatalf("validateNodeID(%q) = nil, want error", tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateNodeID(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}
