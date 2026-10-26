// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !embed_envd

package envd

func DefaultBinary() []byte {
	return nil
}

func HasDefaultBinary() bool {
	return false
}
