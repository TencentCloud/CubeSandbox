package cubevs

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/cilium/ebpf"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// newPolicyManagerTest pins fresh policy maps on a scratch bpffs, points
// snapshots at a temp dir, and clears the process-wide registry and inner-map
// cache so each test starts from nothing.
//
// Never touches the real /sys/fs/bpf: a Cubelet may be running on this host.
func newPolicyManagerTest(t *testing.T, ifindex uint32) {
	t.Helper()
	pinPolicyMaps(t, ifindex)
	pinTAPMetadataMaps(t)

	// setDNSPolicyFlags and bumpPolicyVersion do a read-modify-write, so the
	// sandbox needs the metadata row a registered TAP would have.
	if err := UpsertTAPDeviceMetadata(ifindex, net.IPv4(10, 0, 0, 1), "policy-manager-test", 1); err != nil {
		t.Fatalf("seed TAP metadata: %v", err)
	}

	SetPolicySnapshotDir(t.TempDir())
	resetPolicyManagerRegistry(t)
}

func resetPolicyManagerRegistry(t *testing.T) {
	t.Helper()
	clear := func() {
		managerRegistryMu.Lock()
		managerRegistry = map[uint32]*NetPolicyManager{}
		managerInit = map[uint32]chan struct{}{}
		managerRegistryMu.Unlock()
		clearPolicyInnerCacheForTest()
	}
	clear()
	t.Cleanup(clear)
}

// restartProcess simulates a Cubelet restart: the in-memory registry and the
// inner-map cache are lost, while the pinned maps and the snapshot files on
// tmpfs survive.
func restartProcess(t *testing.T) {
	t.Helper()
	managerRegistryMu.Lock()
	managerRegistry = map[uint32]*NetPolicyManager{}
	managerInit = map[uint32]chan struct{}{}
	managerRegistryMu.Unlock()
	clearPolicyInnerCacheForTest()
}

func mvmOptions(allowOut []string, l7 []L7Target, denyOut []string) MVMOptions {
	opts := MVMOptions{}
	if allowOut != nil {
		opts.AllowOut = &allowOut
	}
	if l7 != nil {
		opts.L7AllowOut = &l7
	}
	if denyOut != nil {
		opts.DenyOut = &denyOut
	}
	return opts
}

// learn drives one DNS response into the manager for ifindex.
func learn(t *testing.T, ifindex uint32, qname string, ips ...string) {
	t.Helper()
	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	answers := make([]DNSAnswer, 0, len(ips))
	for _, ip := range ips {
		answers = append(answers, DNSAnswer{IP: net.ParseIP(ip), TTLSeconds: 600})
	}
	if err := mgr.LearnDNS(qname, answers); err != nil {
		t.Fatalf("LearnDNS(%q): %v", qname, err)
	}
}

func mustUpdatePolicy(t *testing.T, ifindex uint32, opts MVMOptions) {
	t.Helper()
	if err := UpdateTAPDevicePolicy(ifindex, opts); err != nil {
		t.Fatalf("UpdateTAPDevicePolicy: %v", err)
	}
}

func allowKey(t *testing.T, ip string, port uint16) lpmKeyV3 {
	t.Helper()
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		t.Fatalf("bad test IP %q", ip)
	}
	key := lpmKeyV3{Prefixlen: 32, IP: ipToUint32(addr)}
	if port != 0 {
		key.Prefixlen = 48
		key.Port = htonsPort(port)
	}
	return key
}

// allowRow reports the installed row for (ip, port), if any.
func allowRow(t *testing.T, ifindex uint32, ip string, port uint16) (netPolicyValueV3, bool) {
	t.Helper()
	inner := mustInner(t, MapNameAllowOutV3, ifindex)
	key := allowKey(t, ip, port)
	var value netPolicyValueV3
	err := inner.Lookup(&key, &value)
	if err != nil {
		return netPolicyValueV3{}, false
	}
	// LPM lookups are longest-prefix: a hit under a shorter covering key is
	// not the row we asked about.
	if value.KeyPrefixlen != uint8(key.Prefixlen) {
		return netPolicyValueV3{}, false
	}
	return value, true
}

func requireAllowRow(t *testing.T, ifindex uint32, ip string, port uint16) netPolicyValueV3 {
	t.Helper()
	value, ok := allowRow(t, ifindex, ip, port)
	if !ok {
		t.Fatalf("allow_out_v3 row for %s:%d missing, want present", ip, port)
	}
	return value
}

func requireNoAllowRow(t *testing.T, ifindex uint32, ip string, port uint16) {
	t.Helper()
	if value, ok := allowRow(t, ifindex, ip, port); ok {
		t.Fatalf("allow_out_v3 row for %s:%d still present: %+v", ip, port, value)
	}
}

// ---------------------------------------------------------------------------
// T01 — revoking a domain retires the addresses it taught, immediately
// ---------------------------------------------------------------------------

// TestRevokeDomainRetiresLearnedRowsImmediately is the whole point of moving
// learning to user space: dropping a domain rule must remove the rows it
// taught without waiting for a TTL or a reaper pass.
func TestRevokeDomainRetiresLearnedRowsImmediately(t *testing.T) {
	const ifindex = uint32(701)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	row := requireAllowRow(t, ifindex, "1.2.3.4", 0)
	if row.ExpiresAtNS == 0 {
		t.Fatal("learned row must carry a TTL, not a static zero expiry")
	}

	// No clock advance, no reaper run.
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, nil, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestRevokeL7DomainRetiresLearnedPortRows is the same guarantee for the /48
// rows an L7 domain rule teaches.
func TestRevokeL7DomainRetiresLearnedPortRows(t *testing.T) {
	const ifindex = uint32(702)
	newPolicyManagerTest(t, ifindex)

	l7 := []L7Target{{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP}}
	mustUpdatePolicy(t, ifindex, mvmOptions(nil, l7, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	row := requireAllowRow(t, ifindex, "1.2.3.4", 8080)
	if row.Scheme != L7SchemeHTTP {
		t.Fatalf("scheme = %d, want HTTP", row.Scheme)
	}

	mustUpdatePolicy(t, ifindex, mvmOptions(nil, []L7Target{}, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 8080)
}

// TestRevokeDomainWithL3CoexistenceRetiresBothRowShapes: a host that is both
// plain-allowed and L7-ruled teaches a /32 and one /48 per port. Revoking must
// take all of them.
func TestRevokeDomainWithL3CoexistenceRetiresBothRowShapes(t *testing.T) {
	const ifindex = uint32(703)
	newPolicyManagerTest(t, ifindex)

	l7 := []L7Target{{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP}}
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, l7, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	requireAllowRow(t, ifindex, "1.2.3.4", 8080)
	plain := requireAllowRow(t, ifindex, "1.2.3.4", 0)
	if plain.Flags&uint8(netPolicyFlagL7Required) != 0 {
		t.Fatalf("plain /32 must not carry the L7 marker: %#x", plain.Flags)
	}

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, []L7Target{}, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 8080)
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// ---------------------------------------------------------------------------
// T02 — shared addresses
// ---------------------------------------------------------------------------

// TestSharedLearnedIPSurvivesPartialRevoke is the case a per-row provenance
// stamp cannot get right: two domains resolve to one address, so the row must
// live until the last of them is revoked. Any implementation that deletes by
// "the addresses this domain taught" fails here.
func TestSharedLearnedIPSurvivesPartialRevoke(t *testing.T) {
	const ifindex = uint32(704)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"cdn.qq.com", "img.qq.com"}, nil, nil))
	learn(t, ifindex, "cdn.qq.com", "1.2.3.4")
	learn(t, ifindex, "img.qq.com", "1.2.3.4")
	requireAllowRow(t, ifindex, "1.2.3.4", 0)

	// Revoke one holder: the other still authorises the address.
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"img.qq.com"}, nil, nil))
	requireAllowRow(t, ifindex, "1.2.3.4", 0)

	// Revoke the last holder: now it goes.
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, nil, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestWildcardRevokeRetiresAllItsSubdomains: subdomains matched by one wildcard
// all lose their addresses together, since they all stop matching at once.
func TestWildcardRevokeRetiresAllItsSubdomains(t *testing.T) {
	const ifindex = uint32(705)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"*.qq.com"}, nil, nil))
	learn(t, ifindex, "a.qq.com", "1.2.3.4")
	learn(t, ifindex, "b.qq.com", "5.6.7.8")
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
	requireAllowRow(t, ifindex, "5.6.7.8", 0)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, nil, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
	requireNoAllowRow(t, ifindex, "5.6.7.8", 0)
}

// ---------------------------------------------------------------------------
// T03 — the rule survives but its L7 port set changes
// ---------------------------------------------------------------------------

// TestL7PortSetChangeRewritesLearnedRows: rows are derived from the *current*
// rule on every apply, so a changed port set moves the learned /48s without
// needing a fresh DNS response.
func TestL7PortSetChangeRewritesLearnedRows(t *testing.T) {
	const ifindex = uint32(706)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions(nil,
		[]L7Target{{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP}}, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")
	requireAllowRow(t, ifindex, "1.2.3.4", 8080)

	mustUpdatePolicy(t, ifindex, mvmOptions(nil,
		[]L7Target{{Host: "qq.com", Port: 9090, Scheme: L7SchemeHTTPS}}, nil))

	requireNoAllowRow(t, ifindex, "1.2.3.4", 8080)
	row := requireAllowRow(t, ifindex, "1.2.3.4", 9090)
	if row.Scheme != L7SchemeHTTPS {
		t.Fatalf("scheme = %d, want HTTPS", row.Scheme)
	}
}

func TestL7PortSetGrowthAddsLearnedRows(t *testing.T) {
	const ifindex = uint32(707)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions(nil,
		[]L7Target{{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP}}, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	mustUpdatePolicy(t, ifindex, mvmOptions(nil, []L7Target{
		{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP},
		{Host: "qq.com", Port: 9090, Scheme: L7SchemeHTTPS},
	}, nil))

	requireAllowRow(t, ifindex, "1.2.3.4", 8080)
	requireAllowRow(t, ifindex, "1.2.3.4", 9090)
}

// TestL7PortSetResetToDefaultsRewritesLearnedRows: dropping the explicit port
// set falls back to {80, 443}, and the old explicit row must go.
func TestL7PortSetResetToDefaultsRewritesLearnedRows(t *testing.T) {
	const ifindex = uint32(708)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions(nil,
		[]L7Target{{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP}}, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	mustUpdatePolicy(t, ifindex, mvmOptions(nil, []L7Target{{Host: "qq.com"}}, nil))

	requireNoAllowRow(t, ifindex, "1.2.3.4", 8080)
	requireAllowRow(t, ifindex, "1.2.3.4", 80)
	requireAllowRow(t, ifindex, "1.2.3.4", 443)
}

// ---------------------------------------------------------------------------
// T06 / T19 — what must not be learned, and what must
// ---------------------------------------------------------------------------

// TestLearnRejectsUnmatchedQName: a response for a name no rule covers must
// touch neither the index nor the maps.
func TestLearnRejectsUnmatchedQName(t *testing.T) {
	const ifindex = uint32(709)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	learn(t, ifindex, "evil.com", "9.9.9.9")

	requireNoAllowRow(t, ifindex, "9.9.9.9", 0)

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	mgr.mu.Lock()
	_, tracked := mgr.snap.DynamicLearned["evil.com"]
	mgr.mu.Unlock()
	if tracked {
		t.Fatal("unmatched QName must not enter the learned index")
	}
}

// TestLearnRejectsApexUnderWildcardOnly: "*.qq.com" does not cover the apex, so
// a reply for "qq.com" must not be learned.
func TestLearnRejectsApexUnderWildcardOnly(t *testing.T) {
	const ifindex = uint32(710)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"*.qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)

	learn(t, ifindex, "a.qq.com", "5.6.7.8")
	requireAllowRow(t, ifindex, "5.6.7.8", 0)
}

// TestDomainOnlyPolicyStillLearns is a regression guard on the create path.
// A sandbox whose policy names only domains has an empty static allow_out set;
// an install path that skipped its work in that case would leave the sandbox
// without a domain index and silently never learn anything.
func TestDomainOnlyPolicyStillLearns(t *testing.T) {
	const ifindex = uint32(711)
	newPolicyManagerTest(t, ifindex)

	// applyNetPolicy, not UpdateTAPDevicePolicy: this is the create path.
	if err := applyNetPolicy(ifindex, mvmOptions([]string{"qq.com"}, nil, nil)); err != nil {
		t.Fatalf("applyNetPolicy: %v", err)
	}
	learn(t, ifindex, "qq.com", "1.2.3.4")
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
}

// ---------------------------------------------------------------------------
// T20 — a learned row must never demote a static one
// ---------------------------------------------------------------------------

// TestLearnDoesNotExpireStaticRow: when a static rule and a DNS answer name the
// same key, the static (zero) expiry wins. Otherwise the row would age out and
// the reaper would delete a control-plane rule.
func TestLearnDoesNotExpireStaticRow(t *testing.T) {
	const ifindex = uint32(712)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"1.2.3.4", "qq.com"}, nil, nil))
	if row := requireAllowRow(t, ifindex, "1.2.3.4", 0); row.ExpiresAtNS != 0 {
		t.Fatalf("static row expiry = %d, want 0", row.ExpiresAtNS)
	}

	learn(t, ifindex, "qq.com", "1.2.3.4")

	row := requireAllowRow(t, ifindex, "1.2.3.4", 0)
	if row.ExpiresAtNS != 0 {
		t.Fatalf("static row picked up a TTL (%d) from a DNS answer", row.ExpiresAtNS)
	}
}

// TestLearnDoesNotExpireStaticL7Row is the same guarantee on an exact /48.
func TestLearnDoesNotExpireStaticL7Row(t *testing.T) {
	const ifindex = uint32(713)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"},
		[]L7Target{
			{Host: "1.2.3.4", Port: 8080, Scheme: L7SchemeHTTP},
			{Host: "qq.com", Port: 8080, Scheme: L7SchemeHTTP},
		}, nil))
	if row := requireAllowRow(t, ifindex, "1.2.3.4", 8080); row.ExpiresAtNS != 0 {
		t.Fatalf("static /48 expiry = %d, want 0", row.ExpiresAtNS)
	}

	learn(t, ifindex, "qq.com", "1.2.3.4")

	if row := requireAllowRow(t, ifindex, "1.2.3.4", 8080); row.ExpiresAtNS != 0 {
		t.Fatalf("static /48 picked up a TTL (%d)", row.ExpiresAtNS)
	}
}

// TestStaticRuleRemovalStillRetiresRowHeldOnlyByStatic: once the static rule is
// gone and no QName holds the key, the row must go.
func TestStaticRuleRemovalStillRetiresRowHeldOnlyByStatic(t *testing.T) {
	const ifindex = uint32(714)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"1.2.3.4"}, nil, nil))
	requireAllowRow(t, ifindex, "1.2.3.4", 0)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, nil, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// ---------------------------------------------------------------------------
// T04 — the index has to survive a Cubelet restart
// ---------------------------------------------------------------------------

// TestRevokeAfterRestartStillRetiresLearnedRows: the pinned maps outlive the
// process, so if the learned index did not, an update after a restart could
// only manage static rows — exactly the gap this work closes.
func TestRevokeAfterRestartStillRetiresLearnedRows(t *testing.T) {
	const ifindex = uint32(715)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")
	requireAllowRow(t, ifindex, "1.2.3.4", 0)

	restartProcess(t)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, nil, nil))
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestRestartPreservesLearnedRowsWhenRuleSurvives is the other half: a restart
// must not blow away learning that is still authorised.
func TestRestartPreservesLearnedRowsWhenRuleSurvives(t *testing.T) {
	const ifindex = uint32(716)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	restartProcess(t)

	// Touching the manager triggers load + reconcile.
	if _, err := GetNetPolicyManager(ifindex); err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestRestartWithoutSnapshotKeepsStaticRows (T28): a lost or corrupt snapshot
// must never present "desired is empty" to the diff and wipe the sandbox's
// policy. reconcile adopts the live maps as its mirror instead.
func TestRestartWithoutSnapshotKeepsStaticRows(t *testing.T) {
	const ifindex = uint32(717)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"1.2.3.4", "qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "5.6.7.8")

	// Destroy the snapshot, then restart.
	dir := currentPolicySnapshotDir()
	if err := os.WriteFile(filepath.Join(dir, "717.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}
	restartProcess(t)

	if _, err := GetNetPolicyManager(ifindex); err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	// Everything the sandbox had must still be there.
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
	requireAllowRow(t, ifindex, "5.6.7.8", 0)
}

// TestRestartRepairsStaticDrift (T26): the snapshot's static plan is what the
// control plane last installed, and nothing re-pushes it after a restart —
// recovery is the only thing that can repair the maps. So a static row the
// plan does not name must go, and one it names that the map has lost must
// come back.
func TestRestartRepairsStaticDrift(t *testing.T) {
	const ifindex = uint32(718)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"198.51.100.7", "qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")
	restartProcess(t)

	// Drift in both directions: a row nothing installed, and a planned row
	// that vanished from the map.
	inner := mustInner(t, MapNameAllowOutV3, ifindex)
	foreign := allowKey(t, "203.0.113.9", 0)
	if err := inner.Update(&foreign, &netPolicyValueV3{KeyPrefixlen: 32}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed foreign static row: %v", err)
	}
	planned := allowKey(t, "198.51.100.7", 0)
	if err := inner.Delete(&planned); err != nil {
		t.Fatalf("drop planned static row: %v", err)
	}

	// Touching the manager triggers load + reconcile, which converges both.
	if _, err := GetNetPolicyManager(ifindex); err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	requireNoAllowRow(t, ifindex, "203.0.113.9", 0)
	requireAllowRow(t, ifindex, "198.51.100.7", 0)
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestRestartKeepsDenyAndDomainRows: deny_out and dns_allow_v2 are entirely
// static, so recovery must reproduce them exactly from the persisted plan. A
// converge that recomputed them from an empty desired set would delete the
// always-denied private ranges — opening the host network to the sandbox — and
// drop every domain rule, silently stopping DNS learning.
func TestRestartKeepsDenyAndDomainRows(t *testing.T) {
	const ifindex = uint32(719)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions(
		[]string{"qq.com"}, nil, []string{"198.51.100.0/24"}))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	denyBefore := mustDenyCIDRs(t, ifindex)
	dnsBefore := mustDNSDomains(t, ifindex)

	restartProcess(t)

	// The sequence a DNS event takes after a restart: load, reconcile against
	// the live maps, then this learn's apply.
	learn(t, ifindex, "qq.com", "1.2.3.5")

	if got := mustDenyCIDRs(t, ifindex); !reflect.DeepEqual(got, denyBefore) {
		t.Errorf("deny_out changed across a restart+learn:\n got %v\nwant %v", got, denyBefore)
	}
	if got := mustDNSDomains(t, ifindex); !reflect.DeepEqual(got, dnsBefore) {
		t.Errorf("dns_allow_v2 changed across a restart+learn:\n got %v\nwant %v", got, dnsBefore)
	}
	// The learn itself still landed.
	requireAllowRow(t, ifindex, "1.2.3.5", 0)
}

// mustDenyCIDRs returns the sorted CIDRs installed in deny_out[ifindex].
func mustDenyCIDRs(t *testing.T, ifindex uint32) []string {
	t.Helper()
	rows, err := dumpDenyOutMirror(ifindex)
	if err != nil {
		t.Fatalf("dump %s: %v", MapNameDenyOut, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.CIDR)
	}
	sort.Strings(out)
	return out
}

// mustDNSDomains returns the sorted domain patterns installed in
// dns_allow_v2[ifindex].
func mustDNSDomains(t *testing.T, ifindex uint32) []string {
	t.Helper()
	rows, err := dumpDNSAllowMirror(ifindex)
	if err != nil {
		t.Fatalf("dump %s: %v", MapNameDNSAllowV2, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Domain)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// T05 — concurrency
// ---------------------------------------------------------------------------

// TestConcurrentLearnAndUpdateNeverResurrectsRevokedIP: learning and updating
// share the per-ifindex lock, so a response in flight when the rule is revoked
// either lands before the update (and is diffed away) or after it (and fails to
// match). Both orders must converge to "not allowed".
func TestConcurrentLearnAndUpdateNeverResurrectsRevokedIP(t *testing.T) {
	const ifindex = uint32(719)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		answers := []DNSAnswer{{IP: net.ParseIP("1.2.3.4"), TTLSeconds: 600}}
		for {
			select {
			case <-stop:
				return
			default:
				_ = mgr.LearnDNS("qq.com", answers)
			}
		}
	}()

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{}, nil, nil))
	close(stop)
	wg.Wait()

	// The revoking update is the last control-plane word; any learn that raced
	// it saw the new (empty) index and did not write.
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)
}

// ---------------------------------------------------------------------------
// T23 — a torn-down TAP must not be resurrected
// ---------------------------------------------------------------------------

// TestLearnAfterCleanupIsNoOp: a DNS event can arrive after teardown. It must
// not recreate the snapshot or write rows.
func TestLearnAfterCleanupIsNoOp(t *testing.T) {
	const ifindex = uint32(720)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}

	if err := CleanupTAPDevicePolicy(ifindex); err != nil {
		t.Fatalf("CleanupTAPDevicePolicy: %v", err)
	}

	// The poller may still hold the old manager reference.
	if err := mgr.LearnDNS("qq.com", []DNSAnswer{
		{IP: net.ParseIP("1.2.3.4"), TTLSeconds: 600},
	}); err != nil {
		t.Fatalf("late LearnDNS should be a benign no-op, got %v", err)
	}
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)

	if _, err := os.Stat(filepath.Join(currentPolicySnapshotDir(), "720.json")); !os.IsNotExist(err) {
		t.Fatal("snapshot must be removed on cleanup")
	}
}

// TestIfindexReuseStartsClean (T11): a recycled ifindex must not inherit the
// previous sandbox's learned addresses.
func TestIfindexReuseStartsClean(t *testing.T) {
	const ifindex = uint32(721)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	learn(t, ifindex, "qq.com", "1.2.3.4")
	requireAllowRow(t, ifindex, "1.2.3.4", 0)

	if err := CleanupTAPDevicePolicy(ifindex); err != nil {
		t.Fatalf("CleanupTAPDevicePolicy: %v", err)
	}

	// A new sandbox lands on the same ifindex.
	if err := applyNetPolicy(ifindex, mvmOptions([]string{"other.com"}, nil, nil)); err != nil {
		t.Fatalf("applyNetPolicy: %v", err)
	}
	requireNoAllowRow(t, ifindex, "1.2.3.4", 0)

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	mgr.mu.Lock()
	_, stale := mgr.snap.DynamicLearned["qq.com"]
	mgr.mu.Unlock()
	if stale {
		t.Fatal("recycled ifindex inherited the previous sandbox's learned QName")
	}
}

// ---------------------------------------------------------------------------
// T25 — the three control-plane paths must agree
// ---------------------------------------------------------------------------

// TestApplyPathsAgree: create, recovery-replace and update are one operation
// now, so the same options must produce byte-identical map contents whichever
// entry point installs them.
func TestApplyPathsAgree(t *testing.T) {
	opts := mvmOptions(
		[]string{"198.51.100.7", "qq.com"},
		[]L7Target{{Host: "203.0.113.5", Port: 8443, Scheme: L7SchemeHTTPS}},
		[]string{"203.0.113.0/24"},
	)

	paths := map[string]func(uint32) error{
		"applyNetPolicy":        func(i uint32) error { return applyNetPolicy(i, opts) },
		"replaceNetPolicy":      func(i uint32) error { return replaceNetPolicy(i, opts) },
		"UpdateTAPDevicePolicy": func(i uint32) error { return UpdateTAPDevicePolicy(i, opts) },
	}

	var reference []allowMirrorEntry
	var refName string
	for name, apply := range paths {
		ifindex := uint32(730)
		t.Run(name, func(t *testing.T) {
			newPolicyManagerTest(t, ifindex)
			if err := apply(ifindex); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got, err := dumpAllowOutMirror(ifindex)
			if err != nil {
				t.Fatalf("dump mirror: %v", err)
			}
			if reference == nil {
				reference, refName = got, name
				return
			}
			if len(got) != len(reference) {
				t.Fatalf("%s installed %d rows, %s installed %d", name, len(got), refName, len(reference))
			}
			for i := range got {
				if got[i] != reference[i] {
					t.Fatalf("%s row %d = %+v, %s has %+v", name, i, got[i], refName, reference[i])
				}
			}
		})
	}
}

// TestReapplyIsIdempotent: applying the same plan twice must produce an empty
// second diff. This is what makes retry-on-failure safe, and it also proves the
// desired state has no nondeterministic ordering.
func TestReapplyIsIdempotent(t *testing.T) {
	const ifindex = uint32(731)
	newPolicyManagerTest(t, ifindex)

	opts := mvmOptions([]string{"198.51.100.7", "qq.com"},
		[]L7Target{{Host: "qq.com", Port: 8443, Scheme: L7SchemeHTTPS}}, nil)
	mustUpdatePolicy(t, ifindex, opts)
	learn(t, ifindex, "qq.com", "1.2.3.4")

	mgr, err := GetNetPolicyManager(ifindex)
	if err != nil {
		t.Fatalf("GetNetPolicyManager: %v", err)
	}
	mgr.mu.Lock()
	before := append([]allowMirrorEntry(nil), mgr.snap.AllowMirror...)
	mgr.mu.Unlock()

	mustUpdatePolicy(t, ifindex, opts)

	mgr.mu.Lock()
	after := append([]allowMirrorEntry(nil), mgr.snap.AllowMirror...)
	mgr.mu.Unlock()

	if len(before) != len(after) {
		t.Fatalf("mirror size changed on reapply: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("mirror row %d changed on reapply: %+v -> %+v", i, before[i], after[i])
		}
	}
	if diff := diffAllowOut(before, after); !diff.empty() {
		t.Fatalf("reapply produced a non-empty diff: %+v", diff)
	}
}

// ---------------------------------------------------------------------------
// T16 — deterministic conflict resolution
// ---------------------------------------------------------------------------

// TestSharedKeySchemeConflictIsDeterministic: two domains can claim the same
// (ip, port) with different schemes, which buildL7Plan cannot catch because it
// only checks within one host. allow_out_v3 has a single slot, so one loses —
// but the outcome must not depend on Go's map iteration order, or every apply
// would rewrite the row.
func TestSharedKeySchemeConflictIsDeterministic(t *testing.T) {
	rs := buildDomainRuleSetFromPersisted([]DomainRule{
		{
			Domain: "a.example.com", Flags: uint8(netPolicyFlagL7Required),
			Ports: []L7Port{{Port: 8080, Scheme: L7SchemeHTTP}},
		},
		{
			Domain: "b.example.com", Flags: uint8(netPolicyFlagL7Required),
			Ports: []L7Port{{Port: 8080, Scheme: L7SchemeHTTPS}},
		},
	})
	snap := newEmptySnapshot(1)
	snap.DynamicLearned = map[string]*DNSEntry{
		"a.example.com": {IPs: map[string]DNSIPTTL{"1.2.3.4": {ExpiresAtNs: 1 << 40}}},
		"b.example.com": {IPs: map[string]DNSIPTTL{"1.2.3.4": {ExpiresAtNs: 1 << 40}}},
	}

	// Repeat: map iteration order is randomised per range, so a dependency on
	// it shows up across runs.
	var first []allowMirrorEntry
	for i := 0; i < 32; i++ {
		got := computeEffective(snap, rs, 0).allowOut
		if first == nil {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("row count varies across runs: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d row %d = %+v, first run had %+v", i, j, got[j], first[j])
			}
		}
	}

	var row *allowMirrorEntry
	for i := range first {
		if first[i].Port == 8080 {
			row = &first[i]
		}
	}
	if row == nil {
		t.Fatal("no /48 row for port 8080")
	}
	// "a.example.com" sorts first, so its scheme wins.
	if row.Scheme != L7SchemeHTTP {
		t.Fatalf("scheme = %d, want HTTP (lexicographically first QName)", row.Scheme)
	}
}

// ---------------------------------------------------------------------------
// T18 — persistence
// ---------------------------------------------------------------------------

func TestSnapshotRoundTrip(t *testing.T) {
	const ifindex = uint32(740)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"198.51.100.7", "qq.com"},
		[]L7Target{{Host: "qq.com", Port: 8443, Scheme: L7SchemeHTTPS}}, []string{"203.0.113.0/24"}))
	learn(t, ifindex, "qq.com", "1.2.3.4")

	path := filepath.Join(currentPolicySnapshotDir(), "740.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("snapshot mode = %o, want 0600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	snap, err := unmarshalSnapshot(data)
	if err != nil {
		t.Fatalf("unmarshalSnapshot: %v", err)
	}
	if snap.SchemaVersion != policySnapshotSchemaV1 || snap.Ifindex != ifindex {
		t.Fatalf("snapshot header = %+v", snap)
	}
	entry := snap.DynamicLearned["qq.com"]
	if entry == nil || len(entry.IPs) != 1 {
		t.Fatalf("learned state not persisted: %+v", snap.DynamicLearned)
	}
	if _, ok := entry.IPs["1.2.3.4"]; !ok {
		t.Fatalf("learned IP not persisted: %+v", entry.IPs)
	}
	if len(snap.DomainRules) == 0 || len(snap.AllowOut) == 0 || len(snap.DenyOut) == 0 {
		t.Fatalf("static plan not persisted: %+v", snap)
	}
}

// TestSnapshotPreservesUnknownFields: a newer binary may write fields this one
// does not know. Round-tripping through here must not drop them, or a rollback
// followed by a roll-forward would lose them permanently.
func TestSnapshotPreservesUnknownFields(t *testing.T) {
	raw := []byte(`{
		"schema_version": 1,
		"ifindex": 750,
		"future_field": {"nested": [1, 2, 3]}
	}`)
	snap, err := unmarshalSnapshot(raw)
	if err != nil {
		t.Fatalf("unmarshalSnapshot: %v", err)
	}
	if _, ok := snap.Extra["future_field"]; !ok {
		t.Fatalf("unknown field dropped: %+v", snap.Extra)
	}

	out, err := marshalSnapshot(snap)
	if err != nil {
		t.Fatalf("marshalSnapshot: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode re-marshalled snapshot: %v", err)
	}
	if _, ok := decoded["future_field"]; !ok {
		t.Fatalf("unknown field not re-emitted: %s", out)
	}
}

func TestLoadRejectsUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	SetPolicySnapshotDir(dir)
	path := filepath.Join(dir, "760.json")
	if err := os.WriteFile(path, []byte(`{"schema_version": 999, "ifindex": 760}`), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	m := &NetPolicyManager{ifindex: 760, path: path}
	if err := m.load(); err == nil {
		t.Fatal("an unknown schema version must be rejected so the caller starts clean")
	}
}

// ---------------------------------------------------------------------------
// T14 — derived rows must match the retired BPF learner
// ---------------------------------------------------------------------------

// TestLearnedAllowOutRowsMatchRetiredDatapath is the golden-vector test for the
// port: these are the exact rows dns_learn_response_ip used to write.
func TestLearnedAllowOutRowsMatchRetiredDatapath(t *testing.T) {
	const ip = "1.2.3.4"
	const expires = uint64(12345)
	addr := ipToUint32(net.ParseIP(ip).To4())

	cases := []struct {
		name string
		rule domainRule
		want []allowOutRow
	}{
		{
			name: "plain domain learns one any-port row",
			rule: domainRule{},
			want: []allowOutRow{{
				key: lpmKeyV3{Prefixlen: 32, IP: addr},
				val: netPolicyValueV3{ExpiresAtNS: expires, KeyPrefixlen: 32},
			}},
		},
		{
			name: "L7 explicit port",
			rule: domainRule{
				flags: uint8(netPolicyFlagL7Required),
				ports: []l7PortEntry{{Port: htonsPort(8080), Scheme: L7SchemeHTTP}},
			},
			want: []allowOutRow{{
				key: lpmKeyV3{Prefixlen: 48, IP: addr, Port: htonsPort(8080)},
				val: netPolicyValueV3{
					ExpiresAtNS: expires, Flags: uint8(netPolicyFlagL7Required),
					Scheme: L7SchemeHTTP, KeyPrefixlen: 48,
				},
			}},
		},
		{
			name: "L7 with no explicit ports falls back to 80/443",
			rule: domainRule{flags: uint8(netPolicyFlagL7Required)},
			want: []allowOutRow{
				{
					key: lpmKeyV3{Prefixlen: 48, IP: addr, Port: htonsPort(80)},
					val: netPolicyValueV3{
						ExpiresAtNS: expires, Flags: uint8(netPolicyFlagL7Required),
						Scheme: L7SchemeHTTP, KeyPrefixlen: 48,
					},
				},
				{
					key: lpmKeyV3{Prefixlen: 48, IP: addr, Port: htonsPort(443)},
					val: netPolicyValueV3{
						ExpiresAtNS: expires, Flags: uint8(netPolicyFlagL7Required),
						Scheme: L7SchemeHTTPS, KeyPrefixlen: 48,
					},
				},
			},
		},
		{
			name: "L7 plus plain cover also learns the any-port row with markers stripped",
			rule: domainRule{
				flags: uint8(netPolicyFlagL7Required | netPolicyFlagL3Allowed),
				ports: []l7PortEntry{{Port: htonsPort(8080), Scheme: L7SchemeHTTP}},
			},
			want: []allowOutRow{
				{
					key: lpmKeyV3{Prefixlen: 48, IP: addr, Port: htonsPort(8080)},
					val: netPolicyValueV3{
						ExpiresAtNS: expires,
						Flags:       uint8(netPolicyFlagL7Required | netPolicyFlagL3Allowed),
						Scheme:      L7SchemeHTTP, KeyPrefixlen: 48,
					},
				},
				{
					key: lpmKeyV3{Prefixlen: 32, IP: addr},
					val: netPolicyValueV3{ExpiresAtNS: expires, KeyPrefixlen: 32},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := learnedAllowOutRows(ip, &tc.rule, expires)
			if len(got) != len(tc.want) {
				t.Fatalf("rows = %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("row %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLearnedAllowOutRowsRejectsBadIP(t *testing.T) {
	if rows := learnedAllowOutRows("not-an-ip", &domainRule{}, 1); rows != nil {
		t.Fatalf("expected no rows for a malformed address, got %+v", rows)
	}
}

// ---------------------------------------------------------------------------
// T29 — diff ordering and determinism
// ---------------------------------------------------------------------------

// TestDiffAllowOutOrdersMoreSpecificFirst: within a phase the operations are
// sorted by prefix length descending, so the more specific rows land first and
// the sequence is reproducible.
func TestDiffAllowOutOrdersMoreSpecificFirst(t *testing.T) {
	next := []allowMirrorEntry{
		{IP: "1.2.3.4", Prefixlen: 32},
		{IP: "1.2.3.4", Prefixlen: 48, Port: 443, Flags: uint8(netPolicyFlagL7Required), Scheme: L7SchemeHTTPS},
		{IP: "1.2.3.4", Prefixlen: 48, Port: 80, Flags: uint8(netPolicyFlagL7Required), Scheme: L7SchemeHTTP},
	}
	diff := diffAllowOut(nil, next)
	if len(diff.upsert) != 3 {
		t.Fatalf("upserts = %d, want 3", len(diff.upsert))
	}
	if diff.upsert[0].key.Prefixlen != 48 || diff.upsert[1].key.Prefixlen != 48 {
		t.Fatalf("expected the /48 rows first: %+v", diff.upsert)
	}
	if diff.upsert[2].key.Prefixlen != 32 {
		t.Fatalf("expected the /32 row last: %+v", diff.upsert)
	}
	// Ports ascending within the same prefix length keeps it fully ordered.
	if ntohsPort(diff.upsert[0].key.Port) != 80 || ntohsPort(diff.upsert[1].key.Port) != 443 {
		t.Fatalf("expected ports ascending: %+v", diff.upsert)
	}
}

func TestDiffAllowOutDetectsValueChange(t *testing.T) {
	prev := []allowMirrorEntry{{IP: "1.2.3.4", Prefixlen: 32, ExpiresAtNs: 100}}
	same := diffAllowOut(prev, prev)
	if !same.empty() {
		t.Fatalf("identical states must diff empty: %+v", same)
	}

	next := []allowMirrorEntry{{IP: "1.2.3.4", Prefixlen: 32, ExpiresAtNs: 200}}
	renewed := diffAllowOut(prev, next)
	if len(renewed.del) != 0 || len(renewed.upsert) != 1 {
		t.Fatalf("a renewed TTL must be one upsert and no delete: %+v", renewed)
	}
}

func TestDiffDenyOutConverges(t *testing.T) {
	prev := []denyMirrorEntry{{CIDR: "10.0.0.0/8"}, {CIDR: "192.168.0.0/16"}}
	next := []denyMirrorEntry{{CIDR: "10.0.0.0/8"}, {CIDR: "172.16.0.0/12"}}
	diff := diffDenyOut(prev, next)
	if len(diff.del) != 1 || len(diff.upsert) != 1 {
		t.Fatalf("diff = %+v, want one delete and one insert", diff)
	}
	if formatLPMKey(diff.del[0]) != "192.168.0.0/16" {
		t.Fatalf("deleted %s, want 192.168.0.0/16", formatLPMKey(diff.del[0]))
	}
	if formatLPMKey(diff.upsert[0]) != "172.16.0.0/12" {
		t.Fatalf("inserted %s, want 172.16.0.0/12", formatLPMKey(diff.upsert[0]))
	}
}

// ---------------------------------------------------------------------------
// Invariants preserved from before the port
// ---------------------------------------------------------------------------

// TestDefaultDenyRangesSurviveUpdate: the private and link-local ranges are an
// invariant, so an update that names no deny_out rules must still keep them.
func TestDefaultDenyRangesSurviveUpdate(t *testing.T) {
	const ifindex = uint32(770)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	inner := mustInner(t, MapNameDenyOut, ifindex)
	for _, cidr := range alwaysDeniedSandboxCIDRs {
		key, err := parseCIDR(cidr)
		if err != nil {
			t.Fatalf("parseCIDR(%q): %v", cidr, err)
		}
		var value uint32
		if err := inner.Lookup(&key, &value); err != nil {
			t.Fatalf("invariant deny rule %s missing after update: %v", cidr, err)
		}
	}
}

// TestUpdateBumpsPolicyVersion / TestLearnDoesNotBumpPolicyVersion (T27) are
// the two halves of the rule that keeps DNS learning off the flow-recheck path.
func TestUpdateBumpsPolicyVersion(t *testing.T) {
	const ifindex = uint32(771)
	newPolicyManagerTest(t, ifindex)

	before := readPolicyVersion(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	if got := readPolicyVersion(t, ifindex); got != before+1 {
		t.Fatalf("policy_version = %d, want %d", got, before+1)
	}
}

// TestLearnDoesNotBumpPolicyVersion: a bump forces every established flow on
// the node to re-evaluate. At DNS rates that would be continuous, and learning
// a new address cannot change the verdict of any flow that already exists.
func TestLearnDoesNotBumpPolicyVersion(t *testing.T) {
	const ifindex = uint32(772)
	newPolicyManagerTest(t, ifindex)

	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))
	before := readPolicyVersion(t, ifindex)

	learn(t, ifindex, "qq.com", "1.2.3.4")
	learn(t, ifindex, "qq.com", "5.6.7.8")

	now, err := currentNS()
	if err != nil {
		t.Fatalf("currentNS: %v", err)
	}
	ageAllManagers(now)

	if got := readPolicyVersion(t, ifindex); got != before {
		t.Fatalf("policy_version moved from %d to %d on the learning path", before, got)
	}
}

func readPolicyVersion(t *testing.T, ifindex uint32) uint32 {
	t.Helper()
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		t.Fatalf("load metadata map: %v", err)
	}
	defer m.Close()
	var meta mvmMetadata
	if err := m.Lookup(&ifindex, &meta); err != nil {
		t.Fatalf("lookup metadata: %v", err)
	}
	return meta.PolicyVersion
}
