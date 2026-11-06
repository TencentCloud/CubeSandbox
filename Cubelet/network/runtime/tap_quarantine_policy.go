// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"fmt"
)

type tapQuarantinePolicyAdapter interface {
	CleanupTAPPolicy(uint32) error
	InstallTAPDefaultDenyPolicy(uint32) error
}

// resetTAPQuarantinePolicy requires old allow/DNS state to be removed before
// the link can be restored. Attempt default deny even after a partial flush,
// but propagate either failure so callers cannot publish Ready.
func resetTAPQuarantinePolicy(adapter tapQuarantinePolicyAdapter, ifindex uint32) error {
	clearErr := adapter.CleanupTAPPolicy(ifindex)
	if clearErr != nil {
		clearErr = fmt.Errorf("clear TAP %d policy: %w", ifindex, clearErr)
	}
	denyErr := adapter.InstallTAPDefaultDenyPolicy(ifindex)
	if denyErr != nil {
		denyErr = fmt.Errorf("install TAP %d default deny: %w", ifindex, denyErr)
	}
	return errors.Join(clearErr, denyErr)
}
