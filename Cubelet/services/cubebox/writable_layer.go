// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// effectiveSandboxWritableLayer is the upper a snapshot package records. Empty means no writable root.
func effectiveSandboxWritableLayer(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	return cb.EffectiveWritableLayer(context.Background())
}
