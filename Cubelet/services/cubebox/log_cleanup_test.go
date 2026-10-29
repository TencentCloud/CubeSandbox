// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestDelayedLogCleanupConfiguration(t *testing.T) {
	t.Setenv(delayedShimLogCleanupEnv, "1")
	cfg := defaultConfig()
	if cfg.SandboxLogCleanupEnabled {
		t.Fatal("delayed cleanup must default to disabled")
	}
	if err := cfg.configureLogCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv(delayedShimLogCleanupEnv); ok {
		t.Fatal("disabled configuration must not pass retention to shim")
	}

	cfg.SandboxLogCleanupEnabled = true
	cfg.SandboxLogRetention = "2h"
	cfg.SandboxLogCleanupInterval = "5m"
	if err := cfg.configureLogCleanup(); err != nil {
		t.Fatal(err)
	}
	if cfg.logRetention != 2*time.Hour || cfg.logCleanupInterval != 5*time.Minute {
		t.Fatal("durations were not applied")
	}
	if os.Getenv(delayedShimLogCleanupEnv) != "1" {
		t.Fatal("enabled configuration must pass retention to shim")
	}
	for _, raw := range []string{"", "0s", "-1s", "invalid"} {
		if _, err := positiveDuration(raw, "retention"); err == nil {
			t.Fatalf("accepted invalid duration %q", raw)
		}
	}
}

func TestDelayedCleanupSerializesCreateWithReusedID(t *testing.T) {
	locks := utils.NewResourceLocks()
	s := &service{
		cubeboxMgr:            &local{config: &CubeConfig{SandboxLogCleanupEnabled: true}},
		sandboxLifecycleLocks: locks,
	}
	id := "sandbox-id"
	unlock := locks.Lock(id)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rsp, err := s.Create(ctx, &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{constants.MasterAnnotationDesiredSandboxID: id},
	})
	if err != nil || rsp.Ret.RetCode != errorcode.ErrorCode_TaskStateInvalid {
		t.Fatalf("expected lifecycle lock failure, got response=%v error=%v", rsp, err)
	}
	if !strings.Contains(rsp.Ret.RetMsg, "retry Create") {
		t.Fatalf("wrong retry message: %s", rsp.Ret.RetMsg)
	}
}

func TestLogCleanupRequiresAbsentSandboxAndShim(t *testing.T) {
	unknown := errors.New("lookup failed")
	for _, tc := range []struct {
		name       string
		sandboxErr error
		shimErr    error
		wantSafe   bool
		wantErr    bool
		wantLookup bool
	}{
		{"sandbox exists", nil, nil, false, false, false},
		{"sandbox unknown", unknown, nil, false, true, false},
		{"shim exists", utils.ErrorKeyNotFound, nil, false, false, true},
		{"shim unknown", utils.ErrorKeyNotFound, unknown, false, true, true},
		{"both absent", utils.ErrorKeyNotFound, errdefs.ErrNotFound, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookedUp := false
			safe, err := logCleanupEligible(tc.sandboxErr, func() error {
				lookedUp = true
				return tc.shimErr
			})
			if safe != tc.wantSafe || (err != nil) != tc.wantErr || lookedUp != tc.wantLookup {
				t.Fatalf("safe=%v err=%v lookedUp=%v", safe, err, lookedUp)
			}
		})
	}
}

func TestOrphanShimDeleteInheritsDelayedCleanup(t *testing.T) {
	t.Setenv(delayedShimLogCleanupEnv, "1")
	env := shimDeleteEnv(orphanShim{})
	want := delayedShimLogCleanupEnv + "=1"
	for _, item := range env {
		if item == want {
			return
		}
	}
	t.Fatalf("shim delete environment missing %s in %s", want, strings.Join(env, ","))
}
