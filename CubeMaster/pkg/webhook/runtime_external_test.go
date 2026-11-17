// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

// An independent one-task backend persists bytes, restores them on Claim, and
// generates its own receipts. It never uses MemoryDeliveryStore.
type savedStore struct {
	mu        sync.Mutex
	task      *webhook.DeliveryTask
	snapshot  webhook.EventSnapshot
	attempts  int
	token     string
	sequence  int
	completed chan webhook.TerminalResult
	receipts  []webhook.Receipt
	closes    int
}

func newSavedStore() *savedStore { return &savedStore{completed: make(chan webhook.TerminalResult, 4)} }
func (s *savedStore) Put(ctx context.Context, task webhook.DeliveryTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.task != nil {
		return webhook.ErrDeliveryCapacity
	}
	s.snapshot = task.Event.Snapshot()
	task.Event = nil
	s.task = &task
	return nil
}
func (s *savedStore) Claim(ctx context.Context, now time.Time) (*webhook.ClaimedTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.task == nil || s.token != "" || s.task.NextAttemptAt.After(now) {
		return nil, nil
	}
	event, err := webhook.RestoreEvent(s.snapshot)
	if err != nil {
		return nil, err
	}
	s.sequence++
	s.token = fmt.Sprintf("backend-claim/%d", s.sequence)
	s.attempts++
	task := *s.task
	task.Event = event
	receipt := webhook.Receipt{Token: s.token}
	s.receipts = append(s.receipts, receipt)
	return &webhook.ClaimedTask{Task: task, Attempts: s.attempts, Receipt: receipt}, nil
}
func (s *savedStore) Complete(ctx context.Context, receipt webhook.Receipt, result webhook.TerminalResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if receipt.Token == "" || receipt.Token != s.token {
		return webhook.ErrStaleReceipt
	}
	s.task, s.token = nil, ""
	s.completed <- result
	return nil
}
func (s *savedStore) Retry(ctx context.Context, receipt webhook.Receipt, next time.Time, _ webhook.FailureSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if receipt.Token == "" || receipt.Token != s.token {
		return webhook.ErrStaleReceipt
	}
	s.task.NextAttemptAt, s.token = next, ""
	return nil
}
func (s *savedStore) Drain(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		empty := s.task == nil
		s.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *savedStore) Close(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return 0, nil // Persistent work is retained, never reported as discarded.
}

type busSend struct {
	task webhook.DeliveryTask
	id   string
}
type busSender struct {
	calls     chan busSend
	failFirst bool
	attempts  int
	closes    int
}

func (s *busSender) Send(_ context.Context, task webhook.DeliveryTask, id string) webhook.SendResult {
	s.attempts++
	s.calls <- busSend{task, id}
	return webhook.SendResult{Succeeded: !s.failFirst || s.attempts > 1, Attempted: true}
}
func (s *busSender) Close() { s.closes++ }

type mutableSubscriptions struct{ items []webhook.Subscription }

func (p *mutableSubscriptions) List() []webhook.Subscription { return p.items }
func runtimeOptions() webhook.RuntimeOptions {
	return webhook.RuntimeOptions{IngressCapacity: 2, Workers: 1, RequestTimeout: time.Second, MaxAttempts: 3, StoreOperationTimeout: time.Second}
}
func operationCount(t *testing.T, op, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "cubemaster_webhook_operations_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["operation"] == op && labels["result"] == result {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestExternalRuntimeRestoresAndDeliversWithoutHTTP(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", failFirst), func(t *testing.T) {
			s := newSavedStore()
			sender := &busSender{calls: make(chan busSend, 8), failFirst: failFirst}
			provider := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Enabled: true, Target: "test-bus/topic-a", Events: []string{webhook.TypeCreated}}}}
			before := []float64{operationCount(t, "http", "attempt"), operationCount(t, "http", "success"), operationCount(t, "http_status", "204")}
			r, err := webhook.NewRuntime(context.Background(), runtimeOptions(), provider, s, sender)
			require.NoError(t, err)
			t.Cleanup(func() { r.Shutdown(context.Background()) })
			provider.items[0].Target = "changed/topic"
			provider.items[0].Events[0] = webhook.TypeKilled
			provider.items[0].Enabled = false
			c, err := webhook.Capture(webhook.CaptureInput{ID: encodedEvent(t).ID(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "external", OccurredAt: time.Now().UTC()})
			require.NoError(t, err)
			p, err := webhook.MapToProtocol(c)
			require.NoError(t, err)
			expected, err := webhook.Encode(c, p)
			require.NoError(t, err)
			require.Equal(t, webhook.PublishAccepted, r.Publisher().TryPublish(c).Status)
			select {
			case result := <-s.completed:
				require.Equal(t, webhook.DeliverySucceeded, result.State)
				require.Zero(t, result.Summary.HTTPStatus)
			case <-time.After(5 * time.Second):
				t.Fatal("external delivery never completed")
			}
			// Leave the worker live past the next retry interval: success must
			// resolve the claim instead of merely suppressing sends at shutdown.
			select {
			case unexpected := <-s.completed:
				t.Fatalf("duplicate terminal: %+v", unexpected)
			case <-time.After(2600 * time.Millisecond):
			}
			r.Shutdown(context.Background())
			want := 1
			if failFirst {
				want = 2
			}
			require.Len(t, sender.calls, want)
			previous := ""
			for i := 0; i < want; i++ {
				call := <-sender.calls
				require.Equal(t, "test-bus/topic-a", call.task.Target)
				require.Equal(t, expected.ID(), call.task.Event.ID())
				require.Equal(t, expected.Body(), call.task.Event.Body())
				require.NotEmpty(t, call.id)
				require.NotEqual(t, previous, call.id)
				previous = call.id
			}
			s.mu.Lock()
			receipts := append([]webhook.Receipt(nil), s.receipts...)
			closes := s.closes
			s.mu.Unlock()
			require.Len(t, receipts, want)
			if want == 2 {
				require.NotEqual(t, receipts[0], receipts[1])
			}
			require.Equal(t, 1, closes)
			require.Equal(t, 1, sender.closes)
			require.Equal(t, before, []float64{operationCount(t, "http", "attempt"), operationCount(t, "http", "success"), operationCount(t, "http_status", "204")})
		})
	}
}

func TestRuntimeConstructionFailureKeepsCallerResources(t *testing.T) {
	s := newSavedStore()
	sender := &busSender{calls: make(chan busSend, 1)}
	provider := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Target: "topic", Events: []string{webhook.TypeCreated}}}}
	options := runtimeOptions()
	options.Workers = 0
	r, err := webhook.NewRuntime(context.Background(), options, provider, s, sender)
	require.Error(t, err)
	require.Nil(t, r)
	require.Zero(t, s.closes)
	require.Zero(t, sender.closes)
	_, err = s.Close(context.Background())
	require.NoError(t, err)
	sender.Close()
	require.Equal(t, 1, s.closes)
	require.Equal(t, 1, sender.closes)
}

// Faults happen before the independent backend claims a task. The hook must
// honor ctx just like a remote backend; no Runtime internals are involved.
type claimFaultStore struct {
	*savedStore
	calls        atomic.Int32
	active       atomic.Int32
	fault        func(context.Context, int32) error
	drainEntered chan struct{}
}

func (s *claimFaultStore) Claim(ctx context.Context, now time.Time) (*webhook.ClaimedTask, error) {
	s.active.Add(1)
	defer s.active.Add(-1)
	if err := s.fault(ctx, s.calls.Add(1)); err != nil {
		return nil, err
	}
	return s.savedStore.Claim(ctx, now)
}

func (s *claimFaultStore) Drain(ctx context.Context) error {
	if s.drainEntered != nil {
		close(s.drainEntered)
	}
	return s.savedStore.Drain(ctx)
}

func TestRuntimeRecoversAfterClaimError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		deadline  bool
		failFirst bool
	}{
		{name: "deadline", deadline: true},
		{name: "ordinary", err: errors.New("backend unavailable")},
		{name: "wrapped-deadline", err: fmt.Errorf("backend: %w", context.DeadlineExceeded)},
		{name: "cancelled", err: context.Canceled},
		{name: "failed-send-then-success", err: errors.New("backend unavailable"), failFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan context.Context, 1)
			failed := make(chan error, 1)
			ready := make(chan struct{})
			var once sync.Once
			recoverStore := func() { once.Do(func() { close(ready) }) }
			type claimContext struct {
				ctx context.Context
				err error
			}
			healthy := make(chan claimContext, 1)
			s := &claimFaultStore{savedStore: newSavedStore()}
			s.fault = func(ctx context.Context, n int32) error {
				if n == 1 {
					entered <- ctx
					err := tc.err
					if tc.deadline {
						<-ctx.Done()
						err = ctx.Err()
					}
					failed <- err
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ready:
				}
				select {
				case healthy <- claimContext{ctx, ctx.Err()}:
				default:
				}
				return nil
			}
			sender := &busSender{calls: make(chan busSend, 8), failFirst: tc.failFirst}
			p := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Enabled: true, Target: "test-bus/topic-a", Events: []string{webhook.TypeCreated}}}}
			before := map[[2]string]float64{}
			for _, labels := range [][2]string{{"task", "claimed"}, {"send", "other"}, {"retry", "ok"}, {"complete", "ok"}, {"terminal", "succeeded"}, {"terminal", "failed"}, {"terminal", "cancelled"}} {
				before[labels] = operationCount(t, labels[0], labels[1])
			}
			options := runtimeOptions()
			options.StoreOperationTimeout = 150 * time.Millisecond
			r, err := webhook.NewRuntime(context.Background(), options, p, s, sender)
			require.NoError(t, err)
			t.Cleanup(func() {
				recoverStore()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r.Shutdown(ctx)
			})
			var first context.Context
			select {
			case first = <-entered:
				_, bounded := first.Deadline()
				require.True(t, bounded)
			case <-time.After(3 * time.Second):
				t.Fatal("first Claim never entered")
			}
			select {
			case err := <-failed:
				require.Error(t, err)
				if tc.deadline {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Claim did not return its error")
			}
			require.Empty(t, sender.calls)
			s.mu.Lock()
			attempts := s.attempts
			s.mu.Unlock()
			require.Zero(t, attempts, "failed Claim must not consume a task attempt")
			for labels, value := range before {
				require.Equal(t, value, operationCount(t, labels[0], labels[1]), "%v changed before any successful Claim", labels)
			}
			publishExternal(t, r)
			recoverStore()
			select {
			case claim := <-healthy:
				require.NotSame(t, first, claim.ctx)
				require.NoError(t, claim.err, "recovered Claim must get a live context")
			case <-time.After(3 * time.Second):
				t.Fatal("worker never claimed again after backend recovery")
			}
			select {
			case result := <-s.completed:
				require.Equal(t, webhook.DeliverySucceeded, result.State)
			case <-time.After(5 * time.Second):
				t.Fatal("recovered backend never completed delivery")
			}
			want := 1
			if tc.failFirst {
				want = 2
			}
			var firstSend busSend
			for i := 0; i < want; i++ {
				select {
				case call := <-sender.calls:
					require.Equal(t, "test-bus/topic-a", call.task.Target)
					require.NotEmpty(t, call.id)
					if i == 0 {
						firstSend = call
					} else {
						require.Equal(t, firstSend.task.Event.ID(), call.task.Event.ID())
						require.Equal(t, firstSend.task.Event.Body(), call.task.Event.Body())
						require.NotEqual(t, firstSend.id, call.id)
					}
				case <-time.After(time.Second):
					t.Fatal("missing send after successful completion")
				}
			}
			if tc.failFirst {
				select {
				case <-sender.calls:
					t.Fatal("successful delivery retried a third time")
				case <-time.After(2600 * time.Millisecond):
				}
			}
			r.Shutdown(context.Background())
			require.Empty(t, sender.calls)
			require.Equal(t, want, s.attempts)
			require.Len(t, s.receipts, want)
			require.Equal(t, 1, s.closes)
			require.Equal(t, 1, sender.closes)
			for labels, value := range before {
				increment := float64(0)
				switch labels {
				case [2]string{"task", "claimed"}, [2]string{"send", "other"}:
					increment = float64(want)
				case [2]string{"retry", "ok"}:
					increment = float64(want - 1)
				case [2]string{"complete", "ok"}, [2]string{"terminal", "succeeded"}:
					increment = 1
				}
				require.Equal(t, value+increment, operationCount(t, labels[0], labels[1]), "%v", labels)
			}
		})
	}
}

func TestRuntimeRecoversAfterEveryWorkerClaimError(t *testing.T) {
	const workers = 3
	entered := make(chan struct{}, workers)
	returned := make(chan struct{}, workers)
	resumed := make(chan error, workers)
	fail := make(chan struct{})
	healthy := make(chan struct{})
	var failOnce, healthyOnce sync.Once
	releaseFailures := func() { failOnce.Do(func() { close(fail) }) }
	recoverStore := func() { healthyOnce.Do(func() { close(healthy) }) }
	s := &claimFaultStore{savedStore: newSavedStore(), drainEntered: make(chan struct{})}
	s.fault = func(ctx context.Context, n int32) error {
		if n <= workers {
			entered <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-fail:
			}
			returned <- struct{}{}
			return errors.New("backend unavailable")
		}
		if n <= 2*workers {
			resumed <- ctx.Err()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-healthy:
			}
		}
		return nil
	}
	sender := &busSender{calls: make(chan busSend, 8)}
	p := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Enabled: true, Target: "topic", Events: []string{webhook.TypeCreated}}}}
	options := runtimeOptions()
	options.Workers = workers
	options.StoreOperationTimeout = 10 * time.Second
	r, err := webhook.NewRuntime(context.Background(), options, p, s, sender)
	require.NoError(t, err)
	t.Cleanup(func() {
		releaseFailures()
		recoverStore()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.Shutdown(ctx)
	})
	for i := 0; i < workers; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("not every worker entered its first Claim")
		}
	}
	require.Equal(t, int32(workers), s.calls.Load())
	releaseFailures()
	for i := 0; i < workers; i++ {
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			t.Fatal("Claim did not return its controlled failure")
		}
		select {
		case err := <-resumed:
			require.NoError(t, err, "each worker must resume with a live context")
		case <-time.After(3 * time.Second):
			t.Fatal("workers disappeared after Claim errors")
		}
	}
	publishExternal(t, r)
	done := make(chan struct{})
	go func() { r.Shutdown(context.Background()); close(done) }()
	select {
	case <-s.drainEntered:
	case <-time.After(time.Second):
		t.Fatal("Shutdown never entered Drain")
	}
	// Normal drain must leave recovered workers running until delivery completes.
	recoverStore()
	select {
	case result := <-s.completed:
		require.Equal(t, webhook.DeliverySucceeded, result.State)
	case <-time.After(time.Second):
		t.Fatal("draining runtime did not finish the recovered delivery")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not join recovered workers")
	}
	require.Len(t, sender.calls, 1)
	require.Equal(t, 1, s.attempts)
	require.Zero(t, s.active.Load())
	require.Equal(t, 1, s.closes)
	require.Equal(t, 1, sender.closes)
}

func TestRuntimeClaimErrorsRespectPollingAndShutdown(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			entered := make(chan struct{}, 2)
			busy := make(chan struct{}, 1)
			s := &claimFaultStore{savedStore: newSavedStore(), drainEntered: make(chan struct{})}
			s.fault = func(_ context.Context, n int32) error {
				if n <= 2 {
					entered <- struct{}{}
				}
				if n == 64 {
					busy <- struct{}{}
				}
				return errors.New("backend unavailable")
			}
			sender := &busSender{calls: make(chan busSend, 8)}
			p := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Enabled: true, Target: "topic", Events: []string{webhook.TypeCreated}}}}
			terminal := operationCount(t, "terminal", "cancelled")
			r, err := webhook.NewRuntime(context.Background(), runtimeOptions(), p, s, sender)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r.Shutdown(ctx)
			})
			for i := 0; i < 2; i++ {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("worker did not continue polling the failing backend")
				}
			}
			// A loose ceiling detects a busy loop without asserting timer precision.
			observe, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer stop()
			select {
			case <-busy:
				t.Fatal("Claim errors caused a busy loop")
			case <-observe.Done():
			}
			require.Less(t, s.calls.Load(), int32(64))
			if pending {
				publishExternal(t, r)
			}
			caller, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { r.Shutdown(caller); close(done) }()
			select {
			case <-s.drainEntered:
			case <-time.After(time.Second):
				t.Fatal("Shutdown never entered Drain")
			}
			if pending {
				cancel() // Earlier caller cancellation interrupts a pending drain.
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Shutdown did not interrupt Claim error polling")
			}
			calls := s.calls.Load()
			require.Zero(t, s.active.Load())
			require.Empty(t, sender.calls)
			require.Zero(t, s.attempts)
			require.Equal(t, 1, s.closes)
			require.Equal(t, 1, sender.closes)
			require.Equal(t, terminal, operationCount(t, "terminal", "cancelled"))
			quiet, stopQuiet := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer stopQuiet()
			<-quiet.Done()
			require.Equal(t, calls, s.calls.Load(), "Claim continued after Shutdown joined workers")
		})
	}
}

// Each blocked backend method cooperates with its own context. No goroutine
// wrapper is used to hide a call still running after Shutdown returns.
type waitingStore struct {
	*savedStore
	operation string
	entered   chan context.Context
	exited    chan error
}

func (s *waitingStore) wait(ctx context.Context) error {
	s.entered <- ctx
	<-ctx.Done()
	err := ctx.Err()
	s.exited <- err
	return err
}
func (s *waitingStore) Put(ctx context.Context, task webhook.DeliveryTask) error {
	if s.operation == "put" {
		return s.wait(ctx)
	}
	return s.savedStore.Put(ctx, task)
}
func (s *waitingStore) Claim(ctx context.Context, now time.Time) (*webhook.ClaimedTask, error) {
	if s.operation == "claim" {
		return nil, s.wait(ctx)
	}
	return s.savedStore.Claim(ctx, now)
}
func (s *waitingStore) Retry(ctx context.Context, receipt webhook.Receipt, next time.Time, summary webhook.FailureSummary) error {
	if s.operation == "retry" {
		return s.wait(ctx)
	}
	return s.savedStore.Retry(ctx, receipt, next, summary)
}
func (s *waitingStore) Complete(ctx context.Context, receipt webhook.Receipt, result webhook.TerminalResult) error {
	if s.operation == "complete" {
		return s.wait(ctx)
	}
	return s.savedStore.Complete(ctx, receipt, result)
}

func publishExternal(t *testing.T, r *webhook.Runtime) {
	t.Helper()
	c, err := webhook.Capture(webhook.CaptureInput{ID: encodedEvent(t).ID(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "external", OccurredAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, webhook.PublishAccepted, r.Publisher().TryPublish(c).Status)
}

func TestRuntimeStoreOperationsAreBounded(t *testing.T) {
	for _, operation := range []string{"put", "claim", "retry", "complete"} {
		for _, shutdown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shutdown=%t", operation, shutdown), func(t *testing.T) {
				s := &waitingStore{savedStore: newSavedStore(), operation: operation, entered: make(chan context.Context, 8), exited: make(chan error, 8)}
				sender := &busSender{calls: make(chan busSend, 8), failFirst: operation == "retry"}
				p := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Enabled: true, Target: "topic", Events: []string{webhook.TypeCreated}}}}
				options := runtimeOptions()
				options.StoreOperationTimeout = 60 * time.Millisecond
				if shutdown {
					options.StoreOperationTimeout = 10 * time.Second
				}
				terminal := operationCount(t, "terminal", "succeeded")
				r, err := webhook.NewRuntime(context.Background(), options, p, s, sender)
				require.NoError(t, err)
				t.Cleanup(func() { ctx, cancel := context.WithCancel(context.Background()); cancel(); r.Shutdown(ctx) })
				publishExternal(t, r)
				select {
				case ctx := <-s.entered:
					_, bounded := ctx.Deadline()
					require.True(t, bounded)
				case <-time.After(2 * time.Second):
					t.Fatal("backend operation never entered")
				}
				if shutdown {
					ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
					defer cancel()
					start := time.Now()
					r.Shutdown(ctx)
					require.Less(t, time.Since(start), time.Second)
				}
				select {
				case err := <-s.exited:
					require.Error(t, err)
				case <-time.After(time.Second):
					t.Fatal("backend operation did not exit")
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r.Shutdown(ctx)
				require.Equal(t, terminal, operationCount(t, "terminal", "succeeded"), "failed Complete cannot create a terminal result")
				require.Equal(t, 1, s.closes)
				require.Equal(t, 1, sender.closes)
			})
		}
	}
}

type drainingStore struct {
	*savedStore
	entered      chan struct{}
	err          error
	waitClose    bool
	closeContext context.Context
}

func (s *drainingStore) Drain(ctx context.Context) error {
	close(s.entered)
	if s.err != nil {
		return s.err
	}
	return s.savedStore.Drain(ctx)
}
func (s *drainingStore) Close(ctx context.Context) (int, error) {
	s.closeContext = ctx
	if s.waitClose {
		<-ctx.Done()
	}
	return s.savedStore.Close(ctx)
}

type waitingSender struct {
	entered chan struct{}
	exited  chan struct{}
	closes  int
}

func (s *waitingSender) Send(ctx context.Context, _ webhook.DeliveryTask, _ string) webhook.SendResult {
	close(s.entered)
	<-ctx.Done()
	close(s.exited)
	return webhook.SendResult{Attempted: true, Summary: webhook.FailureSummary{Code: "cancelled"}}
}
func (s *waitingSender) Close() { s.closes++ }

func TestRuntimeShutdownAcknowledgementAndRemainingBudget(t *testing.T) {
	for _, mode := range []string{"acknowledged", "budget-exhausted", "concurrent-earlier-caller", "close-remaining-budget"} {
		t.Run(mode, func(t *testing.T) {
			s := &drainingStore{savedStore: newSavedStore(), entered: make(chan struct{})}
			if mode == "acknowledged" || mode == "close-remaining-budget" {
				s.err = errors.New("backend drain unavailable")
			}
			s.waitClose = mode == "close-remaining-budget"
			sender := &waitingSender{entered: make(chan struct{}), exited: make(chan struct{})}
			p := &mutableSubscriptions{items: []webhook.Subscription{{ID: "bus", Enabled: true, Target: "topic", Events: []string{webhook.TypeCreated}}}}
			options := runtimeOptions()
			options.RequestTimeout = 30 * time.Second
			terminal := operationCount(t, "terminal", "cancelled")
			discarded := operationCount(t, "task", "shutdown_discarded")
			r, err := webhook.NewRuntime(context.Background(), options, p, s, sender)
			require.NoError(t, err)
			t.Cleanup(func() { ctx, cancel := context.WithCancel(context.Background()); cancel(); r.Shutdown(ctx) })
			publishExternal(t, r)
			select {
			case <-sender.entered:
			case <-time.After(time.Second):
				t.Fatal("send never entered")
			}
			start := time.Now()
			var wg sync.WaitGroup
			ctx := context.Background()
			cancel := func() {}
			if mode == "close-remaining-budget" {
				ctx, cancel = context.WithTimeout(ctx, 120*time.Millisecond)
			}
			defer cancel()
			wg.Add(1)
			go func() { defer wg.Done(); r.Shutdown(ctx) }()
			select {
			case <-s.entered:
			case <-time.After(time.Second):
				t.Fatal("drain never entered")
			}
			if mode == "concurrent-earlier-caller" {
				short, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
				defer stop()
				wg.Add(1)
				go func() { defer wg.Done(); r.Shutdown(short) }()
			}
			wg.Wait()
			limit := 2500 * time.Millisecond
			if mode != "budget-exhausted" {
				limit = time.Second
			}
			require.Less(t, time.Since(start), limit)
			select {
			case <-sender.exited:
			default:
				t.Fatal("Shutdown returned before Send exited")
			}
			require.Equal(t, 1, s.closes)
			require.Equal(t, 1, sender.closes)
			require.Equal(t, discarded, operationCount(t, "task", "shutdown_discarded"), "retained persistent tasks are not discarded")
			if mode == "acknowledged" || mode == "close-remaining-budget" {
				select {
				case result := <-s.completed:
					require.Equal(t, webhook.DeliveryCancelled, result.State)
				default:
					t.Fatal("live cleanup budget should confirm cancellation")
				}
				require.Equal(t, terminal+1, operationCount(t, "terminal", "cancelled"))
			} else {
				require.Empty(t, s.completed, "expired budget cannot confirm remote state")
				require.Equal(t, terminal, operationCount(t, "terminal", "cancelled"))
				s.mu.Lock()
				retained := s.task != nil
				s.mu.Unlock()
				require.True(t, retained)
			}
			if mode == "close-remaining-budget" {
				require.ErrorIs(t, s.closeContext.Err(), context.DeadlineExceeded)
			}
		})
	}
}
