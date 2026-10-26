// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build embed_envd

package envd

import _ "embed"

//go:embed assets/envd
var defaultBinary []byte

func DefaultBinary() []byte {
	return defaultBinary
}

func HasDefaultBinary() bool {
	return len(defaultBinary) > 0
}
