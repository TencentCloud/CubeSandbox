// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"context"
	"testing"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func TestGUsesRequestTraceContext(t *testing.T) {
	ctx := context.Background()
	entry := G(ctx)
	if entry == nil {
		t.Fatal("G returned nil entry")
	}
	stored := WithLogger(ctx, entry)
	if got := G(stored); got != entry {
		t.Fatal("G did not return the entry stored in context")
	}
}

func TestParseCubeLogLevel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want cubelog.LogLevel
	}{
		{name: "debug", in: "debug", want: cubelog.DEBUG},
		{name: "warning", in: "warning", want: cubelog.WARN},
		{name: "error", in: "error", want: cubelog.ERROR},
		{name: "default", in: "unknown", want: cubelog.INFO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCubeLogLevel(tt.in); got != tt.want {
				t.Errorf("parseCubeLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
