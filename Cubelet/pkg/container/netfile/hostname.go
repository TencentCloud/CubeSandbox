// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package netfile

// shortHostnameLen is the guest hostname length taken from the sandbox ID.
// It matches cubecli's truncated CONTAINER/CUBEBOX display (id[:12]).
const shortHostnameLen = 12

// TrimHostName returns the short guest hostname derived from sandboxID.
// Collisions across sandboxes are acceptable: each sandbox has its own UTS
// namespace, so hostnames do not need to be cluster-unique.
func TrimHostName(sandboxID string) string {
	if len(sandboxID) <= shortHostnameLen {
		return sandboxID
	}
	return sandboxID[:shortHostnameLen]
}
