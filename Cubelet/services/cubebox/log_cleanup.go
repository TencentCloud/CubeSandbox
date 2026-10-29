// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/logcleanup"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const delayedShimLogCleanupEnv = "CUBELET_DELAY_SANDBOX_LOG_CLEANUP"

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
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := logcleanup.Scan(ctx, logcleanup.Root, s.cubeboxMgr.config.logRetention, now, s.guardDelayedLogCleanup); err != nil {
				CubeLog.Warnf("delayed sandbox log cleanup: %v", err)
			}
		}
	}
}

func (s *service) guardDelayedLogCleanup(ctx context.Context, id string, remove func() error) error {
	unlock, err := s.sandboxLifecycleLocks.LockContext(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	// A record includes paused sandboxes and any in-progress teardown. Keep
	// their logs until the record is gone; unknown state also fails closed.
	_, sandboxErr := s.cubeboxMgr.cubeboxManger.Get(ctx, id)
	safe, err := logCleanupEligible(sandboxErr, func() error {
		if s.cubeboxMgr.shims == nil {
			return errors.New("shim manager unavailable")
		}
		_, shimErr := s.cubeboxMgr.shims.Get(namespaces.WithNamespace(ctx, namespaces.Default), id)
		return shimErr
	})
	if err != nil || !safe {
		return err
	}
	return remove()
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
