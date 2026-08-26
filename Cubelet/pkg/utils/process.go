// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

// ProcessAlive is a single kill(pid, 0) probe. It does not wait.
// A zombie is treated as gone: it is no longer running and cannot hold a
// volume, even if the parent has not reaped the pid yet.
func ProcessAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
		return false
	}
	return !processZombie(pid)
}

func processZombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// comm is inside parentheses and may contain spaces; state follows ') '.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] == 'Z'
}

// WaitProcessGone blocks until pid is gone or ctx is done.
// Missing / invalid pids succeed immediately so callers can pass a captured
// list after the process has already exited.
func WaitProcessGone(ctx context.Context, pid int) error {
	if !ProcessAlive(pid) {
		return nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !ProcessAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("process %d still running: %w", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

func ProcessExists(ctx context.Context, pid int) bool {
	if pid <= 0 {
		return false
	}
	ctxTmp, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	for {
		select {
		case <-ctxTmp.Done():
			log.G(ctx).Warnf("process[%d] check timeout,still exist,%v", pid, ctxTmp.Err())
			return true
		default:

			if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
				log.G(ctx).Debugf("process[%d] not exist,%v", pid, err)
				return false
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}
