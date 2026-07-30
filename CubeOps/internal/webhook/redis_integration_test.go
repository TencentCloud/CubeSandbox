// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/redis/go-redis/v9"
)

const redisTestImageTag = "7-alpine"

// requireRedisDocker keeps the integration test usable on developer machines
// without Docker, while ensuring CI does not silently skip it.
func requireRedisDocker(t *testing.T) *dockertest.Pool {
	t.Helper()
	pool, err := dockertest.NewPool("")
	if err != nil {
		skipOrFailRedisDocker(t, "dockertest not available (%v)", err)
	}
	if err := pool.Client.Ping(); err != nil {
		skipOrFailRedisDocker(t, "docker daemon not reachable (%v)", err)
	}
	return pool
}

func skipOrFailRedisDocker(t *testing.T, format string, args ...any) {
	t.Helper()
	message := fmt.Sprintf(format, args...)
	if os.Getenv("CUBEOPS_REQUIRE_DOCKER_TESTS") == "1" ||
		strings.EqualFold(os.Getenv("CUBEOPS_REQUIRE_DOCKER_TESTS"), "true") ||
		os.Getenv("CI") == "1" || strings.EqualFold(os.Getenv("CI"), "true") {
		t.Fatal(message)
	}
	t.Skip(message)
}

func TestRedisSourceRecoversEventsAcrossRedisAndConsumerRestart(t *testing.T) {
	pool := requireRedisDocker(t)
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "redis",
		Tag:        redisTestImageTag,
		Cmd: []string{
			"redis-server",
			"--appendonly", "yes",
			"--appendfsync", "always",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		skipOrFailRedisDocker(t, "could not start redis container (%v)", err)
	}
	t.Cleanup(func() { _ = pool.Purge(resource) })

	address := fmt.Sprintf("127.0.0.1:%s", resource.GetPort("6379/tcp"))
	client := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { _ = client.Close() })
	pool.MaxWait = 30 * time.Second
	if err := pool.Retry(func() error { return client.Ping(t.Context()).Err() }); err != nil {
		t.Fatalf("redis container never became reachable: %v", err)
	}

	redisURL := "redis://" + address + "/0"
	group := "cubeops-webhook-restart-test"
	beforeRestart, err := newRedisSource(redisURL)
	if err != nil {
		t.Fatalf("new source before restart: %v", err)
	}
	if err := beforeRestart.EnsureGroup(t.Context(), group); err != nil {
		t.Fatalf("create consumer group: %v", err)
	}
	if err := beforeRestart.Close(); err != nil {
		t.Fatalf("close source before restart: %v", err)
	}

	streamID, err := client.XAdd(t.Context(), &redis.XAddArgs{
		Stream: EventStreamKey,
		Values: map[string]any{
			"event_id":   "event-written-during-downtime",
			"op":         "create",
			"sandbox_id": "sandbox-1",
			"ts":         "1760000000000",
			"payload":    `{"template_id":"template-1"}`,
		},
	}).Result()
	if err != nil {
		t.Fatalf("append lifecycle event: %v", err)
	}
	if err := pool.Client.RestartContainer(resource.Container.ID, 0); err != nil {
		t.Fatalf("restart redis container: %v", err)
	}
	if err := pool.Retry(func() error { return client.Ping(t.Context()).Err() }); err != nil {
		t.Fatalf("redis container did not recover after restart: %v", err)
	}

	afterRestart, err := newRedisSource(redisURL)
	if err != nil {
		t.Fatalf("new source after restart: %v", err)
	}
	t.Cleanup(func() { _ = afterRestart.Close() })
	if err := afterRestart.EnsureGroup(t.Context(), group); err != nil {
		t.Fatalf("reuse consumer group after restart: %v", err)
	}

	events, err := afterRestart.Read(t.Context(), group, "consumer-after-restart", time.Second, 10)
	if err != nil {
		t.Fatalf("read lifecycle event after restart: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events read after restart = %d, want 1", len(events))
	}
	if events[0].StreamID != streamID || events[0].EventID != "event-written-during-downtime" {
		t.Fatalf("event read after restart = %#v, want stream_id %q and event_id %q", events[0], streamID, "event-written-during-downtime")
	}
	if err := afterRestart.Ack(t.Context(), group, streamID); err != nil {
		t.Fatalf("ack recovered event: %v", err)
	}
}
