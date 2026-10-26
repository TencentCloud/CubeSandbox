// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package netfile

func TrimHostName(sandboxID string) string {
	if len(sandboxID) < 8 {
		return sandboxID
	}
	return sandboxID[:8]
}
