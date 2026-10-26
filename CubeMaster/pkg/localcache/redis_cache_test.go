// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package localcache

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
)

func TestSandboxProxyMapMaskRequestHostRoundTrip(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := config.GetConfig()
	cfg.RedisConf = &config.RedisConf{
		Nodes:       server.Addr(),
		MaxActive:   4,
		MaxIdle:     1,
		MaxRetry:    1,
		DbNo:        0,
		IdleTimeout: 30,
	}

	cache := &local{}
	key := "test:sandbox:proxy:sandbox-1"
	want := &proxytypes.SandboxProxyMap{
		HostIP:             "10.0.0.1",
		SandboxIP:          "192.168.0.2",
		CreatedAt:          "123",
		AllowPublicTraffic: true,
		MaskRequestHost:    "localhost:${PORT}",
		ContainerToHostPorts: map[string]string{
			"3000": "23000",
		},
	}

	if err := cache.setByPassProsyToRedis(context.Background(), key, want); err != nil {
		t.Fatalf("setByPassProsyToRedis failed: %v", err)
	}
	got, err := cache.getByPassProsyFromRedis(context.Background(), key)
	if err != nil {
		t.Fatalf("getByPassProsyFromRedis failed: %v", err)
	}
	if got.MaskRequestHost != want.MaskRequestHost {
		t.Fatalf("MaskRequestHost=%q, want %q", got.MaskRequestHost, want.MaskRequestHost)
	}
	if len(got.ContainerToHostPorts) != 1 || got.ContainerToHostPorts["3000"] != "23000" {
		t.Fatalf("unexpected ContainerToHostPorts: %#v", got.ContainerToHostPorts)
	}
	if _, leaked := got.ContainerToHostPorts["MaskRequestHost"]; leaked {
		t.Fatal("MaskRequestHost leaked into ContainerToHostPorts")
	}
}
