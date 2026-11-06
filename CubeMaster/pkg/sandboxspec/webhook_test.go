// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandboxspec

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"testing"
)

func TestWebhookProvenanceStoredOnly(t *testing.T) {
	for _, m := range []*types.WebhookMetadata{nil, {Values: nil}, {Values: map[string]string{}}, {Values: map[string]string{"my_token": "ok"}}, {TooLarge: true}} {
		req := &types.CreateCubeSandboxReq{WebhookMetadata: m}
		c, err := CanonicalizeRequest(req)
		require.NoError(t, err)
		raw, err := marshalStoredSpec(c)
		require.NoError(t, err)
		got, err := decodeSpec(&models.SandboxSpec{RequestJSON: string(raw)})
		require.NoError(t, err)
		require.Equal(t, m, got.WebhookMetadata)
		api, err := json.Marshal(req)
		require.NoError(t, err)
		require.NotContains(t, string(api), "webhook")
		var spoof types.CreateCubeSandboxReq
		require.NoError(t, json.Unmarshal(raw, &spoof))
		require.Nil(t, spoof.WebhookMetadata)
	}
}
