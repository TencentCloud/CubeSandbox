// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

// A real primary loss and Sentinel election. These synthetic tasks establish
// store recovery, separately from full Master/real-node lifecycle acceptance.
func TestP6RealSentinelElection(t *testing.T) {
	ctx, cancel := context.WithTimeout(testCtx, 60*time.Second)
	defer cancel()
	nodes := os.Getenv("WEBHOOK_P6_SENTINEL_NODES")
	primary := os.Getenv("WEBHOOK_P6_SENTINEL_PRIMARY_CONTAINER")
	if nodes == "" || primary == "" {
		t.Skip("dedicated Sentinel topology not configured")
	}
	require.True(t, strings.HasPrefix(primary, "webhook-1903-"))
	label, err := exec.Command("docker", "inspect", "--format", "{{index .Config.Labels \"purpose\"}}", primary).CombinedOutput()
	require.NoError(t, err, string(label))
	expectedLabel := os.Getenv("WEBHOOK_P6_RESOURCE_LABEL")
	require.True(t, strings.HasPrefix(expectedLabel, "webhook-p6-") && len(expectedLabel) > len("webhook-p6-"))
	require.Equal(t, expectedLabel, strings.TrimSpace(string(label)))
	masterName := os.Getenv("WEBHOOK_P6_SENTINEL_MASTER_NAME")
	require.NotEmpty(t, masterName)
	conf := config.RedisConf{MasterName: masterName, SentinelNodes: nodes, Password: os.Getenv("WEBHOOK_REDIS_PASSWORD"), SentinelPassword: os.Getenv("WEBHOOK_P6_SENTINEL_PASSWORD"), DbNo: 9}
	s, err := NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	prefix := "p6-sentinel:" + uuid.NewString() + ":"
	for i, k := range s.keys {
		s.keys[i] = prefix + k.(string)
	}
	keys := s.keys
	t.Cleanup(func() {
		defer s.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		c, e := wrapredis.DialContext(cleanupCtx, conf)
		if e == nil {
			defer c.Close()
			_, _ = c.Do("DEL", append(keys[:], prefix+"barrier")...)
		}
	})
	tasks := recoveryTasks()
	base := tasks[1].NextAttemptAt
	for _, task := range tasks {
		require.NoError(t, s.Put(testCtx, task))
	}
	claim := claimOne(t, s, base)
	require.Equal(t, "retry", claim.Task.SubscriptionID)
	require.NoError(t, s.Retry(testCtx, claim.Receipt, base.Add(4*time.Hour), FailureSummary{Code: "saved_retry"}))
	claim = claimOne(t, s, base.Add(time.Hour))
	require.Equal(t, "inflight", claim.Task.SubscriptionID)
	original := make([]deliveryRecord, len(tasks))
	for i, task := range tasks {
		original[i] = record(t, s, taskKey(task))
	}
	c, err := wrapredis.DialContext(ctx, conf)
	require.NoError(t, err)
	// WAIT follows a write on this same connection: its replication offset
	// includes earlier Lua writes. WAIT on a fresh idle connection is insufficient.
	_, err = c.Do("SET", prefix+"barrier", "replication-observation")
	require.NoError(t, err)
	replicas, err := redis.Int(c.Do("WAIT", 1, 5000))
	require.NoError(t, err)
	require.Equal(t, 1, replicas)
	c.Close()
	sentinel, err := redis.Dial("tcp", strings.Split(nodes, ",")[0], redis.DialPassword(conf.SentinelPassword))
	require.NoError(t, err)
	defer sentinel.Close()
	old, err := redis.Strings(sentinel.Do("SENTINEL", "get-master-addr-by-name", conf.MasterName))
	require.NoError(t, err)
	killed, err := exec.Command("docker", "kill", "--signal=KILL", primary).CombinedOutput()
	require.NoError(t, err, string(killed))
	var next []string
	require.Eventually(t, func() bool {
		next, err = redis.Strings(sentinel.Do("SENTINEL", "get-master-addr-by-name", conf.MasterName))
		return err == nil && len(next) == 2 && next[0] != old[0]
	}, 30*time.Second, 100*time.Millisecond)
	require.NoError(t, s.Close())
	s, err = NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	s.keys = keys
	require.Eventually(t, func() bool {
		c, e := wrapredis.DialContext(ctx, conf)
		if e != nil {
			return false
		}
		defer c.Close()
		v, e := redis.Values(c.Do("ROLE"))
		if e != nil || len(v) == 0 {
			return false
		}
		role, _ := redis.String(v[0], nil)
		return role == "master"
	}, 10*time.Second, 100*time.Millisecond)
	for i, task := range tasks {
		require.Equal(t, original[i], record(t, s, taskKey(task)), "preserve body/target/attempts/due/lease")
	}
	recovered := claimOne(t, s, base.Add(2*time.Hour))
	require.Equal(t, "inflight", recovered.Task.SubscriptionID)
	require.Equal(t, 2, recovered.Attempts)
	require.ErrorIs(t, s.Complete(testCtx, claim.Receipt, TerminalResult{State: DeliverySucceeded}), ErrStaleReceipt)
	require.NoError(t, s.Complete(testCtx, recovered.Receipt, TerminalResult{State: DeliverySucceeded}))
	evidence, _ := json.Marshal(map[string]any{"old_primary": old, "elected_primary": next, "replicas_acknowledged": replicas, "records_preserved": len(tasks), "recovered_attempts": recovered.Attempts})
	t.Log(string(evidence))
}
