package cubevs

import (
	"net"
	"testing"
)

// learnWithTTL drives one answer whose lease is offsetNs from now, so a test
// can plant an already-expired address without waiting.
func learnWithTTL(t *testing.T, ifindex uint32, qname, ip string, expiresAtNs uint64) {
	t.Helper()
	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	err = mgr.apply(func(s *PolicySnapshot) error {
		entry := s.DynamicLearned[qname]
		if entry == nil {
			entry = &DNSEntry{IPs: map[string]DNSIPTTL{}}
			s.DynamicLearned[qname] = entry
		}
		entry.IPs[ip] = DNSIPTTL{ExpiresAtNs: expiresAtNs}
		return nil
	})
	if err != nil {
		t.Fatalf("seed learned lease: %v", err)
	}
}

func mustNow(t *testing.T) uint64 {
	t.Helper()
	now, err := currentNS()
	if err != nil {
		t.Fatalf("currentNS: %v", err)
	}
	return now
}

// TestPruneExpiredRetiresLapsedLease: once the last lease on an address has
// lapsed, aging reclaims the row.
func TestPruneExpiredRetiresLapsedLease(t *testing.T) {
	const ifindex = uint32(801)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	now := mustNow(t)
	learnWithTTL(t, ifindex, "qq.com", "1.2.3.4", now+uint64(nsecPerSec))
	requireAllowRow(t, ifindex, "1.2.3.4", 0)

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	// Age at a point past the lease.
	if err := mgr.pruneExpired(now + 2*uint64(nsecPerSec)); err != nil {
		t.Fatalf("pruneExpired: %v", err)
	}
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestPruneExpiredKeepsAddressHeldByAnotherQName is the shared-address case on
// the aging path. Scanning the map for lapsed expiries — which is what this
// replaced — deletes the row as soon as one holder lapses, silently
// de-authorising the domain that still holds it.
func TestPruneExpiredKeepsAddressHeldByAnotherQName(t *testing.T) {
	const ifindex = uint32(802)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"cdn.qq.com", "img.qq.com"}, nil, nil))

	now := mustNow(t)
	// cdn's lease lapses at +1s, img's is good until +100s.
	learnWithTTL(t, ifindex, "cdn.qq.com", "1.2.3.4", now+uint64(nsecPerSec))
	learnWithTTL(t, ifindex, "img.qq.com", "1.2.3.4", now+100*uint64(nsecPerSec))

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	if err := mgr.pruneExpired(now + 2*uint64(nsecPerSec)); err != nil {
		t.Fatalf("pruneExpired: %v", err)
	}

	row := requireAllowRow(t, ifindex, "1.2.3.4", 0)
	if row.ExpiresAtNS <= now+2*uint64(nsecPerSec) {
		t.Fatalf("surviving row carries the lapsed lease (%d), want img's longer one", row.ExpiresAtNS)
	}

	mgr.mu.Lock()
	_, cdnPresent := mgr.snap.DynamicLearned["cdn.qq.com"]
	_, imgPresent := mgr.snap.DynamicLearned["img.qq.com"]
	mgr.mu.Unlock()
	if cdnPresent {
		t.Fatal("the lapsed QName should have been dropped from the index")
	}
	if !imgPresent {
		t.Fatal("the live QName must remain in the index")
	}

	// Now the second holder lapses too.
	if err := mgr.pruneExpired(now + 200*uint64(nsecPerSec)); err != nil {
		t.Fatalf("pruneExpired: %v", err)
	}
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestPruneExpiredLeavesStaticRows: a static row has no expiry, so aging must
// never touch it — including when a DNS answer named the same key.
func TestPruneExpiredLeavesStaticRows(t *testing.T) {
	const ifindex = uint32(803)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"1.2.3.4", "qq.com"}, nil, nil))

	now := mustNow(t)
	learnWithTTL(t, ifindex, "qq.com", "1.2.3.4", now+uint64(nsecPerSec))

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	if err := mgr.pruneExpired(now + 1000*uint64(nsecPerSec)); err != nil {
		t.Fatalf("pruneExpired: %v", err)
	}

	row := requireAllowRow(t, ifindex, "1.2.3.4", 0)
	if row.ExpiresAtNS != 0 {
		t.Fatalf("static row expiry = %d, want 0", row.ExpiresAtNS)
	}
}

// TestHasExpiredShortCircuitsAging: the reaper visits every sandbox on every
// tick, so the cheap pre-check is what keeps that from costing a full
// desired/diff/persist cycle per sandbox per tick.
func TestHasExpiredShortCircuitsAging(t *testing.T) {
	const ifindex = uint32(804)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	now := mustNow(t)
	learnWithTTL(t, ifindex, "qq.com", "1.2.3.4", now+100*uint64(nsecPerSec))

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	if mgr.hasExpired(now) {
		t.Fatal("hasExpired must be false while every lease is live")
	}

	mgr.mu.Lock()
	generation := mgr.snap.Generation
	mgr.mu.Unlock()

	if err := mgr.pruneExpired(now); err != nil {
		t.Fatalf("pruneExpired: %v", err)
	}

	mgr.mu.Lock()
	after := mgr.snap.Generation
	mgr.mu.Unlock()
	if after != generation {
		t.Fatalf("a no-op aging pass ran a full apply (generation %d -> %d)", generation, after)
	}

	if !mgr.hasExpired(now + 200*uint64(nsecPerSec)) {
		t.Fatal("hasExpired must be true once a lease has lapsed")
	}
}

// TestHasExpiredFlagsEmptyQName: a QName left with no addresses is also
// reclaimable, so the index does not accumulate empty entries.
func TestHasExpiredFlagsEmptyQName(t *testing.T) {
	const ifindex = uint32(805)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	mgr.mu.Lock()
	mgr.snap.DynamicLearned["qq.com"] = &DNSEntry{IPs: map[string]DNSIPTTL{}}
	mgr.mu.Unlock()

	now := mustNow(t)
	if !mgr.hasExpired(now) {
		t.Fatal("an empty QName entry must be reported as reclaimable")
	}
	if err := mgr.pruneExpired(now); err != nil {
		t.Fatalf("pruneExpired: %v", err)
	}
	mgr.mu.Lock()
	_, present := mgr.snap.DynamicLearned["qq.com"]
	mgr.mu.Unlock()
	if present {
		t.Fatal("empty QName entry not removed")
	}
}

// TestAgeAllManagersSkipsRemovedManager: a torn-down manager must not be
// resurrected by the aging pass.
func TestAgeAllManagersSkipsRemovedManager(t *testing.T) {
	const ifindex = uint32(806)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	if err := CleanupTAPDevicePolicy(ifindex); err != nil {
		t.Fatalf("CleanupTAPDevicePolicy: %v", err)
	}
	// Must not panic or write anything.
	ageAllManagers(mustNow(t) + 1_000_000*uint64(nsecPerSec))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestLearnDoesNotShortenAnExistingLease: two answers for the same address can
// carry different TTLs; the longer lease must win so a short answer cannot pull
// an address out from under a longer-lived one.
func TestLearnDoesNotShortenAnExistingLease(t *testing.T) {
	const ifindex = uint32(807)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	long := []DNSAnswer{{IP: net.ParseIP("1.2.3.4"), TTLSeconds: dnsMaxTTL}}
	if err := mgr.LearnDNS("qq.com", long); err != nil {
		t.Fatalf("LearnDNS: %v", err)
	}
	before := requireAllowRow(t, ifindex, "1.2.3.4", 0).ExpiresAtNS

	short := []DNSAnswer{{IP: net.ParseIP("1.2.3.4"), TTLSeconds: dnsMinTTL}}
	if err := mgr.LearnDNS("qq.com", short); err != nil {
		t.Fatalf("LearnDNS: %v", err)
	}
	after := requireAllowRow(t, ifindex, "1.2.3.4", 0).ExpiresAtNS

	if after < before {
		t.Fatalf("lease shortened from %d to %d", before, after)
	}
}
