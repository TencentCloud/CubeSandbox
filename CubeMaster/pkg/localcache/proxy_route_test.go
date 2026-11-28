// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
)

// withExistsOn routes the EXISTS seam to a throwaway miniredis.
func withExistsOn(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	withExistsFn(t, func(keys ...interface{}) (interface{}, error) {
		conn, err := redis.Dial("tcp", server.Addr())
		if err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		return conn.Do("EXISTS", keys...)
	})
	return server
}

func withExistsFn(t *testing.T, fn func(...interface{}) (interface{}, error)) {
	t.Helper()
	orig := redisExistsFn
	t.Cleanup(func() { redisExistsFn = orig })
	redisExistsFn = fn
}

func TestProxyRouteGone(t *testing.T) {
	const id = "sb-gone"
	tests := []struct {
		name string
		seed func(*miniredis.Miniredis)
		want bool
	}{
		{name: "both keys absent", seed: func(*miniredis.Miniredis) {}, want: true},
		{name: "current key present", seed: func(s *miniredis.Miniredis) {
			s.HSet(rediskey.SandboxProxy(id), "HostIP", "10.0.0.1")
		}, want: false},
		{name: "legacy key present", seed: func(s *miniredis.Miniredis) {
			s.HSet(rediskey.LegacySandboxProxy(id), "HostIP", "10.0.0.1")
		}, want: false},
		{name: "hash without HostIP is still a route", seed: func(s *miniredis.Miniredis) {
			s.HSet(rediskey.SandboxProxy(id), "SandboxIP", "192.168.0.2")
		}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := withExistsOn(t)
			tt.seed(server)
			assert.Equal(t, tt.want, ProxyRouteGone(context.Background(), id))
		})
	}
}

// TestProxyRouteGoneUnknownOnError: an outage is not evidence of teardown.
func TestProxyRouteGoneUnknownOnError(t *testing.T) {
	withExistsFn(t, func(...interface{}) (interface{}, error) { return nil, errors.New("i/o timeout") })
	assert.False(t, ProxyRouteGone(context.Background(), "sb-err"))
}

// TestSetSandboxCacheIfAbsentKeepsProxyBacking: a List re-stamp of the same host
// must not demote an entry the teardown check relies on.
func TestSetSandboxCacheIfAbsentKeepsProxyBacking(t *testing.T) {
	withShortTTL(t)
	SetSandboxCache("sb-12", &SandboxCache{SandboxID: "sb-12", HostIP: "10.0.0.1", FromProxy: true})
	t.Cleanup(func() { DeleteSandboxCache("sb-12") })
	expire(t)

	SetSandboxCacheIfAbsent("sb-12", &SandboxCache{SandboxID: "sb-12", HostIP: "10.0.0.1"})
	assert.True(t, GetSandboxCache("sb-12").FromProxy)

	expire(t)
	SetSandboxCacheIfAbsent("sb-12", &SandboxCache{SandboxID: "sb-12", HostIP: "10.0.0.2"})
	assert.False(t, GetSandboxCache("sb-12").FromProxy, "a different host is a new source")
}
