// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

func recoveryTasks() []DeliveryTask {
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	out := make([]DeliveryTask, 3)
	for i, sub := range []string{"pending", "retry", "inflight"} {
		out[i] = testTask()
		out[i].Event.id = uuid.MustParse("d12727fc-ab97-43d8-80cf-556cc1215017")
		out[i].Event.occurredAt = base
		out[i].CreatedAt = base
		out[i].SubscriptionID = sub
	}
	out[0].NextAttemptAt = base.Add(3 * time.Hour)
	out[1].NextAttemptAt = base
	out[2].NextAttemptAt = base.Add(time.Hour)
	return out
}
func TestRedisRecoveryProcessHelper(t *testing.T) {
	phase := os.Getenv("P2_RECOVERY_PHASE")
	if phase == "" {
		return
	}
	s, err := NewRedisDeliveryStore(realConf(t), testOptions())
	require.NoError(t, err)
	defer s.Close()
	var keys []string
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("P2_RECOVERY_KEYS")), &keys))
	for i, k := range keys {
		s.keys[i] = k
	}
	tasks := recoveryTasks()
	base := tasks[1].NextAttemptAt
	if phase == "seed" {
		for _, task := range tasks {
			require.NoError(t, s.Put(testCtx, task))
		}
		c := claimOne(t, s, base)
		require.Equal(t, "retry", c.Task.SubscriptionID)
		require.NoError(t, s.Retry(testCtx, c.Receipt, base.Add(4*time.Hour), FailureSummary{Code: "retry_saved"}))
		c = claimOne(t, s, base.Add(time.Hour))
		require.Equal(t, "inflight", c.Task.SubscriptionID)
		return
	}
	for _, task := range tasks {
		require.NoError(t, s.Put(testCtx, task))
	} // uncertain Writer replay must not reset
	// Compare records before any takeover, in a newly started client process.
	for _, task := range tasks {
		key := task.Event.ID().String() + ":" + map[string]string{"pending": "7", "retry": "5", "inflight": "8"}[task.SubscriptionID] + ":" + task.SubscriptionID
		r := record(t, s, key)
		var in immutableDelivery
		require.NoError(t, json.Unmarshal([]byte(r.Input), &in))
		body, err := base64.RawURLEncoding.DecodeString(in.Body)
		require.NoError(t, err)
		require.Equal(t, task.Event.Body(), body)
		require.Equal(t, task.TargetURL, in.TargetURL)
		switch task.SubscriptionID {
		case "pending":
			require.Equal(t, 0, r.Attempts)
			require.Equal(t, task.NextAttemptAt.UnixMilli(), r.Next)
		case "retry":
			require.Equal(t, 1, r.Attempts)
			require.Equal(t, "pending", r.State)
			require.Equal(t, base.Add(4*time.Hour).UnixMilli(), r.Next)
			require.Equal(t, "retry_saved", r.Failure.Code)
		case "inflight":
			require.Equal(t, 1, r.Attempts)
			require.Equal(t, "inflight", r.State)
			require.Equal(t, base.Add(time.Hour+time.Second).UnixMilli(), r.Lease)
		}
	}
	if phase == "recover" {
		c := claimOne(t, s, base.Add(2*time.Hour))
		require.Equal(t, "inflight", c.Task.SubscriptionID)
		require.Equal(t, 2, c.Attempts)
		require.NoError(t, s.Complete(testCtx, c.Receipt, TerminalResult{State: DeliverySucceeded}))
		c = claimOne(t, s, base.Add(3*time.Hour))
		require.Equal(t, "pending", c.Task.SubscriptionID)
		require.Equal(t, 1, c.Attempts)
		c = claimOne(t, s, base.Add(4*time.Hour)) // pending lease expires before retry; consume it first if present
		if c.Task.SubscriptionID == "pending" {
			require.NoError(t, s.Complete(testCtx, c.Receipt, TerminalResult{State: DeliverySucceeded}))
			c = claimOne(t, s, base.Add(4*time.Hour))
		}
		require.Equal(t, "retry", c.Task.SubscriptionID)
		require.Equal(t, 2, c.Attempts)
	}
}
func TestRedisProcessAndServiceRecovery(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	keys, err := json.Marshal(s.keys)
	require.NoError(t, err)
	run := func(phase string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestRedisRecoveryProcessHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "P2_RECOVERY_PHASE="+phase, "P2_RECOVERY_KEYS="+string(keys))
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		t.Logf("child %s: %s", phase, out)
	}
	run("seed")
	run("inspect")
	container := os.Getenv("WEBHOOK_REDIS_RESTART_CONTAINER")
	if container == "" {
		t.Log("service restart not requested; process recovery still verified")
	} else {
		// Explicit opt-in only: never discover/restart a business Redis container.
		require.True(t, strings.HasPrefix(container, "webhook-1903-p2-"))
		out, err := exec.Command("docker", "restart", container).CombinedOutput()
		require.NoError(t, err, string(out))
		t.Logf("normal same-volume restart: %s", out)
		// docker restart returns before Redis finishes loading its AOF. Wait only
		// for readiness here; the new child still verifies all persisted states.
		conf := realConf(t)
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(testCtx, 200*time.Millisecond)
			defer cancel()
			c, err := wrapredis.DialContext(ctx, conf)
			if err != nil {
				return false
			}
			defer c.Close()
			_, err = redis.DoContext(c, ctx, "PING")
			return err == nil
		}, 10*time.Second, 20*time.Millisecond)
	}
	run("recover")
}
