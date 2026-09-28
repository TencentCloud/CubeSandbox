// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAppSnapshotContextUsesConfiguredTimeout(t *testing.T) {
	const timeoutInSec = 2
	start := time.Now()
	ctx, cancel := appSnapshotContext(context.Background(), timeoutInSec)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("AppSnapshot context has no deadline")
	}
	remaining := deadline.Sub(start)
	if remaining < time.Duration(timeoutInSec)*time.Second || remaining > time.Duration(timeoutInSec)*time.Second+time.Second {
		t.Fatalf("AppSnapshot deadline offset = %v, want about %ds", remaining, timeoutInSec)
	}
}

func TestAppSnapshotContextPreservesEarlierParentDeadline(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := appSnapshotContext(parent, 300)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("AppSnapshot context error = %v, want context.Canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("AppSnapshot context did not preserve parent cancellation")
	}
}
