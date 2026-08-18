// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"testing"
	"time"
)

func TestBackoffDelay_IsPositiveAndCapped(t *testing.T) {
	for attempts := 1; attempts <= 15; attempts++ {
		delay := backoffDelay(attempts)
		if delay <= 0 {
			t.Fatalf("attempts=%d: delay %v must be positive", attempts, delay)
		}
		if delay > backoffCap+500*time.Millisecond {
			t.Fatalf("attempts=%d: delay %v exceeds cap %v", attempts, delay, backoffCap)
		}
	}
}

func TestBackoffDelay_IsDeterministicInRange(t *testing.T) {
	// base * 2^(attempts-1) with jitter in [0, min(base,500ms)); the first
	// attempt must be within (0, 1.5s].
	delay := backoffDelay(1)
	if delay <= 0 || delay > 1500*time.Millisecond {
		t.Fatalf("first retry delay = %v, want (0,1.5s]", delay)
	}
}
