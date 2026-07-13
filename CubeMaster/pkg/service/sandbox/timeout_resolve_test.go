// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestResolveTimeoutSeconds(t *testing.T) {
	ptr := func(v int) *int { return &v }

	cases := []struct {
		name          string
		clientTimeout *int
		serverDefault int
		want          int
		wantErr       bool
	}{
		{
			name:          "nil with positive server default fills default",
			clientTimeout: nil,
			serverDefault: 300,
			want:          300,
		},
		{
			name:          "nil with zero server default means never timeout",
			clientTimeout: nil,
			serverDefault: 0,
			want:          types.NeverTimeout,
		},
		{
			name:          "nil with negative server default means never timeout",
			clientTimeout: nil,
			serverDefault: -5,
			want:          types.NeverTimeout,
		},
		{
			name:          "explicit zero is preserved (immediate timeout)",
			clientTimeout: ptr(0),
			serverDefault: 300,
			want:          0,
		},
		{
			name:          "explicit positive passes through",
			clientTimeout: ptr(45),
			serverDefault: 300,
			want:          45,
		},
		{
			name:          "explicit -1 means never timeout",
			clientTimeout: ptr(-1),
			serverDefault: 300,
			want:          types.NeverTimeout,
		},
		{
			name:          "any negative is normalized to never timeout",
			clientTimeout: ptr(-42),
			serverDefault: 300,
			want:          types.NeverTimeout,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveTimeoutSeconds(c.clientTimeout, c.serverDefault)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (value=%d)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("resolveTimeoutSeconds = %d, want %d", got, c.want)
			}
		})
	}
}
