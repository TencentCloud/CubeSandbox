// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package app

import (
	"context"
	"os"
	"syscall"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var handledSignals = []os.Signal{
	syscall.SIGTERM,
	syscall.SIGINT,
	syscall.SIGHUP,
}

func handleSignals(ctx context.Context, signals chan os.Signal, cancel context.CancelFunc) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-signals:
				CubeLog.WithContext(ctx).Infof("templatecenter received signal %v, shutting down", sig)
				cancel()
				return
			}
		}
	}()
	return done
}
