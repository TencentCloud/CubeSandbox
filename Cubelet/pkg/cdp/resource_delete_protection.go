// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cdp

import (
	"context"
	"fmt"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

type ResourceDeleteProtectionType string

const (
	ResourceDeleteProtectionTypeCubebox ResourceDeleteProtectionType = "cubebox"

	ResourceDeleteProtectionTypeStorage ResourceDeleteProtectionType = "storage"

	ResourceDeleteProtectionTypeStorageBaseBlock ResourceDeleteProtectionType = "storage-base-block"

	ResourceTypeVmSnapshot ResourceDeleteProtectionType = "vm-snapshot"

	ResourceDeleteProtectionTypeImage ResourceDeleteProtectionType = "image"

	ResourceCubeRunTemplate ResourceDeleteProtectionType = "cube-run-template"
)

var (
	rdpm = &resourceDeleteProtectionManager{
		cdps: make(map[ResourceDeleteProtectionType][]DeleteProtectionHook),
	}
)

func RegisterDeleteProtectionHook(t ResourceDeleteProtectionType, hook DeleteProtectionHook) {
	rdpm.add(t, hook)
}

func PreDelete(ctx context.Context, opt *DeleteOption, opts ...interface{}) error {
	if opt.SkipDeleteFlagCheck {
		log.G(ctx).Warnf("skip delete flag check for resource %v", opt.ResourceType)
		return nil
	}
	hooks := rdpm.listByType(opt.ResourceType)
	for _, hook := range hooks {
		if err := hook.PreDelete(ctx, opt, opts...); err != nil {
			return fmt.Errorf(" %s do not be allowed to delete by pre %s: %v :%w", opt.ID, hook.Name(), err, errdefs.ErrAborted)
		}
	}
	return nil
}

func PostDelete(ctx context.Context, opt *DeleteOption, opts ...interface{}) error {
	if opt.SkipDeleteFlagCheck {
		log.G(ctx).Warnf("skip delete flag check for resource %v", opt.ResourceType)
		return nil
	}
	hooks := rdpm.listByType(opt.ResourceType)
	for _, hook := range hooks {
		if err := hook.PostDelete(ctx, opt, opts...); err != nil {
			return fmt.Errorf("%s do not be allowed to delete by post %s: %v: %w", opt.ID, hook.Name(), err, errdefs.ErrAborted)
		}
	}
	return nil
}

type resourceDeleteProtectionManager struct {
	cdps map[ResourceDeleteProtectionType][]DeleteProtectionHook
	lock sync.RWMutex
}

func (r *resourceDeleteProtectionManager) add(t ResourceDeleteProtectionType, hook DeleteProtectionHook) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if _, ok := r.cdps[t]; !ok {
		r.cdps[t] = make([]DeleteProtectionHook, 0)
	}

	name := hook.Name()
	for _, h := range r.cdps[t] {
		if h.Name() == name {
			return
		}
	}
	r.cdps[t] = append(r.cdps[t], hook)
}

func (r *resourceDeleteProtectionManager) listByType(t ResourceDeleteProtectionType) []DeleteProtectionHook {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.cdps[t]
}
