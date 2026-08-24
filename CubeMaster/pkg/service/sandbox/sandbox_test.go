// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRunAsUser(t *testing.T) {
	content := `{
		"name": "test-container",
		"security_context":{
			"run_as_user":{}
		}
	}`
	req := types.Container{}
	if err := json.Unmarshal([]byte(content), &req); err != nil {
		t.Fatal(err)
	}
	t.Logf("req: %+v\n", req)
	securityContext := req.SecurityContext
	if securityContext == nil {
		t.Fatal("securityContext is nil")
	}
	if utils.SafeValue(req.SecurityContext).RunAsUser == nil {
		t.Fatal("RunAsUser is nil")
	}
	userstr := strconv.FormatInt(securityContext.RunAsUser.Value, 10)
	if userstr != "0" {
		t.Fatalf("RunAsUser is %s, want 0", userstr)
	}
}

func TestLogContext(t *testing.T) {
	rt := &CubeLog.RequestTrace{
		CalleeEndpoint: "localhost",
	}
	ctx := CubeLog.WithRequestTrace(context.TODO(), rt)
	ctx = log.WithLogger(ctx, CubeLog.WithContext(ctx))

	testctx(t, ctx)
	if rt.RequestID != "testid" {
		t.Errorf("rt.RequestID = %s, want %s", rt.RequestID, "testid")
	}
}
func testctx(t *testing.T, ctx context.Context) {
	type args struct {
		ctx context.Context
	}
	tests := &args{
		ctx: ctx,
	}
	defer func() {
		v := tests.ctx.Value("RequestId").(string)
		if v != "testid" {
			t.Errorf("RequestId should be test, but got %s", v)
		}
	}()
	tests.ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
		"RequestId": "testid",
	}))
	tests.ctx = context.WithValue(tests.ctx, "RequestId", "testid")
	rtInCtx := CubeLog.GetTraceInfo(ctx)
	rtInCtx.RequestID = "testid"
}

func TestReqResource(t *testing.T) {
	cpu, _ := resource.ParseQuantity(fmt.Sprintf("%f", (10*1.0)/100.0))

	assert.Equal(t, cpu.String(), "100m")

	cpu, _ = resource.ParseQuantity(fmt.Sprintf("%f", (500*1.0)/100.0))

	assert.Equal(t, cpu.String(), "5")
}
func TestBackoffRetryDelay(t *testing.T) {
	cfg := config.GetConfig().CubeletConf
	maxDelay := time.Duration(cfg.MaxDelayInSecond) * time.Second

	c := &createSandboxContext{}

	// First call seeds the base delay.
	c.backoffRetryDelay()
	assert.Equal(t, cfg.BackoffRetryDelay, c.delay, "first backoff should be the base delay")

	// Growth uses a random factor in [1.0, 1.8), so the exact delay at any
	// given iteration is non-deterministic. Assert only the invariants that
	// hold for every RNG sequence: the delay is monotonic non-decreasing and
	// never exceeds the cap.
	prev := c.delay
	for i := 1; i < 21; i++ {
		c.backoffRetryDelay()
		assert.GreaterOrEqual(t, c.delay, prev, "backoff must not shrink")
		assert.LessOrEqual(t, c.delay, maxDelay, "backoff must not exceed the cap")
		prev = c.delay
	}

	// Once at the cap, further growth stays clamped regardless of the factor.
	c.delay = maxDelay
	c.backoffRetryDelay()
	assert.Equal(t, maxDelay, c.delay, "backoff must stay clamped at the cap")
}

func init() {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mydir=%s\n", mydir)
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Clean(filepath.Join(mydir, "../../../conf.yaml")))
	}
	config.Init()
}

func TestBackoffDelay(t *testing.T) {
	c := &createSandboxContext{}
	for i := 0; i < 10; i++ {
		c.backoffRetryDelay()
		t.Logf("i:%d, c.backoffRetryDelay() = %v", i, c.delay)
	}
}

func TestCheckAndGetProbe(t *testing.T) {
	tests := []struct {
		name          string
		container     *types.Container
		expectedError error
		expectedProbe *cubebox.Probe
	}{
		{
			name:          "Probe is nil",
			container:     &types.Container{},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "ProbeHandler is nil",
			container: &types.Container{
				Probe: &types.Probe{
					PeriodMs:         1000,
					SuccessThreshold: 1,
					FailureThreshold: 1,
				},
			},
			expectedError: fmt.Errorf("ProbeHandler is nil"),
			expectedProbe: nil,
		},
		{
			name: "InitialDelaySeconds not zero",
			container: &types.Container{
				Probe: &types.Probe{
					PeriodMs:            1000,
					SuccessThreshold:    1,
					FailureThreshold:    1,
					ProbeHandler:        &types.ProbeHandler{},
					InitialDelaySeconds: 5,
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.Probe{
				PeriodMs:         1000,
				SuccessThreshold: 1,
				FailureThreshold: 1,
				ProbeHandler:     &cubebox.ProbeHandler{},
				InitialDelayMs:   5000,
			},
		},
		{
			name: "TimeoutSeconds not zero",
			container: &types.Container{
				Probe: &types.Probe{
					PeriodMs:         1000,
					SuccessThreshold: 1,
					FailureThreshold: 1,
					ProbeHandler:     &types.ProbeHandler{},
					TimeoutSeconds:   3,
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.Probe{
				PeriodMs:         1000,
				SuccessThreshold: 1,
				FailureThreshold: 1,
				ProbeHandler:     &cubebox.ProbeHandler{},
				TimeoutMs:        3000,
			},
		},
		{
			name: "TimeoutMs not zero",
			container: &types.Container{
				Probe: &types.Probe{
					PeriodMs:         1000,
					SuccessThreshold: 1,
					FailureThreshold: 1,
					ProbeHandler:     &types.ProbeHandler{},
					TimeoutMs:        3000,
					ProbeTimeoutMs:   3000,
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.Probe{
				PeriodMs:         1000,
				SuccessThreshold: 1,
				FailureThreshold: 1,
				ProbeHandler:     &cubebox.ProbeHandler{},
				TimeoutMs:        3000,
				ProbeTimeoutMs:   3000,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cubebox.ContainerConfig{}
			err := checkAndGetProbe(c, tt.container)
			assert.Equal(t, tt.expectedError, err)
			assert.Equal(t, tt.expectedProbe, c.Probe)
		})
	}
}

func TestCheckAndGetPrestop(t *testing.T) {
	tests := []struct {
		name          string
		container     *types.Container
		expectedError error
		expectedProbe *cubebox.PreStop
	}{
		{
			name:          "Prestop is nil",
			container:     &types.Container{},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "LifecyleHandler is nil",
			container: &types.Container{
				Prestop: &types.PreStop{
					TerminationGracePeriodMs: 1000,
				},
			},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "LifecyleHandler.HttpGet is nil",
			container: &types.Container{
				Prestop: &types.PreStop{
					TerminationGracePeriodMs: 1000,
					LifecyleHandler:          &types.LifecycleHandler{},
				},
			},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "TerminationGracePeriodMs not zero",
			container: &types.Container{
				Prestop: &types.PreStop{
					TerminationGracePeriodMs: 1000,
					LifecyleHandler: &types.LifecycleHandler{
						HttpGet: &types.HTTPGetAction{
							Path: utils.StringPtr("/"),
							Port: 8080,
						},
					},
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.PreStop{
				TerminationGracePeriodMs: 1000,
				LifecyleHandler: &cubebox.LifecycleHandler{
					HttpGet: &cubebox.HTTPGetAction{
						Path: utils.StringPtr("/"),
						Port: 8080,
					},
				},
			},
		},
		{
			name: "LifecyleHandler.HttpHeaders not nil",
			container: &types.Container{
				Prestop: &types.PreStop{
					TerminationGracePeriodMs: 1000,
					LifecyleHandler: &types.LifecycleHandler{
						HttpGet: &types.HTTPGetAction{
							HttpHeaders: []*types.HTTPHeader{
								{
									Name:  utils.StringPtr("header1"),
									Value: utils.StringPtr("value1"),
								},
							},
						},
					},
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.PreStop{
				TerminationGracePeriodMs: 1000,
				LifecyleHandler: &cubebox.LifecycleHandler{
					HttpGet: &cubebox.HTTPGetAction{
						HttpHeaders: []*cubebox.HTTPHeader{
							{
								Name:  utils.StringPtr("header1"),
								Value: utils.StringPtr("value1"),
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cubebox.ContainerConfig{}
			err := checkAndGetProbe(c, tt.container)
			assert.Equal(t, tt.expectedError, err)
			assert.Equal(t, tt.expectedProbe, c.Prestop)
		})
	}
}

func TestCheckAndGetPoststop(t *testing.T) {
	tests := []struct {
		name          string
		container     *types.Container
		expectedError error
		expectedProbe *cubebox.PostStop
	}{
		{
			name:          "Poststop is nil",
			container:     &types.Container{},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "LifecyleHandler is nil",
			container: &types.Container{
				Poststop: &types.PostStop{
					TimeoutMs: 1000,
				},
			},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "LifecyleHandler.HttpGet is nil",
			container: &types.Container{
				Poststop: &types.PostStop{
					TimeoutMs:       1000,
					LifecyleHandler: &types.LifecycleHandler{},
				},
			},
			expectedError: nil,
			expectedProbe: nil,
		},
		{
			name: "TimeoutMs not zero",
			container: &types.Container{
				Poststop: &types.PostStop{
					TimeoutMs: 1000,
					LifecyleHandler: &types.LifecycleHandler{
						HttpGet: &types.HTTPGetAction{
							Path: utils.StringPtr("/"),
							Port: 8080,
						},
					},
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.PostStop{
				TimeoutMs: 1000,
				LifecyleHandler: &cubebox.LifecycleHandler{
					HttpGet: &cubebox.HTTPGetAction{
						Path: utils.StringPtr("/"),
						Port: 8080,
					},
				},
			},
		},
		{
			name: "LifecyleHandler.HttpHeaders not nil",
			container: &types.Container{
				Poststop: &types.PostStop{
					TimeoutMs: 1000,
					LifecyleHandler: &types.LifecycleHandler{
						HttpGet: &types.HTTPGetAction{
							HttpHeaders: []*types.HTTPHeader{
								{
									Name:  utils.StringPtr("header1"),
									Value: utils.StringPtr("value1"),
								},
							},
						},
					},
				},
			},
			expectedError: nil,
			expectedProbe: &cubebox.PostStop{
				TimeoutMs: 1000,
				LifecyleHandler: &cubebox.LifecycleHandler{
					HttpGet: &cubebox.HTTPGetAction{
						HttpHeaders: []*cubebox.HTTPHeader{
							{
								Name:  utils.StringPtr("header1"),
								Value: utils.StringPtr("value1"),
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cubebox.ContainerConfig{}
			err := checkAndGetProbe(c, tt.container)
			assert.Equal(t, tt.expectedError, err)
			assert.Equal(t, tt.expectedProbe, c.Poststop)
		})
	}
}

func TestHandleLifecycleHooks(t *testing.T) {
	strPtr := func(s string) *string { return &s }

	t.Run("nil source is no-op", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		require.NoError(t, handleLifecycleHooks(&dst, nil))
		assert.Nil(t, dst)
	})

	t.Run("exec hook maps command and ABORT default policy", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		require.NoError(t, handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PrePause: &types.LifecycleHook{
				TimeoutMs: 5000,
				Handler: &types.LifecycleHookHandler{
					Exec: &types.ExecAction{Command: []string{"/usr/bin/flush", "--now"}, WorkingDir: strPtr("/app")},
				},
				// FailurePolicy empty → must default to ABORT.
			},
		}))
		require.NotNil(t, dst)
		require.NotNil(t, dst.PrePause)
		assert.Equal(t, int32(5000), dst.PrePause.TimeoutMs)
		assert.Equal(t, cubebox.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT, dst.PrePause.FailurePolicy)
		require.NotNil(t, dst.PrePause.Handler.Exec)
		assert.Equal(t, []string{"/usr/bin/flush", "--now"}, dst.PrePause.Handler.Exec.Command)
		assert.Equal(t, "/app", *dst.PrePause.Handler.Exec.WorkingDir)
		assert.Nil(t, dst.PrePause.Handler.HttpGet)
		assert.Nil(t, dst.PostResume)
	})

	t.Run("http hook maps port/path/headers and IGNORE policy", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		require.NoError(t, handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PostResume: &types.LifecycleHook{
				TimeoutMs:     2000,
				FailurePolicy: types.HookFailurePolicyIgnore,
				Handler: &types.LifecycleHookHandler{
					HttpGet: &types.HTTPGetAction{
						Path: strPtr("/ready"),
						Port: 49983,
						HttpHeaders: []*types.HTTPHeader{
							{Name: strPtr("X-Custom"), Value: strPtr("v")},
						},
					},
				},
			},
		}))
		require.NotNil(t, dst)
		require.NotNil(t, dst.PostResume)
		assert.Equal(t, cubebox.HookFailurePolicy_HOOK_FAILURE_POLICY_IGNORE, dst.PostResume.FailurePolicy)
		require.NotNil(t, dst.PostResume.Handler.HttpGet)
		assert.Equal(t, int32(49983), dst.PostResume.Handler.HttpGet.Port)
		assert.Equal(t, "/ready", *dst.PostResume.Handler.HttpGet.Path)
		require.Len(t, dst.PostResume.Handler.HttpGet.HttpHeaders, 1)
		assert.Nil(t, dst.PostResume.Handler.Exec)
	})

	t.Run("failure policy is case-insensitive", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		require.NoError(t, handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PrePause: &types.LifecycleHook{
				TimeoutMs: 1000, FailurePolicy: "ignore",
				Handler: &types.LifecycleHookHandler{Exec: &types.ExecAction{Command: []string{"true"}}},
			},
		}))
		require.NotNil(t, dst.PrePause)
		assert.Equal(t, cubebox.HookFailurePolicy_HOOK_FAILURE_POLICY_IGNORE, dst.PrePause.FailurePolicy,
			"lowercase \"ignore\" must map to IGNORE, not silently become blocking ABORT")
	})

	t.Run("hook with nil handler is dropped", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		require.NoError(t, handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PrePause: &types.LifecycleHook{TimeoutMs: 1000},
		}))
		require.NotNil(t, dst)
		assert.Nil(t, dst.PrePause, "a hook with no handler must not produce a wire hook")
	})

	t.Run("rejects invalid timeout_ms at create time", func(t *testing.T) {
		for _, ms := range []int32{0, -1, 30001} {
			var dst *cubebox.LifecycleHooks
			err := handleLifecycleHooks(&dst, &types.LifecycleHooks{
				PrePause: &types.LifecycleHook{
					TimeoutMs: ms,
					Handler:   &types.LifecycleHookHandler{Exec: &types.ExecAction{Command: []string{"true"}}},
				},
			})
			require.Error(t, err, "timeout_ms=%d must be rejected", ms)
			assert.Contains(t, err.Error(), "timeout_ms")
		}
	})

	t.Run("rejects empty exec command at create time", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		err := handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PrePause: &types.LifecycleHook{
				TimeoutMs: 1000,
				Handler:   &types.LifecycleHookHandler{Exec: &types.ExecAction{Command: []string{}}},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "command is empty")
	})

	t.Run("rejects handler with both exec and http_get", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		err := handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PrePause: &types.LifecycleHook{
				TimeoutMs: 1000,
				Handler: &types.LifecycleHookHandler{
					Exec:    &types.ExecAction{Command: []string{"true"}},
					HttpGet: &types.HTTPGetAction{Port: 49983},
				},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both exec and http_get")
	})

	t.Run("rejects http_get with non-positive port", func(t *testing.T) {
		var dst *cubebox.LifecycleHooks
		err := handleLifecycleHooks(&dst, &types.LifecycleHooks{
			PostResume: &types.LifecycleHook{
				TimeoutMs: 1000,
				Handler:   &types.LifecycleHookHandler{HttpGet: &types.HTTPGetAction{Port: 0}},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port must be > 0")
	})

	t.Run("wired through checkAndGetProbe", func(t *testing.T) {
		c := &cubebox.ContainerConfig{}
		err := checkAndGetProbe(c, &types.Container{
			LifecycleHooks: &types.LifecycleHooks{
				PrePause: &types.LifecycleHook{
					TimeoutMs: 1000,
					Handler: &types.LifecycleHookHandler{
						Exec: &types.ExecAction{Command: []string{"true"}},
					},
				},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, c.LifecycleHooks)
		require.NotNil(t, c.LifecycleHooks.PrePause)
		require.NotNil(t, c.LifecycleHooks.PrePause.Handler.Exec)
	})
}
