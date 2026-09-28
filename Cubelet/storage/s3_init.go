// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// s3InitRetryInterval is the pause between failed S3 init attempts.
var s3InitRetryInterval = 5 * time.Second

// ErrS3NotReady is returned for backend=s3 requests while the S3 cubecow
// handle and metadata base are still initializing (or retrying).
var ErrS3NotReady = errors.New("s3 storage is not ready (initializing)")

// ErrS3NotConfigured is returned for backend=s3 requests when the operator
// has not opted into CubeS3lvol ([cow.s3] enable is false).
var ErrS3NotConfigured = errors.New("s3 storage is not configured (set [cow.s3] enable = true to enable CubeS3lvol)")

// ensureS3MetadataReadyFn is swapped in tests to avoid real mkfs/mount.
var ensureS3MetadataReadyFn = EnsureS3MetadataReady

// s3CowOverride lets the init loop run EnsureS3MetadataReady before
// publishing s3CowManager (so storeForBackend still returns ErrS3NotReady).
var (
	s3CowOverrideMu sync.Mutex
	s3CowOverride   *S3Cow
)

func (l *local) startS3CowInitLoop(parent context.Context) {
	if l == nil || !l.useCowStorage() {
		return
	}
	if l.config == nil || !l.config.s3lvolConfigured() {
		CubeLog.Infof("s3 cubecow init skipped; set [cow.s3] enable = true to enable")
		return
	}
	l.stopS3CowInitLoop()
	ctx, cancel := context.WithCancel(parent)
	l.s3InitCancel = cancel
	// Warn, not info: cubelet drops the log level to the configured one
	// (warn on our nodes) as soon as boot finishes, which is about when
	// this loop starts. At info, both ends of the window would vanish and
	// a node stuck without s3lvol would look identical to a healthy one.
	CubeLog.Warnf("s3 cubecow init loop starting; s3 requests fail with %v until it succeeds", ErrS3NotReady)
	go l.runS3CowInitLoop(ctx)
}

func (l *local) stopS3CowInitLoop() {
	if l == nil || l.s3InitCancel == nil {
		return
	}
	l.s3InitCancel()
	l.s3InitCancel = nil
}

func (l *local) runS3CowInitLoop(ctx context.Context) {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			CubeLog.Warnf("s3 cubecow init loop stopped before attempt %d: %v", attempt, ctx.Err())
			return
		}
		CubeLog.Infof("s3 cubecow init attempt %d", attempt)
		if err := l.tryS3CowInitOnce(ctx); err != nil {
			CubeLog.Errorf("s3 cubecow init attempt %d fail (retry in %s): %v", attempt, s3InitRetryInterval, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(s3InitRetryInterval):
			}
			continue
		}
		CubeLog.Warnf("cubecow s3 handle and metadata base ready after %d attempt(s) in %s; s3 requests now served",
			attempt, time.Since(start).Round(time.Millisecond))
		// Boot recovery is deliberately not re-run here: it does nothing for
		// an s3-backed entry (see recoverStorageInfo), so there is nothing
		// waiting on this handle.
		return
	}
}

// tryS3CowInitOnce initializes the S3 cubecow engine and metadata base.
// On success it publishes s3CowEngine / s3CowManager. On failure it leaves
// them unset (and closes any partial engine).
func (l *local) tryS3CowInitOnce(ctx context.Context) error {
	if l == nil {
		return fmt.Errorf("storage is not initialized")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if l.s3CowManager != nil && l.s3CowEngine != nil {
		CubeLog.Infof("s3 cubecow handle already published; nothing to do")
		return nil
	}

	l.clearS3Cow()

	engine, source, err := initS3CowEngine(l.config)
	if err != nil {
		return fmt.Errorf("s3 cubecow handle init: %w", err)
	}
	mgr := newS3CowVolumeManager(engine)

	// Logged as its own step: it talks to s3lvol and is where a wedged
	// backend leaves the loop sitting, with the handle not yet published.
	CubeLog.Infof("s3 cubecow handle open (%s); ensuring metadata base", source)
	setS3CowOverride(mgr)
	metaErr := ensureS3MetadataReadyFn(ctx)
	setS3CowOverride(nil)
	if metaErr != nil {
		engine.Close()
		return fmt.Errorf("s3 metadata base init: %w", metaErr)
	}
	if ctx.Err() != nil {
		engine.Close()
		return ctx.Err()
	}

	l.s3CowEngine = engine
	l.s3CowManager = mgr
	CubeLog.Infof("cubecow s3 handle initialized from %s", source)
	return nil
}

func setS3CowOverride(m *S3Cow) {
	s3CowOverrideMu.Lock()
	s3CowOverride = m
	s3CowOverrideMu.Unlock()
}

func (l *local) clearS3Cow() {
	if l == nil {
		return
	}
	setS3CowOverride(nil)
	if l.s3CowEngine != nil {
		l.s3CowEngine.Close()
		l.s3CowEngine = nil
	}
	l.s3CowManager = nil
}
