// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
)

type imageDeleteHook struct {
	*localCubeRunTemplateManager
}

func (i *imageDeleteHook) Name() string {
	return "distribution-image-delete-hook"
}

func (i *imageDeleteHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {

	return nil
}

func (i *imageDeleteHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return fmt.Errorf("get namespace failed: %v", err)
	}
	key := ns + "/" + opt.ID
	templates, err := i.store.ByIndexGeneric(imageNamespaceIDIndexerKey, key)
	if err != nil {
		return fmt.Errorf("get template by image id failed: %v", err)
	}

	if len(templates) > 0 {
		return fmt.Errorf("image %q is used by template %q", opt.ID, templates[0].TemplateID)
	}
	return nil

}

var _ cdp.DeleteProtectionHook = &imageDeleteHook{}

type baseBlockDeleteHook struct {
	*localCubeRunTemplateManager
}

func (b *baseBlockDeleteHook) Name() string {
	return "distribution-base-block-delete-hook"
}

func (b *baseBlockDeleteHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

func (b *baseBlockDeleteHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return fmt.Errorf("get namespace failed: %v", err)
	}
	key := ns + "/" + opt.ID
	templates, err := b.store.ByIndexGeneric(baseBlockNamespaceIDIndexerKey, key)
	if err != nil {
		return fmt.Errorf("get template by base block id failed: %v", err)
	}

	if len(templates) > 0 {
		return fmt.Errorf("base block %q is used by template %q", opt.ID, templates[0].TemplateID)
	}
	return nil
}

var _ cdp.DeleteProtectionHook = &baseBlockDeleteHook{}

type snapshotDeleteHook struct {
	*localCubeRunTemplateManager
}

func (s *snapshotDeleteHook) Name() string {
	return "distribution-snapshot-delete-hook"
}

func (s *snapshotDeleteHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

func (s *snapshotDeleteHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	templates, err := s.store.ByIndexGeneric(snapshotIDIndexerKey, opt.ID)
	if err != nil {
		return fmt.Errorf("get template by snapshot id failed: %v", err)
	}

	if len(templates) > 0 {
		return fmt.Errorf("snapshot %q is used by template %q", opt.ID, templates[0].TemplateID)
	}
	return nil
}

var _ cdp.DeleteProtectionHook = &snapshotDeleteHook{}
