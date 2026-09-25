// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package eventbus

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestSubscriberRunCancellationDuringRetryBackoffReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempted := make(chan struct{}, 1)
	subscriber := &Subscriber{log: zap.NewNop()}
	done := make(chan error, 1)
	go func() {
		done <- subscriber.run(ctx, time.Hour, func(context.Context) error {
			attempted <- struct{}{}
			return errors.New("subscription unavailable")
		})
	}()

	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not attempt subscription")
	}
	// Give run time to pass its post-attempt context check and enter the
	// hour-long backoff. The old time.Sleep implementation then cannot return.
	time.Sleep(10 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancellation during retry backoff")
	}
}
