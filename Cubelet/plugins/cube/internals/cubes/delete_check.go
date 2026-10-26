// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
	"errors"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func (l *local) registerCDPDeleteHooks() {

	user := &userDeleteCubeboxChecker{l: l}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeCubebox, user)
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeStorage, user)

	vm := &vmExistDeleteCubeboxChecker{l: l}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeCubebox, vm)
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeStorage, vm)

	template := &templateDeleteHook{l: l}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceCubeRunTemplate, template)

	baseBlock := &baseBlockDeleteHook{l: l}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeStorageBaseBlock, baseBlock)

	cubeboxImage := &cubeboxImageDeleteHook{l: l}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeImage, cubeboxImage)
}

type userDeleteCubeboxChecker struct {
	l *local
}

var _ cdp.DeleteProtectionHook = &userDeleteCubeboxChecker{}

func (u *userDeleteCubeboxChecker) Name() string {
	return "user-define-delete-cubebox-checker"
}

func (u *userDeleteCubeboxChecker) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

func (u *userDeleteCubeboxChecker) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	cb, err := u.l.Get(ctx, opt.ID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) {
			return nil
		}
		return err
	}
	if cb.UserMarkDeletedTime != nil {
		return nil
	}
	return fmt.Errorf("cubebox %s is not marked as deleted by user", opt.ID)
}

type vmExistDeleteCubeboxChecker struct {
	l *local
}

func (v *vmExistDeleteCubeboxChecker) Name() string {
	return "vm-exist-delete-cubebox-checker"
}

func (v *vmExistDeleteCubeboxChecker) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

func (v *vmExistDeleteCubeboxChecker) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	cb, _ := v.l.Get(ctx, opt.ID)
	if cb == nil {
		return nil
	}
	if cb.GetStatus() != nil {
		pid := cb.GetStatus().Get().Pid
		if pid != 0 {
			if utils.ProcessExists(ctx, int(pid)) {
				return fmt.Errorf("cubebox %s process still exists, do not delete it", cb.ID)
			}
		}
	}
	return nil
}

var _ cdp.DeleteProtectionHook = &vmExistDeleteCubeboxChecker{}

type templateDeleteHook struct {
	l *local
}

func (t *templateDeleteHook) Name() string {
	return "cubebox-template-delete-hook"
}

func (t *templateDeleteHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	cbs, err := t.l.cubeboxStore.GetCubeboxByTemplateID(opt.ID)
	if err != nil {
		return fmt.Errorf("failed to get cubebox by template id %s: %w", opt.ID, err)
	}
	if len(cbs) > 0 {
		return fmt.Errorf("cubebox exist, should not delete template %s", opt.ID)
	}
	return nil
}

func (t *templateDeleteHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

var _ cdp.DeleteProtectionHook = &templateDeleteHook{}

type baseBlockDeleteHook struct {
	l *local
}

func (b *baseBlockDeleteHook) Name() string {
	return "cubebox-base-block-delete-hook"
}

func (b *baseBlockDeleteHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

func (b *baseBlockDeleteHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	cbs, err := b.l.cubeboxStore.GetCubeboxByBaseBlockID(opt.ID)
	if err != nil {
		return fmt.Errorf("failed to get cubebox by base block id %s: %w", opt.ID, err)
	}
	if len(cbs) > 0 {
		return fmt.Errorf("%d cubebox exist, should not delete base block %s", len(cbs), opt.ID)
	}
	return nil
}

var _ cdp.DeleteProtectionHook = &baseBlockDeleteHook{}

type cubeboxImageDeleteHook struct {
	l *local
}

func (i *cubeboxImageDeleteHook) Name() string {
	return "cubebox-used-image-delete-hook"
}

func (i *cubeboxImageDeleteHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {

	return nil
}

func (i *cubeboxImageDeleteHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	cbs, err := i.l.cubeboxStore.GetCubeboxByImageID(opt.ID)
	if err != nil {
		return fmt.Errorf("failed to get cubebox by image id %s: %w", opt.ID, err)
	}
	if len(cbs) > 0 {
		return fmt.Errorf("cubebox %s is used by %d cubebox", opt.ID, len(cbs))
	}
	return nil

}
