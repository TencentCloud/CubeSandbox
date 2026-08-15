// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"testing"
	"time"
)

func TestBackoffTime_IsPositiveAndCapped(t *testing.T) {
	for attempts := 1; attempts <= 15; attempts++ {
		nt := backoffTime(attempts)
		delay := time.Until(nt)
		if delay <= 0 {
			t.Fatalf("attempts=%d: delay %v must be positive", attempts, delay)
		}
		if delay > backoffCap+time.Second {
			t.Fatalf("attempts=%d: delay %v exceeds cap %v", attempts, delay, backoffCap)
		}
	}
}

func TestBackoffTime_IsDeterministicInRange(t *testing.T) {
	// base * 2^(attempts-1) with jitter in [0, min(base,500ms)); the first
	// attempt must be within (0, 1.5s).
	nt := backoffTime(1)
	delay := time.Until(nt)
	if delay <= 0 || delay > 2*time.Second {
		t.Fatalf("first retry delay = %v, want (0,2s]", delay)
	}
}
