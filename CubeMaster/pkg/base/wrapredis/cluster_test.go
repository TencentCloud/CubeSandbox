// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package wrapredis

import (
	"os"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestClusterWrapper(t *testing.T) {
	nodes := os.Getenv("TEST_REDIS_CLUSTER_NODES")
	if nodes == "" {
		t.Skip("TEST_REDIS_CLUSTER_NODES is not set")
	}
	r := GetRedisConnPoolWrap("cluster-test", &config.RedisConf{ClusterNodes: nodes, MaxIdle: 2, MaxActive: 8})
	defer r.RedisConnPool.Close()
	defer r.cluster.Close()
	key := "cube-master-cluster-test:" + time.Now().Format("150405.000000000")
	defer r.Do("DEL", key)
	if _, err := r.Do("SET", key, "owner", "NX", "EX", 60); err != nil {
		t.Fatal(err)
	}
	v, err := redis.String(r.Do("EVAL", "return redis.call('GET',KEYS[1])", 1, key))
	if err != nil || v != "owner" {
		t.Fatalf("wrapper script: %q %v", v, err)
	}
}
