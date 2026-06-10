package cubevs

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
)

// reapDNSState scans DNS-related maps and removes expired entries.
func reapDNSState() {
	// eBPF stores expiration timestamps against CLOCK_MONOTONIC nanoseconds.
	now, err := currentNS()
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to get current time",
		})
		return
	}

	reapDNSLearnedPolicies(now)
	reapDNSQueryTrack(now)
}

// reapDNSLearnedPolicies scans allow_out_v2 and removes expired DNS-learned entries.
func reapDNSLearnedPolicies(now uint64) {
	allowOut, err := loadPinnedMap(MapNameAllowOutV2)
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to load allow_out_v2 map",
		})
		return
	}
	defer allowOut.Close()

	var (
		ifindex    uint32
		innerMapID uint32
	)
	iter := allowOut.Iterate()
	for iter.Next(&ifindex, &innerMapID) {
		if err := reapDNSLearnedPoliciesForInnerMap(innerMapID, now); err != nil {
			enqueueEvent(Event{
				Error:   err,
				Message: fmt.Sprintf("failed to reap DNS-learned policies, ifindex: %d", ifindex),
			})
		}
	}
	if err := iter.Err(); err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to iterate allow_out_v2 map",
		})
		return
	}
}

// reapDNSLearnedPoliciesForInnerMap deletes expired DNS-learned entries from one allow_out_v2 inner map.
func reapDNSLearnedPoliciesForInnerMap(innerMapID uint32, now uint64) error {
	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	var (
		key   lpmKey
		value netPolicyValueV2
	)
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		if !netPolicyValueV2Expired(value, now) {
			continue
		}
		if err := inner.Delete(&key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("failed to delete expired DNS-learned policy: %w", err)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to iterate allow_out_v2 inner map: %w", err)
	}
	return nil
}

// reapDNSQueryTrack deletes expired pending DNS queries that never got a response.
func reapDNSQueryTrack(now uint64) {
	queryTrack, err := loadPinnedMap(MapNameDNSQueryTrack)
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to load dns_query_track map",
		})
		return
	}
	defer queryTrack.Close()

	var (
		key   dnsQueryTrackKey
		value dnsQueryTrackValue
	)
	iter := queryTrack.Iterate()
	for iter.Next(&key, &value) {
		if value.ExpiresAtNS > now {
			continue
		}
		if err := queryTrack.Delete(&key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			enqueueEvent(Event{
				Error:   err,
				Message: "failed to delete expired dns_query_track entry",
			})
		}
	}
	if err := iter.Err(); err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to iterate dns_query_track map",
		})
	}
}
