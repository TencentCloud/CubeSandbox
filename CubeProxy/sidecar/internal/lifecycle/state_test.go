// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"testing"
	"time"
)

func TestTTLForRestoredState(t *testing.T) {
	transitionTTL := 30 * time.Second
	tests := []struct {
		state string
		want  time.Duration
	}{
		{state: "running", want: TerminalStateTTL},
		{state: "paused", want: TerminalStateTTL},
		{state: "killed", want: TerminalStateTTL},
		{state: "pausing", want: transitionTTL},
		{state: "resuming", want: transitionTTL},
		{state: "killing", want: transitionTTL},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := TTLForRestoredState(tt.state, transitionTTL); got != tt.want {
				t.Fatalf("TTLForRestoredState(%q) = %s, want %s", tt.state, got, tt.want)
			}
		})
	}
}
