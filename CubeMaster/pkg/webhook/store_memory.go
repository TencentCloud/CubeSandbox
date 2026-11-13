// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type deliveryKey struct {
	event        uuid.UUID
	subscription string
}
type deliveryClaim struct{ key deliveryKey }
type memoryRecord struct {
	task     DeliveryTask
	next     time.Time
	attempts int
	claim    *deliveryClaim
}

// MemoryDeliveryStore counts pending and inflight records against one limit.
// Terminal records are removed immediately; nothing survives a process exit.
type MemoryDeliveryStore struct {
	mu       sync.Mutex
	records  map[deliveryKey]*memoryRecord
	capacity int
}

var _ DeliveryStore = (*MemoryDeliveryStore)(nil)

func NewMemoryDeliveryStore(capacity int) (*MemoryDeliveryStore, error) {
	if capacity < 1 || capacity > 1000000 {
		return nil, ErrInvalidDelivery
	}
	return &MemoryDeliveryStore{capacity: capacity, records: make(map[deliveryKey]*memoryRecord)}, nil
}
func validText(s string, max int) bool { return len(s) <= max && utf8.ValidString(s) }
func validSummary(f FailureSummary) bool {
	return validText(f.Code, 64) && validText(f.Message, MaxFailureMessageBytes) && (f.HTTPStatus == 0 || f.HTTPStatus >= 100 && f.HTTPStatus <= 599)
}
func cloneTask(t DeliveryTask) DeliveryTask {
	e := *t.Event
	e.body = t.Event.Body()
	t.Event = &e
	return t
}
func sameTask(a, b DeliveryTask) bool {
	return a.Event.id == b.Event.id && a.Event.eventType == b.Event.eventType && a.Event.occurredAt.Equal(b.Event.occurredAt) && bytes.Equal(a.Event.body, b.Event.body) && a.SubscriptionID == b.SubscriptionID && a.TargetURL == b.TargetURL && a.CreatedAt.Equal(b.CreatedAt) && a.NextAttemptAt.Equal(b.NextAttemptAt) && a.MaxAttempts == b.MaxAttempts
}
func (s *MemoryDeliveryStore) Put(ctx context.Context, t DeliveryTask) error {
	e := t.Event
	if e == nil || e.id == uuid.Nil || !validType(e.eventType) || e.occurredAt.IsZero() || len(e.body) == 0 || len(e.body) > MaxDeliveryBodyBytes || strings.TrimSpace(t.SubscriptionID) == "" || !validText(t.SubscriptionID, MaxSubscriptionIDBytes) || strings.TrimSpace(t.TargetURL) == "" || !validText(t.TargetURL, MaxDeliveryURLBytes) || t.CreatedAt.IsZero() || t.NextAttemptAt.IsZero() || t.MaxAttempts < 1 || t.MaxAttempts > maxDeliveryAttempts {
		return ErrInvalidDelivery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := deliveryKey{e.id, t.SubscriptionID}
	if old := s.records[key]; old != nil {
		if !sameTask(old.task, t) {
			return ErrDeliveryConflict
		}
		return nil
	}
	if len(s.records) >= s.capacity {
		return ErrDeliveryCapacity
	}
	s.records[key] = &memoryRecord{task: cloneTask(t), next: t.NextAttemptAt}
	return nil
}
func (s *MemoryDeliveryStore) Claim(ctx context.Context, now time.Time) (*ClaimedTask, error) {
	if now.IsZero() {
		return nil, ErrInvalidDelivery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// ponytail: scan at most capacity records; consider a heap only if measured
	// scheduling cost warrants it. Choose the oldest due task to avoid starvation.
	var chosen *memoryRecord
	var key deliveryKey
	for k, r := range s.records {
		if r.claim == nil && !r.next.After(now) && (chosen == nil || r.next.Before(chosen.next)) {
			key, chosen = k, r
		}
	}
	if chosen == nil {
		return nil, nil
	}
	chosen.attempts++
	chosen.claim = &deliveryClaim{key: key}
	task := cloneTask(chosen.task)
	task.NextAttemptAt = chosen.next
	return &ClaimedTask{Task: task, Attempts: chosen.attempts, Receipt: Receipt{claim: chosen.claim}}, nil
}
func (s *MemoryDeliveryStore) owned(r Receipt) (*memoryRecord, error) {
	if r.claim == nil {
		return nil, ErrStaleReceipt
	}
	record := s.records[r.claim.key]
	if record == nil || record.claim != r.claim {
		return nil, ErrStaleReceipt
	}
	return record, nil
}
func (s *MemoryDeliveryStore) Complete(ctx context.Context, receipt Receipt, result TerminalResult) error {
	if (result.State != DeliverySucceeded && result.State != DeliveryFailed && result.State != DeliveryCancelled) || !validSummary(result.Summary) {
		return ErrInvalidDelivery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.owned(receipt); err != nil {
		return err
	}
	delete(s.records, receipt.claim.key)
	return nil
}
func (s *MemoryDeliveryStore) Retry(ctx context.Context, receipt Receipt, next time.Time, summary FailureSummary) error {
	if next.IsZero() || !validSummary(summary) {
		return ErrInvalidDelivery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := s.owned(receipt)
	if err != nil {
		return err
	}
	if r.attempts >= r.task.MaxAttempts {
		delete(s.records, receipt.claim.key)
	} else {
		r.next, r.claim = next, nil
	}
	return nil
}
func (s *MemoryDeliveryStore) size() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.records) }
func (s *MemoryDeliveryStore) discard() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.records)
	clear(s.records)
	return n
}
