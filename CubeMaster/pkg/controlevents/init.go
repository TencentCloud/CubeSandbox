// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controlevents

import (
	"context"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

// Init verifies Redis connectivity (fatal for the caller on failure), installs
// the package-level publisher, and starts a broadcast consumer goroutine.
//
// Unlike lifecycle.Init, Redis is a hard dependency: Cubemaster must not serve
// without the control-plane fan-out channel.
func Init(ctx context.Context, apply ApplyFunc) error {
	pool := wrapredis.GetRedis()
	if isNilPool(pool) {
		return fmt.Errorf("controlevents: redis pool unavailable")
	}
	if _, err := pool.Do("PING"); err != nil {
		return fmt.Errorf("controlevents: redis ping failed: %w", err)
	}
	// Confirm the Redis server accepts stream commands. TYPE on a missing key
	// returns "none"; any transport/command error is fatal at startup.
	if _, err := pool.Do("TYPE", EventStreamKey); err != nil {
		return fmt.Errorf("controlevents: redis TYPE %s failed: %w", EventStreamKey, err)
	}

	pub := NewPublisher(pool)
	setDefaultPublisher(pub)

	handler := NewIsolationHandler(apply)
	consumer := NewConsumer(pool.RedisConnPool, handler)
	recov.GoWithRecover(func() {
		consumer.Run(ctx)
	})

	log.G(ctx).Infof("controlevents: ready (stream=%s)", EventStreamKey)
	return nil
}

func isNilPool(w *wrapredis.RedisWrap) bool {
	return w == nil || w.RedisConnPool == nil
}
