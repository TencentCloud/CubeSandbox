// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import "time"

// TerminalStateTTL is the Redis TTL for terminal states (0 means no expiration).
const TerminalStateTTL time.Duration = 0

// TTLForRestoredState returns the correct Redis TTL when restoring an observed state.
// Terminal states use TerminalStateTTL; transient states use the provided transitionTTL.
func TTLForRestoredState(state string, transitionTTL time.Duration) time.Duration {
	switch state {
	case "running", "paused", "killed":
		return TerminalStateTTL
	default:
		return transitionTTL
	}
}
