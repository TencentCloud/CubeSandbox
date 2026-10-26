// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cdp

import (
	"context"
)

type DeleteOption struct {
	ID string

	ResourceType ResourceDeleteProtectionType

	ResourceOrigin any

	SkipDeleteFlagCheck bool
}

type DeleteProtectionHook interface {
	Name() string
	PreDelete(ctx context.Context, opt *DeleteOption, opts ...interface{}) error
	PostDelete(ctx context.Context, opt *DeleteOption, opts ...interface{}) error
}
