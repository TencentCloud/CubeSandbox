// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// RecordingPublisher is a test-only lifecycle caller substitute.
type RecordingPublisher struct {
	mu     sync.Mutex
	events []*CapturedEvent
}

func (p *RecordingPublisher) TryPublish(ev *CapturedEvent) PublishResult {
	if err := ev.validate(); err != nil {
		return PublishResult{Status: PublishInvalid, Err: err}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return PublishResult{Status: PublishAccepted}
}

func (p *RecordingPublisher) snapshot() []*CapturedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*CapturedEvent(nil), p.events...)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not complete")
		var zero T
		return zero
	}
}

func TestPublisherAdmission(t *testing.T) {
	var disabled Publisher = NopPublisher{}
	require.Equal(t, PublishDisabled, disabled.TryPublish(nil).Status)
	require.Equal(t, PublishDisabled, disabled.TryPublish(captured(t, TypeCreated)).Status)
	for _, capacity := range []int{0, -1} {
		p, err := NewBufferedPublisher(capacity)
		require.Error(t, err)
		require.Nil(t, p)
	}
	p, err := NewBufferedPublisher(1)
	require.NoError(t, err)
	defer p.Stop()
	for _, ev := range []*CapturedEvent{nil, {}} {
		result := p.TryPublish(ev)
		require.Equal(t, PublishInvalid, result.Status)
		require.ErrorIs(t, result.Err, ErrInvalidEvent)
	}
	suppressed := captured(t, TypeCreated)
	suppressed.input.Origin = OriginCreateFailover
	result := p.TryPublish(suppressed)
	require.Equal(t, PublishInvalid, result.Status)
	require.ErrorIs(t, result.Err, ErrSuppressed)
	ev := captured(t, TypeCreated)
	require.Equal(t, PublishAccepted, p.TryPublish(ev).Status)
	resultCh := make(chan PublishResult, 1)
	go func() { resultCh <- p.TryPublish(ev) }()
	require.Equal(t, PublishFull, receive(t, resultCh).Status)
	require.Same(t, ev, receive(t, p.Events()))
	require.Equal(t, PublishAccepted, p.TryPublish(ev).Status)

	// A full queue with no consumer must not delay Stop.
	done := make(chan struct{})
	go func() {
		p.Stop()
		p.Stop()
		close(done)
	}()
	receive(t, done)
	require.Equal(t, PublishStopped, p.TryPublish(ev).Status)
	require.Equal(t, PublishStopped, p.TryPublish(nil).Status)
	require.Same(t, ev, receive(t, p.Events()))
	select {
	case _, ok := <-p.Events():
		t.Fatalf("unexpected receive after preserved item: channel open=%v", ok)
	default:
	}
}

func TestStopJoinsAdmittedPublication(t *testing.T) {
	p, err := NewBufferedPublisher(1)
	require.NoError(t, err)
	ev := captured(t, TypeCreated)
	admitted := make(chan struct{})
	release := make(chan struct{})
	sent := make(chan bool, 1)

	// Model the paused portion of TryPublish using its actual admission lock
	// and queue. This is deliberately test-only: production has no callbacks
	// or hooks between admission and send. The end-to-end concurrent test below
	// separately exercises TryPublish itself and accounts for every result.
	go func() {
		if !p.mu.TryRLock() {
			sent <- false
			close(admitted)
			return
		}
		defer p.mu.RUnlock()
		close(admitted)
		<-release
		select {
		case p.events <- ev:
			sent <- true
		default:
			sent <- false
		}
	}()
	receive(t, admitted)
	// Always release the test publisher, including on an assertion failure.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	stopped := make(chan struct{}, 4)
	for i := 0; i < cap(stopped); i++ {
		go func() { p.Stop(); stopped <- struct{}{} }()
	}
	// Observe the real writer-pending state. Unlike a sleep, this proves Stop
	// has reached the admission boundary while the admitted sender is paused.
	deadline := time.After(5 * time.Second)
	for p.mu.TryRLock() {
		p.mu.RUnlock()
		select {
		case <-deadline:
			t.Fatal("Stop did not close admission")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while an admitted publisher can still enqueue")
	default:
	}
	// New actual publishers cannot wait behind the held lock or enter.
	result := make(chan PublishResult, 1)
	go func() { result <- p.TryPublish(ev) }()
	require.Equal(t, PublishStopped, receive(t, result).Status)
	unblock()
	require.True(t, receive(t, sent))
	for i := 0; i < cap(stopped); i++ {
		receive(t, stopped)
	}
	require.Len(t, p.events, 1) // Send completed before every Stop returned.
	require.Same(t, ev, receive(t, p.Events()))
	require.Equal(t, PublishStopped, p.TryPublish(ev).Status)
	select {
	case <-p.Events():
		t.Fatal("late enqueue or closed channel after Stop")
	default:
	}
}

func TestConcurrentPublishAndStopAccounting(t *testing.T) {
	p, err := NewBufferedPublisher(64)
	require.NoError(t, err)
	ev := captured(t, TypeCreated)
	const publishers = 16
	var accepted atomic.Int64
	var invalid atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	// Ensure there is accepted work even if all stoppers win scheduling.
	require.Equal(t, PublishAccepted, p.TryPublish(ev).Status)
	accepted.Add(1)
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				switch p.TryPublish(ev).Status {
				case PublishAccepted:
					accepted.Add(1)
				case PublishFull, PublishStopped:
				default:
					invalid.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; p.Stop() }()
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	receive(t, done)
	require.Zero(t, invalid.Load())
	require.Equal(t, accepted.Load(), int64(len(p.events)))
	for i := int64(0); i < accepted.Load(); i++ {
		require.Same(t, ev, receive(t, p.Events()))
	}
	require.Equal(t, PublishStopped, p.TryPublish(ev).Status)
	require.Empty(t, p.events)
}

func TestConcurrentProductionConsumer(t *testing.T) {
	p, err := NewBufferedPublisher(8)
	require.NoError(t, err)
	ev := captured(t, TypeCreated)
	var accepted atomic.Int64
	stopReading := make(chan struct{})
	count := make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-p.Events():
				n++
			case <-stopReading:
				count <- n
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if p.TryPublish(ev).Status == PublishAccepted {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	p.Stop()
	close(stopReading)
	consumed := receive(t, count)
	require.Equal(t, accepted.Load(), int64(consumed+len(p.events)))
}

func TestRecordingPublisherConcurrent(t *testing.T) {
	p := &RecordingPublisher{}
	ev := captured(t, TypeCreated)
	require.Equal(t, PublishInvalid, p.TryPublish(nil).Status)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.TryPublish(ev)
			_ = p.snapshot()
		}()
	}
	wg.Wait()
	snapshot := p.snapshot()
	require.Len(t, snapshot, 16)
	snapshot[0] = nil
	require.Same(t, ev, p.snapshot()[0])
}
