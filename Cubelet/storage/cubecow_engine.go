// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"

func Engine() *cubecow.Engine {
	return localStorage.cowEngine
}
