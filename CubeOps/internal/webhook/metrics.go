// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"sync"
	"sync/atomic"
)

// EventStats holds routing outcomes for one event name.
type EventStats struct {
	Matched  uint64
	Filtered uint64
}

// Stats records in-process webhook delivery counters. Counters intentionally
// reset when CubeOps restarts and are emitted periodically in structured logs.
type Stats struct {
	ingressAcceptedBatches atomic.Uint64
	ingressAcceptedEvents  atomic.Uint64
	ingressRejectedBatches atomic.Uint64
	invalidBatches         atomic.Uint64
	matchedEvents          atomic.Uint64
	filteredEvents         atomic.Uint64
	endpointBatches        atomic.Uint64
	retryAttempts          atomic.Uint64
	deliverySuccesses      atomic.Uint64
	deliveryFailures       atomic.Uint64
	activeDeliveries       atomic.Int64
	activeHTTPAttempts     atomic.Int64
	eventsMu               sync.Mutex
	events                 map[string]EventStats
}

// StatsSnapshot is a consistent-at-read-time view of webhook counters.
type StatsSnapshot struct {
	IngressAcceptedBatches uint64
	IngressAcceptedEvents  uint64
	RejectedBatches        uint64
	InvalidBatches         uint64
	MatchedEvents          uint64
	FilteredEvents         uint64
	EndpointBatches        uint64
	RetryAttempts          uint64
	DeliverySuccesses      uint64
	DeliveryFailures       uint64
	ActiveDeliveries       int64
	ActiveHTTPAttempts     int64
	Events                 map[string]EventStats
}

// NewStats creates an empty webhook statistics registry.
func NewStats() *Stats {
	return &Stats{events: make(map[string]EventStats)}
}

func (s *Stats) recordMatched(event string) {
	s.matchedEvents.Add(1)
	s.eventsMu.Lock()
	stats := s.events[event]
	stats.Matched++
	s.events[event] = stats
	s.eventsMu.Unlock()
}

func (s *Stats) recordFiltered(event string) {
	s.filteredEvents.Add(1)
	s.eventsMu.Lock()
	stats := s.events[event]
	stats.Filtered++
	s.events[event] = stats
	s.eventsMu.Unlock()
}

// Snapshot returns the current in-memory counters.
func (s *Stats) Snapshot() StatsSnapshot {
	s.eventsMu.Lock()
	events := make(map[string]EventStats, len(s.events))
	for event, stats := range s.events {
		events[event] = stats
	}
	s.eventsMu.Unlock()
	return StatsSnapshot{
		IngressAcceptedBatches: s.ingressAcceptedBatches.Load(),
		IngressAcceptedEvents:  s.ingressAcceptedEvents.Load(),
		RejectedBatches:        s.ingressRejectedBatches.Load(),
		InvalidBatches:         s.invalidBatches.Load(),
		MatchedEvents:          s.matchedEvents.Load(),
		FilteredEvents:         s.filteredEvents.Load(),
		EndpointBatches:        s.endpointBatches.Load(),
		RetryAttempts:          s.retryAttempts.Load(),
		DeliverySuccesses:      s.deliverySuccesses.Load(),
		DeliveryFailures:       s.deliveryFailures.Load(),
		ActiveDeliveries:       s.activeDeliveries.Load(),
		ActiveHTTPAttempts:     s.activeHTTPAttempts.Load(),
		Events:                 events,
	}
}
