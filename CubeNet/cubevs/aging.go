// Aging of DNS-learned allow_out_v3 entries.
//
// Aging reclaims rows; it does not close an access window. The datapath
// already enforces expiry on every packet (classify_egress_flow honours an
// allow row only while expires_at_ns == 0 || expires_at_ns > now), so a lapsed
// row stops admitting traffic on its own. What is left for user space is
// deleting it and keeping DynamicLearned honest — which is also why aging must
// not bump policy_version, as that would make every established flow on the
// node re-evaluate on a TTL boundary.
//
// There is no ticker here: the session reaper already runs every 5s and calls
// ageAllManagers from that tick. 5s is ample against a 300s TTL floor.

package cubevs

// ageAllManagers snapshots the current managers under the registry lock, then
// walks the snapshot outside it. Holding the registry lock across each apply
// would serialize aging with sandbox create/teardown for no reason — each
// manager has its own mutex.
func ageAllManagers(nowNs uint64) {
	for _, m := range liveManagers() {
		// A per-manager failure is reported for observability but must not
		// stop the pass: other sandboxes still need their TTLs enforced.
		if err := m.pruneExpired(nowNs); err != nil {
			enqueueEvent(Event{
				Error:   err,
				Message: "DNS-learned policy aging failed",
			})
		}
	}
}

// pruneExpired removes DNS-learned IPs whose TTL has elapsed. QNames left with
// no IPs are removed too, so DynamicLearned does not grow without bound on a
// churny domain set.
func (m *NetPolicyManager) pruneExpired(nowNs uint64) error {
	// Fast check first: apply() recomputes the desired state, diffs three maps
	// and persists JSON. The reaper visits every sandbox on the node every
	// tick, and on the vast majority of ticks nothing has expired, so entering
	// apply unconditionally would burn CPU and /dev/shm writes for nothing.
	if !m.hasExpired(nowNs) {
		return nil
	}
	return m.apply(func(s *PolicySnapshot) error {
		for qname, entry := range s.DynamicLearned {
			if entry == nil {
				delete(s.DynamicLearned, qname)
				continue
			}
			for ip, ttl := range entry.IPs {
				if ttl.ExpiresAtNs <= nowNs {
					delete(entry.IPs, ip)
				}
			}
			if len(entry.IPs) == 0 {
				delete(s.DynamicLearned, qname)
			}
		}
		return nil
	})
}

// hasExpired reports whether this manager holds at least one learned entry
// that pruneExpired would remove. Read-only scan under m.mu so it sees a
// consistent view of DynamicLearned.
func (m *NetPolicyManager) hasExpired(nowNs uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap == nil {
		return false
	}
	for _, entry := range m.snap.DynamicLearned {
		if entry == nil || len(entry.IPs) == 0 {
			return true
		}
		for _, ttl := range entry.IPs {
			if ttl.ExpiresAtNs <= nowNs {
				return true
			}
		}
	}
	return false
}
