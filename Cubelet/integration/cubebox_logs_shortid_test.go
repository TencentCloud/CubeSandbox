// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/sandboxid"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// TestLogsShortIDResolution verifies that a short sandbox ID prefix can be
// resolved to the full 32-char ID via the same List + Resolve path that
// cubecli logs now uses.  This is the E2E counterpart of the unit tests in
// cubebox/logs_test.go and cubebox/resolve_test.go.
func TestLogsShortIDResolution(t *testing.T) {
	sandboxID := SimpleCubeboxConfigWithCleanup(t)
	require.True(t, sandboxid.IsFullID(sandboxID),
		"created sandbox should have a full 32-char hex ID, got %q", sandboxID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := cubeClient.List(ctx, &cubebox.ListCubeSandboxRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Items)

	candidates := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		candidates = append(candidates, item.GetId())
	}

	for _, prefixLen := range []int{4, 8, 12, len(sandboxID)} {
		prefix := sandboxID[:prefixLen]
		t.Run(prefix, func(t *testing.T) {
			resolved, err := sandboxid.Resolve(prefix, candidates)
			require.NoError(t, err)
			require.Equal(t, sandboxID, resolved,
				"prefix %q should resolve to full ID", prefix)
		})
	}
}

// TestLogsShortIDResolutionAmbiguous verifies that an ambiguous prefix is
// rejected with ErrAmbiguous when two sandboxes share the same prefix.
func TestLogsShortIDResolutionAmbiguous(t *testing.T) {
	sb1 := SimpleCubeboxConfigWithCleanup(t)
	sb2 := SimpleCubeboxConfigWithCleanup(t)

	// Find the shortest shared prefix.
	shared := 0
	for i := 0; i < len(sb1) && i < len(sb2); i++ {
		if sb1[i] != sb2[i] {
			break
		}
		shared = i + 1
	}
	if shared == 0 {
		t.Skip("sandbox IDs share no common prefix; cannot test ambiguity")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := cubeClient.List(ctx, &cubebox.ListCubeSandboxRequest{})
	require.NoError(t, err)

	candidates := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		candidates = append(candidates, item.GetId())
	}

	ambiguousPrefix := sb1[:shared]
	_, err = sandboxid.Resolve(ambiguousPrefix, candidates)
	require.ErrorIs(t, err, sandboxid.ErrAmbiguous,
		"prefix %q should be ambiguous across %q and %q", ambiguousPrefix, sb1, sb2)
}
