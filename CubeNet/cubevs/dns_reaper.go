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

// reapDNSLearnedPolicies removes expired DNS-learned allow_out_v3 entries.
//
// The fast path iterates ifindex_to_mvmmeta so Ready-pool TAPs (metadata
// deleted, allow_out flushed) are skipped without walking every HashOfMaps
// outer key. If metadata cannot be loaded, fall back to iterating allow_out_v3
// outers so a transient pin/ENOENT failure cannot stall DNS TTL expiry for
// every subsequent tick.
func reapDNSLearnedPolicies(now uint64) {
	allowOut, err := loadPinnedMap(MapNameAllowOutV3)
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to load allow_out_v3 map",
		})
		return
	}
	defer allowOut.Close()

	meta, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to load ifindex_to_mvmmeta map; falling back to allow_out_v3 outer scan",
		})
		reapDNSLearnedPoliciesFromAllowOutOuter(allowOut, now)
		return
	}
	defer meta.Close()

	var (
		ifindex uint32
		mvmMeta mvmMetadata
	)
	iter := meta.Iterate()
	for iter.Next(&ifindex, &mvmMeta) {
		if err := reapDNSLearnedPoliciesForIfindex(allowOut, ifindex, now); err != nil {
			enqueueEvent(Event{
				Error:   err,
				Message: fmt.Sprintf("failed to reap DNS-learned policies, ifindex: %d", ifindex),
			})
		}
	}
	if err := iter.Err(); err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to iterate ifindex_to_mvmmeta map",
		})
	}
}

func reapDNSLearnedPoliciesFromAllowOutOuter(allowOut *ebpf.Map, now uint64) {
	var (
		ifindex uint32
		value   uint32
	)
	iter := allowOut.Iterate()
	for iter.Next(&ifindex, &value) {
		if err := reapDNSLearnedPoliciesForIfindex(allowOut, ifindex, now); err != nil {
			enqueueEvent(Event{
				Error:   err,
				Message: fmt.Sprintf("failed to reap DNS-learned policies, ifindex: %d", ifindex),
			})
		}
	}
	if err := iter.Err(); err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to iterate allow_out_v3 outer map",
		})
	}
}

func reapDNSLearnedPoliciesForIfindex(allowOut *ebpf.Map, ifindex uint32, now uint64) error {
	inner, err := acquireInnerMap(allowOut, ifindex, MapNameAllowOutV3, nil)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("failed to open allow_out_v3 inner map: %w", err)
	}
	return reapDNSLearnedPoliciesForInner(inner, now)
}

// reapDNSLearnedPoliciesForInner deletes expired DNS-learned entries from one
// allow_out_v3 inner map.
func reapDNSLearnedPoliciesForInner(inner *ebpf.Map, now uint64) error {
	var (
		key   lpmKeyV3
		value netPolicyValueV3
	)
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		if !netPolicyValueV3Expired(value, now) {
			continue
		}
		if err := inner.Delete(&key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("failed to delete expired DNS-learned policy: %w", err)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to iterate allow_out_v3 inner map: %w", err)
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
