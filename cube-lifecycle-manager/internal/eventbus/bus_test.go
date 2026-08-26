// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package eventbus

import (
	"sync"
	"testing"
	"time"
)

func TestBus_WaitReceivesLivePublish(t *testing.T) {
	b := New()
	ch, cancel := b.Wait("sbx")
	defer cancel()

	b.Publish("sbx")

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Wait channel did not receive live publish")
	}
}

func TestBus_DoesNotReplayPastPublish(t *testing.T) {
	b := New()
	b.Publish("sbx")

	ch, cancel := b.Wait("sbx")
	defer cancel()

	select {
	case <-ch:
		t.Fatal("past event must not be replayed")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBus_MultipleWaitersAllReceive(t *testing.T) {
	b := New()
	const waiterCount = 8

	chs := make([]<-chan struct{}, waiterCount)
	cancels := make([]func(), waiterCount)
	for i := range chs {
		chs[i], cancels[i] = b.Wait("sbx")
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	b.Publish("sbx")
	for i, ch := range chs {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("waiter %d did not receive event", i)
		}
	}
}

func TestBus_CancelPreventsFurtherDelivery(t *testing.T) {
	b := New()
	ch, cancel := b.Wait("sbx")
	cancel()
	cancel() // idempotent

	b.Publish("sbx")
	select {
	case <-ch:
		t.Fatal("post-cancel delivery")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBus_ConcurrentPublishAndCancel(t *testing.T) {
	b := New()
	const waiterCount = 100

	cancels := make([]func(), waiterCount)
	for i := range cancels {
		_, cancels[i] = b.Wait("sbx")
	}

	var wg sync.WaitGroup
	wg.Add(waiterCount + 1)
	for _, cancel := range cancels {
		cancel := cancel
		go func() {
			defer wg.Done()
			cancel()
		}()
	}
	go func() {
		defer wg.Done()
		for i := 0; i < waiterCount; i++ {
			b.Publish("sbx")
		}
	}()
	wg.Wait()
}

func TestBus_EmptySandboxIDIgnored(t *testing.T) {
	b := New()
	ch, cancel := b.Wait("")
	defer cancel()

	b.Publish("")
	select {
	case <-ch:
		t.Fatal("empty-id publish should be a no-op")
	case <-time.After(20 * time.Millisecond):
	}
}
