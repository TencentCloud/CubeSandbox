// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxid"
)

func TestCacheResolveNeedsClusterFallback(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "not found", err: sandboxid.ErrNotFound, want: true},
		{name: "ambiguous", err: sandboxid.ErrAmbiguous, want: true},
		{name: "wrapped not found", err: fmt.Errorf("%w: %q", sandboxid.ErrNotFound, "ab"), want: true},
		{name: "wrapped ambiguous", err: fmt.Errorf("%w: %q matches %d sandboxes", sandboxid.ErrAmbiguous, "ab", 2), want: true},
		{name: "other", err: errors.New("boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheResolveNeedsClusterFallback(tt.err); got != tt.want {
				t.Fatalf("cacheResolveNeedsClusterFallback()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeSandboxIDs(t *testing.T) {
	got := mergeSandboxIDs(
		[]string{"aaa", "", "bbb"},
		[]string{"bbb", "ccc", ""},
	)
	want := []string{"aaa", "bbb", "ccc"}
	if len(got) != len(want) {
		t.Fatalf("mergeSandboxIDs()=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeSandboxIDs()=%v, want %v", got, want)
		}
	}
}
