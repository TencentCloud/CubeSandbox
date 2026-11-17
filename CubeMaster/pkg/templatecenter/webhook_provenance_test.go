// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestCloneCreateRequestUsesOrdinaryJSON(t *testing.T) {
	var req types.CreateCubeSandboxReq
	require.NoError(t, json.Unmarshal([]byte(`{"labels":{"project":"user"},"_webhook_metadata":{"values":{"secret":"old"}}}`), &req))
	cloned, err := cloneCreateRequest(&req)
	require.NoError(t, err)
	require.Equal(t, req.Labels, cloned.Labels)
	cloned.Labels["project"] = "clone-only"
	require.Equal(t, "user", req.Labels["project"])
	wire, err := json.Marshal(cloned)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "webhook")
	require.NotContains(t, string(wire), "secret")
}
