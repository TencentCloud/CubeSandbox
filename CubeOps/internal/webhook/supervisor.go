// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
)

const (
	backoffBase      = time.Second
	backoffCap       = 10 * time.Minute
	inflightQuitWait = 3 * time.Second
)

// errGracefulShutdown is the cancellation cause used to interrupt in-flight
// sends only after the shutdown grace window elapsed; the sender classifies
// it as ResultShutdown (no attempt recorded).
var errGracefulShutdown = errors.New("graceful shutdown")

// DeliverySender performs one HTTP delivery. *Sender implements it; tests
// substitute fakes without binding a listener.
type DeliverySender interface {
	Send(ctx context.Context, d *DeliveryForSend) SendResult
}

// Supervisor claims delivery rows and sends them through the worker pool.
type Supervisor struct {
	store   *DeliveryStore
	sender  DeliverySender
	backlog *BacklogCache

	owner             string
	lease             time.Duration
	window            time.Duration
	poll              time.Duration
	claimBatch        int
	workerConcurrency int
	perSubConcurrency int
	softLimit         int
	maxAttempts       int
	deadLetterMode    string
	shutdownTimeout   time.Duration

	workers  chan struct{}
	perSub   sync.Map // int64 → chan struct{}
	inflight sync.Map // int64 → string(owner)

	baseCtx    context.Context
	cancelSend context.CancelCauseFunc
	claimStop  context.CancelFunc

	started atomic.Bool
	healthy atomic.Bool
	closing atomic.Bool
}

// NewSupervisor builds the claim/send loop. owner must be the process-unique
// consumer name so lease ownership is unambiguous across replicas.
func NewSupervisor(
	store *DeliveryStore,
	sender DeliverySender,
	backlog *BacklogCache,
	owner string,
	lease, window, poll, shutdownTimeout time.Duration,
	claimBatch, workerConcurrency, perSubConcurrency, softLimit, maxAttempts int,
	deadLetterMode string,
) *Supervisor {
	return &Supervisor{
		store: store, sender: sender, backlog: backlog,
		owner: owner, lease: lease, window: window, poll: poll,
		shutdownTimeout:   shutdownTimeout,
		claimBatch:        claimBatch,
		workerConcurrency: workerConcurrency,
		perSubConcurrency: perSubConcurrency,
		softLimit:         softLimit,
		maxAttempts:       maxAttempts,
		deadLetterMode:    deadLetterMode,
	}
}

// Start launches the claim loop. The passed ctx cancels everything except the
// graceful-shutdown grace window (see Shutdown).
func (s *Supervisor) Start(ctx context.Context) {
	s.baseCtx, s.cancelSend = context.WithCancelCause(ctx)
	claimCtx, claimStop := context.WithCancel(ctx)
	s.claimStop = claimStop
	s.workers = make(chan struct{}, s.workerConcurrency)
	go s.claimLoop(claimCtx)
	s.started.Store(true)
	s.healthy.Store(true)
}

// claimLoop pages through the two candidate queries (retry-due + expired
// lease), applying per-subscription admission before the atomic claim, and
// dispatching claimed rows to the worker pool. When a full page is entirely
// skipped (soft-limit admission), it continues with a keyset cursor instead
// of falling back to the slow poll, so one slow subscription cannot starve
// the head of the queue.
func (s *Supervisor) claimLoop(ctx context.Context) {
	var retryAt time.Time
	var retryID int64
	var leaseAt time.Time
	var leaseID int64

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		overLimit := s.backlog.OverLimit(s.softLimit)

		due, err := s.store.ClaimCandidatesDue(ctx, ClaimQuery{
			Limit: s.claimBatch, ExcludeSubscriptions: overLimit,
			KeepPendingWindow: s.window,
			AfterRetryAt:      retryAt, AfterRetryID: retryID,
		})
		if err != nil {
			logging.G(ctx).Warnf("webhook supervisor: due candidates: %v", err)
			s.sleep(ctx)
			retryAt, retryID, leaseAt, leaseID = time.Time{}, 0, time.Time{}, 0
			continue
		}
		if len(due) == 0 {
			retryAt, retryID = time.Time{}, 0
		} else if !s.processCandidates(ctx, due) && len(due) >= s.claimBatch {
			if at, _, cerr := s.store.CursorFor(ctx, due[len(due)-1]); cerr == nil {
				retryAt, retryID = at, due[len(due)-1]
				continue
			}
		} else {
			retryAt, retryID = time.Time{}, 0
		}

		expired, err := s.store.ClaimCandidatesLease(ctx, ClaimQuery{
			Limit: s.claimBatch, ExcludeSubscriptions: overLimit,
			AfterLeaseUntil: leaseAt, AfterLeaseID: leaseID,
		})
		if err != nil {
			logging.G(ctx).Warnf("webhook supervisor: lease candidates: %v", err)
			s.sleep(ctx)
			leaseAt, leaseID = time.Time{}, 0
			continue
		}
		if len(expired) == 0 {
			leaseAt, leaseID = time.Time{}, 0
		} else if !s.processCandidates(ctx, expired) && len(expired) >= s.claimBatch {
			if _, lu, cerr := s.store.CursorFor(ctx, expired[len(expired)-1]); cerr == nil && lu != nil {
				leaseAt, leaseID = *lu, expired[len(expired)-1]
				continue
			}
			leaseAt, leaseID = time.Time{}, 0
		} else {
			leaseAt, leaseID = time.Time{}, 0
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll):
		}
	}
}

// processCandidates runs admission + claim for a batch of ids. Returns true
// when at least one row was claimed (caller then starts a fresh round).
func (s *Supervisor) processCandidates(ctx context.Context, ids []int64) bool {
	claimed := false
	for _, id := range ids {
		subID, err := s.store.SubscriptionForDelivery(ctx, id)
		if err != nil {
			logging.G(ctx).Warnf("webhook supervisor: subscription for %d: %v", id, err)
			continue
		}
		if !s.acquirePerSub(subID) {
			continue // soft limit / per-sub concurrency full → skip this row
		}
		ok, err := s.store.Claim(ctx, id, s.owner, s.lease, s.window)
		if err != nil {
			s.releasePerSub(subID)
			logging.G(ctx).Warnf("webhook supervisor: claim %d: %v", id, err)
			continue
		}
		if !ok {
			s.releasePerSub(subID)
			continue
		}
		claimed = true
		select {
		case s.workers <- struct{}{}:
			s.inflight.Store(id, s.owner)
			go s.sendOne(ctx, id, subID)
		default:
			// Pool saturated: hand the lease back rather than hold it queued.
			if err := s.store.ReleaseLease(ctx, id, s.owner); err != nil {
				logging.G(ctx).Warnf("webhook supervisor: release on saturation: %v", err)
			}
			s.releasePerSub(subID)
		}
	}
	return claimed
}

// sendOne loads, sends and completes one delivery.
func (s *Supervisor) sendOne(ctx context.Context, id, subID int64) {
	defer func() {
		<-s.workers
		s.releasePerSub(subID)
		s.inflight.Delete(id)
		if r := recover(); r != nil {
			logging.G(ctx).Errorf("webhook supervisor: send panic recovered: %v", r)
		}
	}()

	d, err := s.store.LoadDeliveryForSend(ctx, id)
	if err != nil {
		// Secret decryption failure is permanent; any other load failure
		// returns the lease so another worker can retry.
		if errors.Is(err, ErrSecretDecrypt) {
			msg := err.Error()
			_, _ = s.store.Complete(ctx, id, s.owner, Completion{
				Result: ResultPermanent, LastError: &msg,
			})
			return
		}
		_ = s.store.ReleaseLease(ctx, id, s.owner)
		return
	}

	sendCtx, cancel := context.WithCancelCause(s.baseCtx)
	res := s.sender.Send(sendCtx, d)
	cancel(nil)

	switch res.Class {
	case ResultSucceeded:
		status := res.HTTPStatus
		_, _ = s.store.Complete(ctx, id, s.owner, Completion{Result: ResultSucceeded, HTTPStatus: &status})
	case ResultPermanent:
		status := res.HTTPStatus
		msg := errText(res.Err)
		_, _ = s.store.Complete(ctx, id, s.owner, Completion{
			Result: ResultPermanent, HTTPStatus: &status, LastError: &msg,
		})
	case ResultRetryable:
		status := res.HTTPStatus
		msg := errText(res.Err)
		nextAttempts := d.Attempts + 1
		if s.deadLetterMode == "dead-letter" && nextAttempts >= s.maxAttempts {
			_, _ = s.store.Complete(ctx, id, s.owner, Completion{
				Result: ResultDead, HTTPStatus: &status, LastError: &msg,
			})
			return
		}
		_, _ = s.store.Complete(ctx, id, s.owner, Completion{
			Result: ResultRetryable, HTTPStatus: &status, LastError: &msg,
			NextRetryAt: backoffTime(nextAttempts),
		})
	case ResultShutdown:
		// Interrupted by graceful shutdown: no attempt recorded, lease stays
		// held; Shutdown releases it in step ③.
	}
}

// Shutdown performs the three-step graceful stop: ① stop claiming; ② wait up
// to shutdownTimeout for in-flight sends to finish WITHOUT cancel (so normal
// completions are not mis-counted as failures); ③ cancel stragglers with the
// shutdown sentinel and return any still-owned leases.
func (s *Supervisor) Shutdown(ctx context.Context) {
	if !s.started.Load() || s.closing.Swap(true) {
		return
	}
	s.claimStop() // ①
	s.healthy.Store(false)

	// ② grace window: no cancel.
	deadline := time.Now().Add(s.shutdownTimeout)
	for s.inflightCount() > 0 {
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s.inflightCount() == 0 {
		return
	}

	// ③ cancel stragglers (sentinel cause → sender reports shutdown, no
	// attempt), give them a moment to unwind, then release remaining leases.
	s.cancelSend(errGracefulShutdown)
	waitUntil := time.Now().Add(inflightQuitWait)
	for s.inflightCount() > 0 && time.Now().Before(waitUntil) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.inflight.Range(func(k, v interface{}) bool {
		id := k.(int64)
		owner := v.(string)
		if err := s.store.ReleaseLease(ctx, id, owner); err != nil {
			logging.G(ctx).Warnf("webhook supervisor: release lease %d: %v", id, err)
		}
		return true
	})
}

func (s *Supervisor) acquirePerSub(subID int64) bool {
	raw, _ := s.perSub.LoadOrStore(subID, make(chan struct{}, s.perSubConcurrency))
	ch := raw.(chan struct{})
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Supervisor) releasePerSub(subID int64) {
	if raw, ok := s.perSub.Load(subID); ok {
		select {
		case <-raw.(chan struct{}):
		default:
		}
	}
}

func (s *Supervisor) inflightCount() int {
	n := 0
	s.inflight.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

func (s *Supervisor) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(s.poll):
	}
}

// Started reports whether the supervisor loop has been launched.
func (s *Supervisor) Started() bool { return s.started.Load() }

// Healthy reports whether the loop is running and not shutting down.
func (s *Supervisor) Healthy() bool { return s.healthy.Load() }

// backoffTime computes next_retry_at using the capped exponential formula
// base * 2^(attempts-1) + jitter, capped at backoffCap.
func backoffTime(attempts int) time.Time {
	delay := backoffBase
	if attempts > 1 {
		for i := 1; i < attempts && delay < backoffCap; i++ {
			delay *= 2
		}
	}
	if delay > backoffCap {
		delay = backoffCap
	}
	jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
	if jitter > delay/2 {
		jitter = delay / 2
	}
	return time.Now().Add(delay + jitter)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
