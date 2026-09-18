// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestCompletedResumeReportsAllSynchronizationFailures(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		stateErr, purgeErr, proxyMapErr error
	}{
		{name: "success"},
		{name: "state", stateErr: errors.New("marker write failed")},
		{name: "proxy", purgeErr: errors.New("cache purge failed")},
		{name: "both", stateErr: errors.New("marker write failed"), purgeErr: errors.New("cache purge failed")},
		{name: "mapping", proxyMapErr: errors.New("mapping write failed")},
		{name: "all", proxyMapErr: errors.New("mapping write failed"), stateErr: errors.New("marker write failed"), purgeErr: errors.New("cache purge failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(markResumedLifecycleState, func(string) error { return tc.stateErr })
			hookCalled := false
			patches.ApplyFunc(runAfterUpdateSandboxSuccessHook, func(context.Context, string, string, string, string) {
				hookCalled = true
			})
			rsp := &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
			completeResumeSynchronization(context.Background(), &types.UpdateRequest{SandboxID: "sbx"}, rsp, "127.0.0.1", tc.proxyMapErr, tc.purgeErr)
			if !rsp.ResumeCompleted || !hookCalled {
				t.Fatal("completed restore lost bookkeeping")
			}
			wantCode := int(errorcode.ErrorCode_Success)
			for _, err := range []error{tc.stateErr, tc.purgeErr, tc.proxyMapErr} {
				if err != nil {
					wantCode = int(errorcode.ErrorCode_MasterInternalError)
					if !strings.Contains(rsp.Ret.RetMsg, err.Error()) {
						t.Fatalf("lost synchronization error %v: %+v", err, rsp.Ret)
					}
				}
			}
			if tc.proxyMapErr != nil {
				wantCode = int(errorcode.ErrorCode_DBError)
			}
			if rsp.Ret.RetCode != wantCode {
				t.Fatalf("ret_code=%d want %d", rsp.Ret.RetCode, wantCode)
			}
		})
	}
}

func TestAutoPauseChecksStateAfterQueuedResume(t *testing.T) {
	const sid = "sb-queued-auto-pause"
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	var stateMu, lifecycleMu sync.Mutex
	state := "pausing" // sweeper already acquired its marker
	r := &wrapredis.RedisWrap{RedisConnPool: &redis.Pool{}}
	patches.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return r })
	patches.ApplyMethod(r, "Do", func(_ *wrapredis.RedisWrap, cmd string, args ...interface{}) (interface{}, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if cmd == "SET" {
			state = args[1].(string)
			return "OK", nil
		}
		return []byte(state), nil
	})
	patches.ApplyFunc(config.GetConfig, func() *config.Config { return &config.Config{Common: &config.CommonConf{}} })
	localcache.SetSandboxCache(sid, &localcache.SandboxCache{SandboxID: sid, HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache(sid)
	var pauses atomic.Int32
	patches.ApplyFunc(pauseSandbox, func(context.Context, *types.UpdateRequest, string) *types.Res {
		pauses.Add(1)
		return successResumeRes()
	})
	queued := make(chan struct{})
	patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, opts sandboxlock.Options, fn func(context.Context) error) error {
		if opts.Value == "pause" {
			close(queued)
		}
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return fn(ctx)
	})
	// Model the real resume lock while Cubelet is restoring. The pending pause
	// must not inspect pausing until after the resume has published running.
	lifecycleMu.Lock()
	done := make(chan *types.Res, 1)
	go func() {
		done <- Update(context.Background(), &types.UpdateRequest{SandboxID: sid, InstanceType: "cubebox", Action: "pause", ExpectedLifecycleState: "pausing"})
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		lifecycleMu.Unlock()
		t.Fatal("pause did not reach lock")
	}
	if err := markResumedLifecycleState(sid); err != nil {
		lifecycleMu.Unlock()
		t.Fatal(err)
	}
	lifecycleMu.Unlock()
	rsp := <-done
	// Pin the wire literals, not the production constants: changing Master's
	// wording must not silently break CLM's superseded-pause classification.
	if rsp.Ret.RetCode != 130409 || rsp.Ret.RetMsg != "auto-pause superseded by lifecycle state change" || pauses.Load() != 0 {
		t.Fatalf("stale pause ran after resume: ret=%+v pauses=%d", rsp.Ret, pauses.Load())
	}
}

func TestAutoPauseStatePrecondition(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	r := &wrapredis.RedisWrap{RedisConnPool: &redis.Pool{}}
	patches.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return r })
	var state string
	var readErr error
	patches.ApplyMethod(r, "Do", func(_ *wrapredis.RedisWrap, _ string, _ ...interface{}) (interface{}, error) {
		return []byte(state), readErr
	})
	for _, value := range []string{"pausing", "running", "paused", "resuming", ""} {
		state = value
		err := checkPauseLifecycleState("sbx", "pausing")
		if (err == nil) != (value == "pausing") {
			t.Fatalf("state=%q err=%v", value, err)
		}
	}
	readErr = errors.New("Redis unavailable")
	if err := checkPauseLifecycleState("sbx", "pausing"); !errors.Is(err, readErr) {
		t.Fatalf("lost Redis error: %v", err)
	}
	if err := markResumedLifecycleState("sbx"); !errors.Is(err, readErr) {
		t.Fatalf("resume synchronization silently failed: %v", err)
	}
}
