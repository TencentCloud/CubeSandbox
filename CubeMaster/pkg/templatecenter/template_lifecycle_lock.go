// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
)

const templateLifecycleLockTimeout = 2 * time.Minute

var templateLifecycleLocalLocks sync.Map // map[templateID]*sync.Mutex

func templateLifecycleLockName(templateID string) string {
	sum := sha256.Sum256([]byte(templateID))
	return "tc_tpl_lifecycle_" + hex.EncodeToString(sum[:16])
}

func withTemplateLifecycleLocalLock(templateID string, fn func() error) error {
	muV, _ := templateLifecycleLocalLocks.LoadOrStore(templateID, &sync.Mutex{})
	mu := muV.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

// withTemplateLifecycleLock serializes eager JFS adoption with template
// deletion across CubeMaster replicas. The existing in-process template lock
// cannot protect a fan-out running on one Master from force deletion handled
// by another.
func withTemplateLifecycleLock(ctx context.Context, templateID string, fn func() error) error {
	return withTemplateLifecycleLocalLock(templateID, func() error {
		if !isReady() {
			return fn()
		}
		lockName := templateLifecycleLockName(templateID)
		return store.db.WithContext(ctx).Connection(func(sess *gorm.DB) error {
			dialect := sess.Dialector.Name()
			if dialect != "mysql" && dialect != "postgres" {
				log.G(ctx).Debugf("template lifecycle lock skipped on dialect %s", dialect)
				return fn()
			}
			locked, err := pollSessionLock(ctx, templateLifecycleLockTimeout, artifactRegisterLockPoll, func() (bool, error) {
				return trySessionLock(sess, lockName)
			})
			if err != nil {
				return errors.Join(fmt.Errorf("acquire template lifecycle lock: %w", err), discardPinnedSession(sess))
			}
			if !locked {
				return fmt.Errorf("template lifecycle lock %q held for over %s", lockName, templateLifecycleLockTimeout)
			}
			defer func() {
				releaseSess := pinnedSessionWithContext(sess, context.Background())
				if _, releaseErr := releaseSessionLock(releaseSess, lockName); releaseErr != nil {
					_ = discardPinnedSession(sess)
				}
			}()
			return fn()
		})
	})
}

func withActiveJFSTemplateLifecycle(ctx context.Context, jobID, templateID string, fn func() error) (bool, error) {
	ran := false
	err := eagerAdoptJFSLifecycle(ctx, templateID, func() error {
		active, err := eagerAdoptJFSJobActive(ctx, jobID)
		if err != nil || !active {
			return err
		}
		ran = true
		return fn()
	})
	return ran, err
}
