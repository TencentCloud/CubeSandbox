// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// TestCreateRequestCopyDropsWebhookMetadata keeps the template-side clone from
// carrying the webhook ingress' _webhook_metadata blob: metadata is captured
// before template merging and must not reach a stored or cached template
// request. Both the JSON clone this used to run through and the deep copy that
// replaced it only carry modelled fields, so an unknown key never survives.
func TestCreateRequestCopyDropsWebhookMetadata(t *testing.T) {
	var req types.CreateCubeSandboxReq
	require.NoError(t, json.Unmarshal([]byte(`{"labels":{"project":"user"},"_webhook_metadata":{"values":{"secret":"old"}}}`), &req))
	cloned := req.DeepCopy()
	require.Equal(t, req.Labels, cloned.Labels)
	cloned.Labels["project"] = "clone-only"
	require.Equal(t, "user", req.Labels["project"])
	wire, err := json.Marshal(cloned)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "webhook")
	require.NotContains(t, string(wire), "secret")
}
