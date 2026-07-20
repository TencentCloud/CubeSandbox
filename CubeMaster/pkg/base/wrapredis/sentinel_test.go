// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package wrapredis

import (
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestParseRedisAddrs(t *testing.T) {
	assert.Equal(t, []string{"127.0.0.1:26379", "127.0.0.2:26379"}, parseRedisAddrs("127.0.0.1:26379, 127.0.0.2:26379"))
	assert.Equal(t, []string{"10.0.0.11:26379", "10.0.0.12:26379"}, parseRedisAddrs("10.0.0.11, 10.0.0.12"))
	assert.Equal(t, []string{"[::1]:26379"}, parseRedisAddrs("[::1]:26379"))
	assert.Empty(t, parseRedisAddrs(""))
	assert.Empty(t, parseRedisAddrs(" , "))
}

func TestResolveRedisAddrStandalone(t *testing.T) {
	addr, err := resolveRedisAddr(&config.RedisConf{Nodes: "127.0.0.1:6379"})
	assert.NoError(t, err)
	assert.Equal(t, "127.0.0.1:6379", addr)
}

func TestResolveRedisAddrStandaloneRequiresNodes(t *testing.T) {
	_, err := resolveRedisAddr(&config.RedisConf{})
	assert.Error(t, err)
}

func TestResolveRedisAddrSentinelRequiresSentinelNodes(t *testing.T) {
	_, err := resolveRedisAddr(&config.RedisConf{
		MasterName: "mymaster",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "sentinel_nodes")
}

func TestRedisDisplayAddr(t *testing.T) {
	assert.Equal(t, "127.0.0.1:6379", redisDisplayAddr(&config.RedisConf{Nodes: "127.0.0.1:6379"}))
	assert.Equal(t, "sentinel:mymaster(127.0.0.1:26379)", redisDisplayAddr(&config.RedisConf{
		MasterName:    "mymaster",
		SentinelNodes: "127.0.0.1:26379",
	}))
}

func TestIsReadonlyErr(t *testing.T) {
	assert.False(t, isReadonlyErr(nil))
	assert.False(t, isReadonlyErr(assert.AnError))
	assert.True(t, isReadonlyErr(redis.Error("READONLY You can't write against a read only replica.")))
}

func TestNormalizeDoReply(t *testing.T) {
	reply, err := normalizeDoReply("OK", nil)
	assert.NoError(t, err)
	assert.Equal(t, "OK", reply)

	reply, err = normalizeDoReply(redis.Error("READONLY boom"), nil)
	assert.Nil(t, reply)
	assert.True(t, isReadonlyErr(err))

	_, err = normalizeDoReply(nil, assert.AnError)
	assert.Equal(t, assert.AnError, err)
}
