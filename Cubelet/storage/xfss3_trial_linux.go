// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package storage

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// runXFSS3TrialProcess runs cube-runtime in a network namespace created at
// clone, so the trial guest's tap and every socket it opens live in an empty
// namespace from the first instruction on. It dies with Cubelet, runs in its
// own process group so a timeout kills everything it started, and reports
// whether the process is confirmed gone.
func runXFSS3TrialProcess(ctx context.Context, dir string, attempt *xfss3TrialAttempt, args []string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, xfss3TrialTimeout)
	defer cancel()
	out := &limitedBuffer{limit: xfss3TrialOutputLimit}
	cmd := exec.Command(xfss3RuntimeBin(), args...) // NOCC:Command Injection()
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET,
		Pdeathsig:  syscall.SIGKILL,
		Setpgid:    true,
	}
	if err := cmd.Start(); err != nil {
		return nil, true, fmt.Errorf("start trial runtime: %w", err)
	}
	attempt.PID = cmd.Process.Pid
	_ = writeXFSS3TrialAttempt(dir, attempt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
		err = fmt.Errorf("trial runtime killed: %w", ctx.Err())
	}
	// The group may hold helpers the runtime forked; none may survive the
	// verdict.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	released := processGone(cmd.Process.Pid, attempt.TrialID)
	return out.Bytes(), released, err
}
