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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
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
