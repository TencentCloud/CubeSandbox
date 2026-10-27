// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package templatecenter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// The create-success hook clones the request before sandboxspec.Put. Losing
// this private field there kept created metadata but erased it for every later
// lifecycle event. It must remain private on the API wire and independently owned.
func TestCloneCreateRequestPreservesWebhookProvenance(t *testing.T) {
	for _, metadata := range []*types.WebhookMetadata{
		nil, {}, {Values: map[string]string{}},
		{Values: map[string]string{"user_token": "original"}}, {TooLarge: true},
	} {
		req := &types.CreateCubeSandboxReq{WebhookMetadata: metadata}
		cloned, err := cloneCreateRequest(req)
		require.NoError(t, err)
		require.Equal(t, metadata, cloned.WebhookMetadata)
		if metadata != nil {
			require.NotSame(t, metadata, cloned.WebhookMetadata)
			if metadata.Values != nil {
				cloned.WebhookMetadata.Values["added"] = "clone-only"
				require.NotContains(t, metadata.Values, "added")
			}
		}
		wire, err := json.Marshal(cloned)
		require.NoError(t, err)
		require.NotContains(t, string(wire), "webhook")
	}
}
