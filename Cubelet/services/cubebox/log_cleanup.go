// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/logcleanup"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	delayedShimLogCleanupEnv = "CUBELET_DELAY_SANDBOX_LOG_CLEANUP"
	logCleanupLockTimeout    = 250 * time.Millisecond
)

func (l *local) markSandboxLogs(ctx context.Context, id, namespace string) {
	l.markSandboxLogsAt(ctx, logcleanup.Root, id, namespace)
}

func (l *local) markSandboxLogsAt(ctx context.Context, root, id, namespace string) {
	if l.config == nil || !l.config.SandboxLogCleanupEnabled {
		return
	}
	if namespace == "" {
		namespace = namespaces.Default
	}
	if err := logcleanup.Mark(root, id, namespace); errors.Is(err, logcleanup.ErrLogDirNotFound) {
		CubeLog.WithContext(ctx).Infof("sandbox log directory missing; no logs to retain for %s", id)
	} else if err != nil {
		CubeLog.WithContext(ctx).Warnf("mark sandbox logs for delayed cleanup %s: %v", id, err)
	}
}

func (c *CubeConfig) configureLogCleanup() error {
	if !c.SandboxLogCleanupEnabled {
		return os.Unsetenv(delayedShimLogCleanupEnv)
	}
	var err error
	c.logRetention, err = positiveDuration(c.SandboxLogRetention, "sandbox_log_retention")
	if err != nil {
		return err
	}
	c.logCleanupInterval, err = positiveDuration(c.SandboxLogCleanupInterval, "sandbox_log_cleanup_interval")
	if err != nil {
		return err
	}
	// ShimManager launches both the live shim and its delete command with
	// Cubelet's environment. This flag is intentionally absent when disabled.
	return os.Setenv(delayedShimLogCleanupEnv, "1")
}

func positiveDuration(raw, name string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

func (s *service) runDelayedLogCleanup(ctx context.Context) {
	ticker := time.NewTicker(s.cubeboxMgr.config.logCleanupInterval)
	defer ticker.Stop()
	runDelayedLogCleanupLoop(ctx, ticker.C, func(now time.Time) error {
		return logcleanup.Scan(ctx, logcleanup.Root, s.cubeboxMgr.config.logRetention, now, s.guardDelayedLogCleanup)
	})
}

// Recover each scan separately so a panic does not terminate Cubelet or stop
// subsequent cleanup ticks. Unremoved logs remain for the next scan.
func runDelayedLogCleanupLoop(ctx context.Context, ticks <-chan time.Time, scan func(time.Time) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			recov.WithRecover(func() {
				if err := scan(now); err != nil {
					CubeLog.Warnf("delayed sandbox log cleanup: %v", err)
				}
			}, func(panicError interface{}) {
				CubeLog.Errorf("delayed sandbox log cleanup panic: %v\n%s", panicError, debug.Stack())
			})
		}
	}
}

func (s *service) guardDelayedLogCleanup(ctx context.Context, id string, remove func() error) error {
	return withLogCleanupLock(ctx, s.sandboxLifecycleLocks, id, func() error {
		_, sandboxErr := s.cubeboxMgr.cubeboxManger.Get(ctx, id)
		safe, err := logCleanupEligible(sandboxErr, func() error {
			if s.cubeboxMgr.shims == nil {
				return errors.New("shim manager unavailable")
			}
			return lookupMarkedShim(ctx, logcleanup.Root, id, func(ctx context.Context, id string) error {
				_, err := s.cubeboxMgr.shims.Get(ctx, id)
				return err
			})
		})
		if err != nil || !safe {
			return err
		}
		return remove()
	})
}

func lookupMarkedShim(ctx context.Context, root, id string, lookup func(context.Context, string) error) error {
	namespace, err := logcleanup.Namespace(root, id)
	if err != nil {
		return fmt.Errorf("read cleanup marker namespace: %w", err)
	}
	return lookup(namespaces.WithNamespace(ctx, namespace), id)
}

func withLogCleanupLock(ctx context.Context, locks *utils.ResourceLocks, id string, fn func() error) error {
	lockCtx, cancel := context.WithTimeout(ctx, logCleanupLockTimeout)
	defer cancel()
	unlock, err := locks.LockContext(lockCtx, id)
	if err != nil {
		// A busy sandbox can be checked on the next scan. Preserve caller
		// cancellation, which is different from our per-entry wait timeout.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil
		}
		return err
	}
	defer unlock()
	return fn()
}

func logCleanupEligible(sandboxErr error, lookupShim func() error) (bool, error) {
	if sandboxErr == nil {
		return false, nil
	}
	if !errors.Is(sandboxErr, utils.ErrorKeyNotFound) {
		return false, sandboxErr
	}
	err := lookupShim()
	if err == nil {
		return false, nil
	}
	if !errdefs.IsNotFound(err) {
		return false, err
	}
	return true, nil
}
