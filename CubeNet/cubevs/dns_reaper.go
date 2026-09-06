package cubevs

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
)

// reapDNSState expires DNS-related state. Called from the session reaper tick.
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

	// Age the sandboxes already under management first (cheap: hasExpired
	// short-circuits), then sweep the maps for ifindices this process has not
	// touched yet — after a restart those hold orphaned learned rows nobody
	// owns until GetNetPolicyManager reloads their snapshot.
	ageAllManagers(now)
	reapDNSLearnedPolicies(now)
	reapDNSQueryTrack(now)
}

// reapDNSLearnedPolicies retires DNS-learned allow_out_v3 rows whose TTL has
// elapsed.
//
// This is index-driven: each sandbox's NetPolicyManager knows which QName
// taught which IP, so expiry is decided per (QName, IP) and the resulting map
// deletes fall out of the normal desired/diff pass. A row shared by several
// QNames therefore survives until the last of them expires — scanning the map
// for lapsed expires_at_ns instead, as this used to, would delete the row as
// soon as *one* holder lapsed and silently de-authorise the others.
//
// Enumeration still comes from the BPF maps rather than the in-memory
// registry: after a Cubelet restart a sandbox has no manager until something
// touches it, and its orphaned learned rows would otherwise never be
// reclaimed. GetNetPolicyManager loads the persisted snapshot and reconciles
// against the live maps on first use, so visiting an ifindex here is what
// brings it back under management.
//
// The fast path iterates ifindex_to_mvmmeta so Ready-pool TAPs (metadata
// deleted, allow_out flushed) are skipped without walking every HashOfMaps
// outer key. If metadata cannot be loaded, fall back to iterating allow_out_v3
// outers so a transient pin/ENOENT failure cannot stall DNS TTL expiry for
// every subsequent tick.
func reapDNSLearnedPolicies(now uint64) {
	ifindices, err := dnsReapCandidates()
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to enumerate sandboxes for DNS TTL expiry",
		})
		return
	}
	for _, ifindex := range ifindices {
		mgr, err := GetNetPolicyManager(ifindex)
		if err != nil {
			enqueueEvent(Event{
				Error:   err,
				Message: fmt.Sprintf("failed to open policy manager, ifindex: %d", ifindex),
			})
			continue
		}
		if err := mgr.pruneExpired(now); err != nil {
			enqueueEvent(Event{
				Error:   err,
				Message: fmt.Sprintf("failed to reap DNS-learned policies, ifindex: %d", ifindex),
			})
		}
	}
}

// dnsReapCandidates lists the ifindices to visit this tick, preferring the
// metadata map and falling back to the allow_out_v3 outer keys.
func dnsReapCandidates() ([]uint32, error) {
	if ifindices, err := outerMapKeys(MapNameIfindexToMVMMetadata, func() any { return new(mvmMetadata) }); err == nil {
		return ifindices, nil
	}
	return outerMapKeys(MapNameAllowOutV3, func() any { return new(uint32) })
}

// outerMapKeys collects the uint32 keys of a pinned map. newValue supplies a
// scratch value of the map's value type for the iterator.
func outerMapKeys(name string, newValue func() any) ([]uint32, error) {
	m, err := loadPinnedMap(name)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	var (
		key  uint32
		keys []uint32
	)
	value := newValue()
	iter := m.Iterate()
	for iter.Next(&key, value) {
		keys = append(keys, key)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate %s: %w", name, err)
	}
	return keys, nil
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
