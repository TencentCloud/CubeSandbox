// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/logcleanup"
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

func TestDelayedLogCleanupContinuesAfterScanPanic(t *testing.T) {
	ticks := make(chan time.Time, 2)
	ticks <- time.Now()
	ticks <- time.Now()
	close(ticks)
	done := make(chan struct{})
	calls := 0
	go func() {
		defer close(done)
		runDelayedLogCleanupLoop(context.Background(), ticks, func(time.Time) error {
			calls++
			if calls == 1 {
				panic("simulated cleanup panic")
			}
			return nil
		})
	}()
	select {
	case <-done:
		if calls != 2 {
			t.Fatalf("expected a later scan after panic, got %d calls", calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup loop did not finish")
	}
}

func TestMarkSandboxLogsHonorsFeatureFlag(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l := &local{config: &CubeConfig{}}
	l.markSandboxLogsAt(context.Background(), root, "sandbox", "")
	if _, err := os.Stat(filepath.Join(dir, ".delayed-cleanup")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled cleanup wrote a marker: %v", err)
	}
	l.config.SandboxLogCleanupEnabled = true
	l.markSandboxLogsAt(context.Background(), root, "sandbox", "")
	ns, err := logcleanup.Namespace(root, "sandbox")
	if err != nil || ns != namespaces.Default {
		t.Fatalf("enabled cleanup marker namespace=%q error=%v", ns, err)
	}
}

func TestLookupMarkedShimUsesSandboxNamespace(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sandbox"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := logcleanup.Mark(root, "sandbox", "nondefault"); err != nil {
		t.Fatal(err)
	}
	called := false
	err := lookupMarkedShim(context.Background(), root, "sandbox", func(ctx context.Context, id string) error {
		called = true
		if ns, _ := namespaces.Namespace(ctx); ns != "nondefault" || id != "sandbox" {
			t.Fatalf("namespace=%q id=%q", ns, id)
		}
		return nil // a live shim prevents cleanup
	})
	if err != nil || !called {
		t.Fatalf("called=%v error=%v", called, err)
	}
	if err := os.WriteFile(filepath.Join(root, "sandbox", ".delayed-cleanup"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	called = false
	err = lookupMarkedShim(context.Background(), root, "sandbox", func(context.Context, string) error { called = true; return errdefs.ErrNotFound })
	if err == nil || called {
		t.Fatalf("unknown namespace must retain logs: called=%v error=%v", called, err)
	}
}

func TestScanKeepsRegisteredShimInNondefaultNamespace(t *testing.T) {
	root := t.TempDir()
	id := "sandbox"
	if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := logcleanup.Mark(root, id, "nondefault"); err != nil {
		t.Fatal(err)
	}
	shimExists := true
	guard := func(ctx context.Context, id string, remove func() error) error {
		safe, err := logCleanupEligible(utils.ErrorKeyNotFound, func() error {
			return lookupMarkedShim(ctx, root, id, func(ctx context.Context, _ string) error {
				ns, _ := namespaces.Namespace(ctx)
				if ns == "nondefault" && shimExists {
					return nil
				}
				return errdefs.ErrNotFound
			})
		})
		if err != nil || !safe {
			return err
		}
		return remove()
	}
	now := time.Now().Add(time.Hour)
	if err := logcleanup.Scan(context.Background(), root, time.Second, now, guard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); err != nil {
		t.Fatalf("live shim logs removed: %v", err)
	}
	shimExists = false
	if err := logcleanup.Scan(context.Background(), root, time.Second, now, guard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped shim logs not removed: %v", err)
	}
}

func TestLockedSandboxDoesNotBlockOtherLogCleanup(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"a-locked", "b-ready"} {
		if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := logcleanup.Mark(root, id, "default"); err != nil {
			t.Fatal(err)
		}
	}
	locks := utils.NewResourceLocks()
	unlock := locks.Lock("a-locked")
	defer unlock()
	err := logcleanup.Scan(context.Background(), root, time.Second, time.Now().Add(time.Hour),
		func(ctx context.Context, id string, remove func() error) error {
			return withLogCleanupLock(ctx, locks, id, remove)
		})
	if err != nil {
		t.Fatalf("busy sandbox must be skipped without an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a-locked")); err != nil {
		t.Fatalf("locked logs removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b-ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ready logs not removed: %v", err)
	}
}

func TestLogCleanupLockPreservesCancellationAndCallbackErrors(t *testing.T) {
	locks := utils.NewResourceLocks()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withLogCleanupLock(ctx, locks, "sandbox", func() error { t.Fatal("callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	unlock := locks.Lock("sandbox")
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := withLogCleanupLock(deadlineCtx, locks, "sandbox", func() error { t.Fatal("callback ran"); return nil })
	deadlineCancel()
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected caller deadline, got %v", err)
	}
	want := errors.New("cleanup failed")
	if err := withLogCleanupLock(context.Background(), locks, "sandbox", func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("expected callback error, got %v", err)
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

func TestDelayedCleanupScanBlocksCreateWithReusedID(t *testing.T) {
	locks := utils.NewResourceLocks()
	s := &service{
		cubeboxMgr:            &local{config: &CubeConfig{SandboxLogCleanupEnabled: true}},
		sandboxLifecycleLocks: locks,
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("cleanup lock: %v", err)
		}
	}()
	go func() {
		done <- withLogCleanupLock(context.Background(), locks, "sandbox-id", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not acquire the lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rsp, err := s.Create(ctx, &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{constants.MasterAnnotationDesiredSandboxID: "sandbox-id"},
	})
	if err != nil || rsp.Ret.RetCode != errorcode.ErrorCode_TaskStateInvalid {
		t.Fatalf("expected Create to wait for cleanup: response=%v error=%v", rsp, err)
	}
}

func TestDelayedCleanupRejectsUnsafeDesiredIDsBeforeLocking(t *testing.T) {
	locks := utils.NewResourceLocks()
	s := &service{
		cubeboxMgr:            &local{config: &CubeConfig{SandboxLogCleanupEnabled: true}},
		sandboxLifecycleLocks: locks,
	}
	for _, id := range []string{" sandbox", "sandbox ", " ", ".", "../sandbox", "bad\x00id"} {
		rsp, err := s.Create(context.Background(), &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{constants.MasterAnnotationDesiredSandboxID: id},
		})
		if err != nil || rsp.Ret.RetCode != errorcode.ErrorCode_InvalidParamFormat {
			t.Fatalf("ID %q: response=%v error=%v", id, rsp, err)
		}
		if locks.Len() != 0 {
			t.Fatalf("ID %q unexpectedly took a lifecycle lock", id)
		}
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
