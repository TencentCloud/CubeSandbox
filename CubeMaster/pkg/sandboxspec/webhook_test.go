// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandboxspec

import (
	"context"
	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestOrdinarySpecIgnoresLegacyWebhookExtension(t *testing.T) {
	for _, extension := range []string{`null`, `{"values":{"user_token":"legacy"}}`, `{"too_large":true}`, `"unknown"`} {
		raw := `{"instanceType":"cubebox","labels":{"project":"user"},"_webhook_metadata":` + extension + `}`
		got, err := decodeSpec(&models.SandboxSpec{RequestJSON: raw})
		require.NoError(t, err)
		require.Equal(t, "user", got.Labels["project"])
		canonical, err := CanonicalizeRequest(got)
		require.NoError(t, err)
		wire, err := json.Marshal(canonical)
		require.NoError(t, err)
		require.NotContains(t, string(wire), "webhook")
		var api types.CreateCubeSandboxReq
		require.NoError(t, json.Unmarshal([]byte(raw), &api))
		ordinary, err := CanonicalizeRequest(&api)
		require.NoError(t, err)
		require.Equal(t, canonical, ordinary)
	}
}

func TestStoredSpecUsesOrdinaryJSONAfterLegacyRead(t *testing.T) {
	database := setupSpecStore(t)
	record := &models.SandboxSpec{SandboxID: "legacy", Backend: "xfs", RequestJSON: `{"instance_type":"cubebox","labels":{"project":"kept"},"_webhook_metadata":{"values":{"old":"private"}}}`}
	require.NoError(t, database.Create(record).Error)
	req, err := Get(context.Background(), "legacy")
	require.NoError(t, err)
	canonical, err := CanonicalizeRequest(req)
	require.NoError(t, err)
	// Put normally stamps the normalized backend in annotations.
	canonical.Annotations[constants.CubeAnnotationStorageBackend] = "xfs"
	raw, err := json.Marshal(canonical)
	require.NoError(t, err)
	require.NoError(t, Put(context.Background(), "legacy", req, PutOptions{}))
	var stored models.SandboxSpec
	require.NoError(t, database.Where("sandbox_id = ?", "legacy").First(&stored).Error)
	require.JSONEq(t, string(raw), stored.RequestJSON)
	require.NotContains(t, stored.RequestJSON, "webhook")
	require.Equal(t, "kept", req.Labels["project"])
}
