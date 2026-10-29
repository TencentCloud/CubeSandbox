// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package wrapredis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestDialContextRequiresBoundAndHonorsCancellation(t *testing.T) {
	_, err := DialContext(context.Background(), config.RedisConf{Nodes: "127.0.0.1:1"})
	require.ErrorContains(t, err, "deadline")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	_, err = DialContext(ctx, config.RedisConf{Nodes: "127.0.0.1:1"})
	require.ErrorIs(t, err, context.Canceled)
	_, err = DialContext(ctx, config.RedisConf{MasterName: "master", SentinelNodes: "127.0.0.1:1,127.0.0.1:2"})
	require.ErrorIs(t, err, context.Canceled)
}
