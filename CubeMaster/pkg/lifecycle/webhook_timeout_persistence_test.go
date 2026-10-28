// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/internal/webhooktest"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"os"
	"testing"
	"time"
)

type p5Redis struct {
	fakeRedis
	readErr error
	missing bool
}

func (r *p5Redis) Do(cmd string, args ...interface{}) (interface{}, error) {
	if cmd == "HGET" {
		if r.readErr != nil {
			return nil, r.readErr
		}
		if r.missing {
			return nil, nil
		}
		b, _ := json.Marshal(&SandboxLifecycleMeta{SandboxID: "sb", EndAt: 1})
		return b, nil
	}
	return r.fakeRedis.Do(cmd, args...)
}
func TestP5TimeoutAuthoritativeWrite(t *testing.T) {
	for _, hfail := range []bool{false, true} {
		for _, xfail := range []bool{false, true} {
			r := &p5Redis{fakeRedis: fakeRedis{failHSET: hfail, failXADD: xfail}}
			p := &storeTimeoutProvider{store: NewStore(r)}
			end, applied, err := p.RefreshTimeout(context.Background(), "sb", 10)
			if err != nil || applied == hfail || end <= 0 {
				t.Fatalf("HSET fail=%v XADD fail=%v: %d %v %v", hfail, xfail, end, applied, err)
			}
			calls := r.snapshot()
			if len(calls) != 2 || calls[0].cmd != "HSET" || calls[1].cmd != "XADD" {
				t.Fatalf("calls=%+v", calls)
			}
			var meta SandboxLifecycleMeta
			if err := json.Unmarshal(calls[0].args[2].([]byte), &meta); err != nil || meta.EndAt != end {
				t.Fatalf("written deadline differs: %+v %v", meta, err)
			}
		}
	}
	for _, name := range []string{"missing", "read-error", "disabled", "no-store"} {
		t.Run(name, func(t *testing.T) {
			r := &p5Redis{}
			s := NewStore(r)
			p := &storeTimeoutProvider{store: s}
			switch name {
			case "missing":
				r.missing = true
			case "read-error":
				r.readErr = errors.New("read")
			case "disabled":
				s.enabled.Store(false)
			case "no-store":
				p.store = nil
			}
			_, applied, _ := p.RefreshTimeout(context.Background(), "sb", 10)
			if applied {
				t.Fatal("unconfirmed write")
			}
		})
	}
}

func TestP5AuthoritativeRedisHTTP(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, 8, webhook.TypeUpdated)
	conn, err := redis.Dial("tcp", os.Getenv("WEBHOOK_P4_REDIS_ADDR"), redis.DialPassword(os.Getenv("WEBHOOK_P4_REDIS_PASSWORD")), redis.DialDatabase(8))
	require.NoError(t, err)
	defer conn.Close()
	s := NewStore(conn)
	payload, _ := json.Marshal(&SandboxLifecycleMeta{SandboxID: "sb-p5-authority"})
	_, err = conn.Do("HSET", MetaKey, "sb-p5-authority", payload)
	require.NoError(t, err)
	sandbox.SetTimeoutProvider(&storeTimeoutProvider{store: s})
	defer sandbox.SetTimeoutProvider(nil)
	sandbox.SetWebhookPublisher(r.Publisher())
	defer sandbox.SetWebhookPublisher(nil)
	localcache.SetSandboxCache("sb-p5-authority", &localcache.SandboxCache{SandboxID: "sb-p5-authority"})
	defer localcache.DeleteSandboxCache("sb-p5-authority")
	rsp := sandbox.SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb-p5-authority", Timeout: 5})
	require.Equal(t, 200, rsp.Ret.RetCode)
	stored, err := s.LoadMeta(context.Background(), "sb-p5-authority")
	require.NoError(t, err)
	require.Equal(t, rsp.EndAt, stored.EndAt)
	select {
	case body := <-bodies:
		var event map[string]any
		require.NoError(t, json.Unmarshal(body, &event))
		require.Equal(t, time.UnixMilli(stored.EndAt).UTC().Format(time.RFC3339), event["event_data"].(map[string]any)["set_timeout"])
	case <-time.After(10 * time.Second):
		t.Fatal("timeout delivery")
	}
}
