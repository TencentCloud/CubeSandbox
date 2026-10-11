// Per-sandbox network policy manager.
//
// NetPolicyManager is the single writer to allow_out_v3[ifindex],
// deny_out[ifindex] and dns_allow_v2[ifindex] inner maps. Control-plane
// updates go through applyUpdate() (one mutex, one persist). DNS learning and
// TTL aging go through apply(), which rematches learned entries against the
// current domain rules, merges them with the static rules, diffs against the
// mirror, and persists.
//
// The two entry points are deliberately separate, mirroring the production
// layout: apply() has no way to swap the static plan, so DNS learning can
// never rewrite control-plane state, and only the control-plane callers in
// netpolicy.go touch mvm_meta (dns_policy_flags / policy_version). A DNS
// response arrives up to ~100 times a second per sandbox; bumping
// policy_version there would force every established flow on the node to
// re-evaluate continuously.
//
// State is persisted as JSON in /dev/shm/cubevs/netpolicy/<ifindex>.json. On
// process restart load() restores the snapshot and reconcile() diffs the
// desired state against the live BPF maps, so in-flight DNS learning survives
// Cubelet restarts. Node reboots wipe /dev/shm — the control plane re-pushes
// static rules and DNS learns again from scratch. Expiry timestamps are
// CLOCK_MONOTONIC, whose epoch is also per-boot, so the two lifetimes agree.

package cubevs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"

	"github.com/cilium/ebpf"
)

// nsecPerSec converts a DNS TTL in seconds to the CLOCK_MONOTONIC nanoseconds
// the datapath compares expiry against.
const nsecPerSec = 1_000_000_000

// Policy snapshot schema version. Bumped only on breaking layout changes
// (semantic reversal, required field removal). Additive changes — new keys,
// new flag values — do not bump this.
const policySnapshotSchemaV1 uint32 = 1

var errNetPolicyManagerRemoved = errors.New("net policy manager was removed")

// policySnapshotDir is the parent directory holding per-sandbox JSON
// snapshots. Overridable in tests via SetPolicySnapshotDir.
var (
	policySnapshotDirMu sync.Mutex
	policySnapshotDir   = "/dev/shm/cubevs/netpolicy"
)

// SetPolicySnapshotDir overrides the location of per-sandbox JSON snapshots.
// Intended for tests; production code should use the default.
func SetPolicySnapshotDir(dir string) {
	policySnapshotDirMu.Lock()
	defer policySnapshotDirMu.Unlock()
	policySnapshotDir = dir
}

func currentPolicySnapshotDir() string {
	policySnapshotDirMu.Lock()
	defer policySnapshotDirMu.Unlock()
	return policySnapshotDir
}

func policySnapshotPath(ifindex uint32) string {
	return filepath.Join(currentPolicySnapshotDir(), fmt.Sprintf("%d.json", ifindex))
}

// ---------------------------------------------------------------------------
// JSON schema
// ---------------------------------------------------------------------------

// L7Port is one (port, scheme) tuple, as carried by both a static allow rule
// and a domain rule. It is the JSON twin of the datapath's l7PortEntry: the
// port is host order here and network order there, and portsToL7Entries /
// l7EntriesToPorts convert at the map boundary.
type L7Port struct {
	Port   uint16 `json:"port"`
	Scheme uint8  `json:"scheme"`
}

// AllowOutRule is a JSON-serialisable static allow_out_v3 rule: one IP/CIDR
// plus the L7 flags and (port, scheme) set the control plane attached to it.
type AllowOutRule struct {
	CIDR  string   `json:"cidr"`
	Flags uint8    `json:"flags,omitempty"`
	Ports []L7Port `json:"ports,omitempty"`
}

// DenyOutRule is a JSON-serialisable deny_out rule. deny_out carries no value
// beyond "present", so the CIDR is the whole rule.
type DenyOutRule struct {
	CIDR string `json:"cidr"`
}

// DomainRule is the persisted form of a dns_allow_v2 rule, used to rebuild the
// in-process learner index after a Cubelet restart. The BPF map survives a
// restart, but the index lives in memory; persisting the rules lets DNS
// learning resume without waiting for a fresh control-plane push.
type DomainRule struct {
	Domain string   `json:"domain"`
	Flags  uint8    `json:"flags,omitempty"`
	Ports  []L7Port `json:"ports,omitempty"`
}

// DNSEntry captures per-domain DNS-learned state: the IPv4 addresses resolved
// for one QName and when each expires.
//
// This is the multi-owner table that makes domain-scoped revocation possible.
// An allow_out_v3 row is keyed only by (ip) or (ip, port) and carries no
// provenance, so nothing in the datapath can say which domain taught it. Here
// one IP may appear under several QNames, and it leaves the desired set only
// when the last QName holding it is gone.
type DNSEntry struct {
	UpdatedAtNs uint64              `json:"updated_at_ns"`
	IPs         map[string]DNSIPTTL `json:"ips"`
}

// DNSIPTTL is a per-IP expiry record inside a DNSEntry.
type DNSIPTTL struct {
	ExpiresAtNs uint64 `json:"expires_at_ns"`
}

// allowMirrorEntry mirrors one row of allow_out_v3[ifindex]'s inner LPM trie.
// Read back from the BPF map on load for reconciliation, and used as the base
// for the next diff. One of three co-equal mirrors, alongside denyMirrorEntry
// and dnsMirrorEntry.
//
// Unlike production's net_policy mirror (a CIDR plus a verdict), an
// allow_out_v3 row's identity includes the destination port, and its value
// carries the L7 scheme and the DNS-learned expiry. ExpiresAtNs == 0 marks a
// static row.
type allowMirrorEntry struct {
	IP          string `json:"ip"`
	Prefixlen   uint32 `json:"prefixlen"`
	Port        uint16 `json:"port,omitempty"`
	Flags       uint8  `json:"flags,omitempty"`
	Scheme      uint8  `json:"scheme,omitempty"`
	ExpiresAtNs uint64 `json:"expires_at_ns,omitempty"`
}

// denyMirrorEntry mirrors one row of deny_out[ifindex].
type denyMirrorEntry struct {
	CIDR string `json:"cidr"`
}

// dnsMirrorEntry mirrors one row of dns_allow_v2[ifindex]. Key is the raw
// binary dnsAllowKey so the JSON form base64-encodes losslessly; Domain is the
// human-readable pattern for tooling.
type dnsMirrorEntry struct {
	Key    []byte   `json:"key"`
	Domain string   `json:"domain,omitempty"`
	Flags  uint8    `json:"flags,omitempty"`
	Ports  []L7Port `json:"ports,omitempty"`
}

// PolicySnapshot is the JSON-serialized per-sandbox policy state.
//
// Two groups: the inputs the desired state is computed from (AllowOut /
// DenyOut / DomainRules / DynamicLearned), and the mirrors of what was last
// written to each BPF map (the diff's "prev").
//
// Unknown top-level fields are preserved via Extra so a rollback to an older
// binary and a later roll-forward do not silently drop fields a newer peer
// wrote.
type PolicySnapshot struct {
	SchemaVersion uint32 `json:"schema_version"`
	Ifindex       uint32 `json:"ifindex"`
	Generation    uint64 `json:"generation"`
	UpdatedAtNs   uint64 `json:"updated_at_ns"`

	AllowOut       []AllowOutRule       `json:"allow_out,omitempty"`
	DenyOut        []DenyOutRule        `json:"deny_out,omitempty"`
	DomainRules    []DomainRule         `json:"domain_rules,omitempty"`
	DynamicLearned map[string]*DNSEntry `json:"dynamic_learned,omitempty"`

	AllowMirror []allowMirrorEntry `json:"allow_mirror,omitempty"`
	DenyMirror  []denyMirrorEntry  `json:"deny_mirror,omitempty"`
	DNSMirror   []dnsMirrorEntry   `json:"dns_mirror,omitempty"`

	// Extra holds every unrecognized top-level key, re-emitted verbatim.
	Extra map[string]json.RawMessage `json:"-"`
}

func newEmptySnapshot(ifindex uint32) *PolicySnapshot {
	return &PolicySnapshot{
		SchemaVersion:  policySnapshotSchemaV1,
		Ifindex:        ifindex,
		DynamicLearned: map[string]*DNSEntry{},
	}
}

// ---------------------------------------------------------------------------
// Manager and registry
// ---------------------------------------------------------------------------

// NetPolicyManager owns one PolicySnapshot per TAP device. The mutex
// serializes every writer (control-plane updates, DNS learning, aging) so the
// per-ifindex inner maps only ever see one goroutine issuing map updates.
type NetPolicyManager struct {
	ifindex uint32
	path    string

	mu      sync.Mutex
	snap    *PolicySnapshot
	removed bool
	// domainRules is the query index LearnDNS and rematchDynamic consult to
	// decide the current flags / port set for a QName. Rebuilt on each
	// applyUpdate from the plan, and from snap.DomainRules after a restart.
	domainRules *domainRuleSet
}

// managerRegistry indexes NetPolicyManagers by ifindex. GetNetPolicyManager is
// the only entry point; beginRemoveNetPolicyManager drops the entry when a TAP
// is torn down. managerInit serializes both first-use load+reconcile and
// teardown per ifindex: while an entry exists, concurrent Get callers wait so
// they cannot observe a half-initialized manager or create a replacement
// before teardown has flushed the old BPF state.
var (
	managerRegistryMu sync.Mutex
	managerRegistry   = map[uint32]*NetPolicyManager{}
	managerInit       = map[uint32]chan struct{}{}
)

// GetNetPolicyManager returns (creating if needed) the manager for ifindex.
// First use loads the existing snapshot from disk, if any, then reconciles the
// desired state against the live BPF maps. The manager is inserted into the
// registry only after that work finishes, so a concurrent poller LearnDNS
// cannot persist an empty snapshot over a good on-disk file.
func GetNetPolicyManager(ifindex uint32) (*NetPolicyManager, error) {
	for {
		managerRegistryMu.Lock()
		if m, ok := managerRegistry[ifindex]; ok {
			managerRegistryMu.Unlock()
			return m, nil
		}
		if wait, loading := managerInit[ifindex]; loading {
			managerRegistryMu.Unlock()
			<-wait
			continue
		}
		wait := make(chan struct{})
		managerInit[ifindex] = wait
		managerRegistryMu.Unlock()

		m := &NetPolicyManager{
			ifindex: ifindex,
			path:    policySnapshotPath(ifindex),
		}
		if err := m.load(); err != nil {
			// A missing or corrupt snapshot just means we start clean. Note
			// that starting clean must NOT wipe the live maps — reconcile()
			// adopts them as the mirror instead (see its "no state" branch).
			m.mu.Lock()
			m.snap = newEmptySnapshot(ifindex)
			m.mu.Unlock()
		}
		// Best-effort: unit tests without pinned maps skip inside reconcile.
		_ = m.reconcile()

		managerRegistryMu.Lock()
		managerRegistry[ifindex] = m
		delete(managerInit, ifindex)
		close(wait)
		managerRegistryMu.Unlock()
		return m, nil
	}
}

// beginRemoveNetPolicyManager prevents a replacement manager from being
// created, waits for any in-flight writer, tombstones the old manager, and
// removes its snapshot file. The returned function must be called after the
// caller has finished flushing BPF state.
//
// The tombstone must land before the caller deletes the HashOfMaps outer keys:
// policyInnerCache has no per-key lock, so a concurrent acquireInnerMap could
// otherwise re-cache an inner FD between the cache eviction and the outer
// delete, leaving a stale cached FD that a later ifindex reuse would write
// into — a map the datapath no longer consults.
func beginRemoveNetPolicyManager(ifindex uint32) func() {
	for {
		managerRegistryMu.Lock()
		if wait, busy := managerInit[ifindex]; busy {
			managerRegistryMu.Unlock()
			<-wait
			continue
		}

		done := make(chan struct{})
		managerInit[ifindex] = done
		m := managerRegistry[ifindex]
		delete(managerRegistry, ifindex)
		managerRegistryMu.Unlock()

		path := policySnapshotPath(ifindex)
		if m != nil {
			m.mu.Lock()
			m.removed = true
			path = m.path
			m.path = ""
			m.mu.Unlock()
		}
		// A caller that never Got a manager for this ifindex may still have a
		// snapshot left by a crashed process.
		_ = os.Remove(path)

		return func() {
			managerRegistryMu.Lock()
			if managerInit[ifindex] == done {
				delete(managerInit, ifindex)
				close(done)
			}
			managerRegistryMu.Unlock()
		}
	}
}

// liveManagers snapshots the registry so callers can walk it without holding
// the registry lock across per-manager work.
func liveManagers() []*NetPolicyManager {
	managerRegistryMu.Lock()
	defer managerRegistryMu.Unlock()
	out := make([]*NetPolicyManager, 0, len(managerRegistry))
	for _, m := range managerRegistry {
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

// LearnDNS ingests a parsed DNS response and, if the QName still matches a
// domain rule, upserts the answers into DynamicLearned so their rows appear in
// allow_out_v3 at the next apply. Called from the dns_events poller; safe to
// call concurrently across ifindices (each manager has its own mutex).
//
// Returns nil for a benign no-op (no matching rule or empty answer set). The
// domain lookup is repeated inside apply's mutex so a concurrent applyUpdate
// that revoked the rule cannot be raced: either the learn happens first and
// the update's diff removes its rows, or the update happens first and the
// rule is already gone. Both orders are correct, which is why no datapath
// generation latch is needed to close the in-flight window.
func (m *NetPolicyManager) LearnDNS(qname string, answers []DNSAnswer) error {
	if len(answers) == 0 {
		return nil
	}
	// Cheap pre-check outside apply, so an unmatched QName costs no diff and
	// no persist. The mutate closure re-looks up under m.mu.
	if m.lookupDomainRule(qname) == nil {
		return nil
	}
	nowNs, err := currentNS()
	if err != nil {
		return err
	}
	name := normalizeQName(qname)
	return m.apply(func(s *PolicySnapshot) error {
		if m.domainRules.lookup(name) == nil {
			return nil
		}
		if s.DynamicLearned == nil {
			s.DynamicLearned = map[string]*DNSEntry{}
		}
		entry := s.DynamicLearned[name]
		if entry == nil {
			entry = &DNSEntry{IPs: map[string]DNSIPTTL{}}
			s.DynamicLearned[name] = entry
		}
		entry.UpdatedAtNs = nowNs

		for ip, ttl := range entry.IPs {
			if ttl.ExpiresAtNs <= nowNs {
				delete(entry.IPs, ip)
			}
		}
		for _, ans := range answers {
			ip := ans.IP.To4()
			if ip == nil {
				continue
			}
			expires := nowNs + uint64(ans.TTLSeconds)*uint64(nsecPerSec)
			key := ip.String()
			if cur, ok := entry.IPs[key]; ok && cur.ExpiresAtNs > expires {
				// Never shorten a lease: another answer already granted a
				// longer TTL for this address.
				continue
			}
			entry.IPs[key] = DNSIPTTL{ExpiresAtNs: expires}
		}
		return nil
	})
}

// lookupDomainRule returns the most-specific domain rule matching qname, or
// nil. Internal helper used for LearnDNS's pre-check; the returned pointer
// must not be mutated.
func (m *NetPolicyManager) lookupDomainRule(qname string) *domainRule {
	m.mu.Lock()
	rs := m.domainRules
	m.mu.Unlock()
	return rs.lookup(qname)
}

// apply is the background write path: DNS learning and TTL aging.
//
// It runs mutate under m.mu, rematches DynamicLearned against the current
// domain rules, recomputes the desired state, diffs it against the mirrors and
// issues the required map writes. It cannot swap the static plan and it never
// touches mvm_meta, so it can neither rewrite control-plane state nor force
// established flows to re-evaluate.
//
// On any map-write failure the mirrors are left at their pre-diff values so a
// retry replays the same operations; the diff is idempotent.
func (m *NetPolicyManager) apply(mutate func(*PolicySnapshot) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// DNS events and aging work may retain a manager reference after teardown
	// detached it. Treat that asynchronous work as a benign no-op so it cannot
	// recreate the snapshot or write ghost BPF rows.
	if m.removed {
		return nil
	}
	if m.snap == nil {
		m.snap = newEmptySnapshot(m.ifindex)
	}
	if err := mutate(m.snap); err != nil {
		return err
	}
	return m.convergeLocked()
}

// applyUpdate is the control-plane write path: create, replace and update all
// reduce to "install this plan". It swaps the static rules and the domain
// index, then runs the same convergence as apply().
//
// Unlike apply(), a teardown tombstone is reported as an error: the caller is
// pushing policy at a TAP that no longer exists and needs to know.
func (m *NetPolicyManager) applyUpdate(plan *netPolicyPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.removed {
		return fmt.Errorf("%w: ifindex %d", errNetPolicyManagerRemoved, m.ifindex)
	}
	if m.snap == nil {
		m.snap = newEmptySnapshot(m.ifindex)
	}

	m.snap.AllowOut = allowOutRulesFromPlan(plan.allowOutEntries)
	m.snap.DenyOut = denyOutRulesFromPlan(effectiveDenyOutEntriesForReplace(plan))
	m.snap.DomainRules = domainRulesFromPlan(plan.dnsAllowRules)
	m.domainRules = buildDomainRuleSetFromPersisted(m.snap.DomainRules)

	return m.convergeLocked()
}

// convergeLocked is the shared tail of apply and applyUpdate: rematch, compute
// desired, diff, write, persist. Caller must hold m.mu.
func (m *NetPolicyManager) convergeLocked() error {
	if m.domainRules == nil {
		m.domainRules = buildDomainRuleSetFromPersisted(m.snap.DomainRules)
	}
	nowNs, err := currentNS()
	if err != nil {
		return err
	}
	rematchDynamic(m.snap, m.domainRules, nowNs)

	desired := computeEffective(m.snap, m.domainRules, nowNs)

	allowDiff := diffAllowOut(m.snap.AllowMirror, desired.allowOut)
	denyDiff := diffDenyOut(m.snap.DenyMirror, desired.denyOut)
	dnsDiff := diffDNSAllow(m.snap.DNSMirror, desired.dnsAllow)
	if err := m.applyDiffs(allowDiff, denyDiff, dnsDiff); err != nil {
		return err
	}

	m.snap.AllowMirror = desired.allowOut
	m.snap.DenyMirror = desired.denyOut
	m.snap.DNSMirror = desired.dnsAllow
	m.snap.Generation++
	m.snap.UpdatedAtNs = nowNs
	return m.persist()
}

// ---------------------------------------------------------------------------
// Desired-state computation
// ---------------------------------------------------------------------------

// desiredState is the full set of rows every managed map should hold.
type desiredState struct {
	allowOut []allowMirrorEntry
	denyOut  []denyMirrorEntry
	dnsAllow []dnsMirrorEntry
}

// rematchDynamic rewrites DynamicLearned against the current domain-rule
// index. Called under m.mu before computeEffective:
//
//   - expired IPs are dropped; QNames left with no IPs are removed
//   - QNames that no longer match any domain rule are removed outright — this
//     is what makes revoking a domain rule retire the IPs it taught, without
//     ever deleting an IP another live QName still holds
func rematchDynamic(s *PolicySnapshot, rs *domainRuleSet, nowNs uint64) {
	if s == nil || s.DynamicLearned == nil {
		return
	}
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
		if len(entry.IPs) == 0 || rs.lookup(qname) == nil {
			delete(s.DynamicLearned, qname)
		}
	}
}

// computeEffective merges the static rules with the DNS-learned entries into
// the exact set of rows each BPF map should hold.
//
// allow_out_v3 is the only map with two sources. The merge rules on a shared
// key are:
//
//   - a static row wins over a learned one: the entry keeps the static zero
//     expiry and becomes permanent, so the reaper cannot age out a row that
//     carries a control-plane verdict
//   - two learned rows keep the later expiry and the union of their flags
//   - a scheme conflict (two domains with different schemes on the same port,
//     resolving to the same IP) is resolved in favour of the lexicographically
//     first QName. buildL7Plan only detects scheme conflicts within one host,
//     so this case is reachable; picking deterministically keeps repeated
//     applies from rewriting the row and flip-flopping the scheme.
func computeEffective(s *PolicySnapshot, rs *domainRuleSet, nowNs uint64) desiredState {
	rows := map[lpmKeyV3]netPolicyValueV3{}

	for _, r := range s.AllowOut {
		entry, err := r.policyEntry()
		if err != nil {
			continue
		}
		for _, row := range expandAllowOutEntry(entry) {
			mergeAllowOutRow(rows, row.key, row.val)
		}
	}

	// Iterate QNames in sorted order so a scheme conflict on a shared
	// (ip, port) key resolves the same way on every apply.
	for _, qname := range sortedQNames(s.DynamicLearned) {
		rule := rs.lookup(qname)
		if rule == nil {
			continue
		}
		entry := s.DynamicLearned[qname]
		for _, ip := range sortedIPs(entry.IPs) {
			ttl := entry.IPs[ip]
			if ttl.ExpiresAtNs <= nowNs {
				continue
			}
			for _, row := range learnedAllowOutRows(ip, rule, ttl.ExpiresAtNs) {
				mergeAllowOutRow(rows, row.key, row.val)
			}
		}
	}

	out := desiredState{allowOut: make([]allowMirrorEntry, 0, len(rows))}
	for key, val := range rows {
		out.allowOut = append(out.allowOut, allowMirrorEntryFromRow(key, val))
	}
	sortAllowMirror(out.allowOut)

	out.denyOut = make([]denyMirrorEntry, 0, len(s.DenyOut))
	for _, r := range s.DenyOut {
		if _, err := parseCIDR(r.CIDR); err != nil {
			continue
		}
		out.denyOut = append(out.denyOut, denyMirrorEntry{CIDR: r.CIDR})
	}
	sortDenyMirror(out.denyOut)

	out.dnsAllow = make([]dnsMirrorEntry, 0, len(s.DomainRules))
	for _, r := range s.DomainRules {
		key, value, err := makeDNSAllowRule(r.Domain, r.Flags)
		if err != nil {
			continue
		}
		applyPortsToDNSAllowValue(&value, portsToL7Entries(r.Ports))
		out.dnsAllow = append(out.dnsAllow, dnsMirrorEntry{
			Key:    dnsAllowKeyBytes(key),
			Domain: r.Domain,
			Flags:  value.Flags,
			Ports:  r.Ports,
		})
	}
	sortDNSMirror(out.dnsAllow)

	return out
}

// mergeAllowOutRow folds one row into the desired set under the merge rules
// documented on computeEffective.
func mergeAllowOutRow(rows map[lpmKeyV3]netPolicyValueV3, key lpmKeyV3, val netPolicyValueV3) {
	old, ok := rows[key]
	if !ok {
		rows[key] = val
		return
	}
	merged := old
	merged.Flags |= val.Flags
	switch {
	case old.ExpiresAtNS == 0 || val.ExpiresAtNS == 0:
		merged.ExpiresAtNS = 0 // static wins: the row becomes permanent
	case val.ExpiresAtNS > old.ExpiresAtNS:
		merged.ExpiresAtNS = val.ExpiresAtNS
	}
	// Scheme: first writer wins. Callers feed static rows first and then
	// learned rows in sorted-QName order, so "first" is deterministic.
	if merged.Scheme == L7SchemeNone {
		merged.Scheme = val.Scheme
	}
	rows[key] = merged
}

// learnedAllowOutRows returns the allow_out_v3 rows one (ip, rule) pair
// occupies.
//
// It delegates to expandAllowOutEntry — the same helper the static path uses —
// so a learned row and a static row for the same host can never expand to
// different key sets. Only the expiry differs: learned rows carry the DNS TTL,
// static rows carry zero.
func learnedAllowOutRows(ip string, rule *domainRule, expires uint64) []allowOutRow {
	addr := parseIPv4(ip)
	if addr == nil {
		return nil
	}
	rows := expandAllowOutEntry(allowOutPolicyEntry{
		key:   lpmKey{Prefixlen: 32, IP: ipToUint32(addr)},
		flags: rule.flags,
		ports: rule.ports,
	})
	for i := range rows {
		rows[i].val.ExpiresAtNS = expires
	}
	return rows
}

// policyEntry converts a persisted static allow rule back into the plan shape
// expandAllowOutEntry consumes.
func (r AllowOutRule) policyEntry() (allowOutPolicyEntry, error) {
	key, err := parseCIDR(r.CIDR)
	if err != nil {
		return allowOutPolicyEntry{}, err
	}
	return allowOutPolicyEntry{
		key:    key,
		flags:  r.Flags,
		ports:  portsToL7Entries(r.Ports),
		source: r.CIDR,
	}, nil
}

func allowOutRulesFromPlan(entries []allowOutPolicyEntry) []AllowOutRule {
	out := make([]AllowOutRule, 0, len(entries))
	for _, e := range entries {
		out = append(out, AllowOutRule{
			CIDR:  formatLPMKey(e.key),
			Flags: e.flags,
			Ports: l7EntriesToPorts(e.ports),
		})
	}
	return out
}

func denyOutRulesFromPlan(entries []denyOutPolicyEntry) []DenyOutRule {
	out := make([]DenyOutRule, 0, len(entries))
	for _, e := range entries {
		out = append(out, DenyOutRule{CIDR: formatLPMKey(e.key)})
	}
	return out
}

// ---------------------------------------------------------------------------
// Diffs
// ---------------------------------------------------------------------------

// allowOutDiff is the minimal change set for allow_out_v3.
type allowOutDiff struct {
	del    []lpmKeyV3
	upsert []allowOutRow
}

type denyOutDiff struct {
	del    []lpmKey
	upsert []lpmKey
}

type dnsAllowDiff struct {
	del    []dnsAllowKey
	upsert []dnsAllowRule
}

func (d allowOutDiff) empty() bool { return len(d.del) == 0 && len(d.upsert) == 0 }
func (d denyOutDiff) empty() bool  { return len(d.del) == 0 && len(d.upsert) == 0 }
func (d dnsAllowDiff) empty() bool { return len(d.del) == 0 && len(d.upsert) == 0 }

// diffAllowOut computes the rows to delete and (re)write to bring allow_out_v3
// from prev to next. Deletions are sorted by prefixlen descending and
// insertions likewise, so the operation sequence is reproducible and the more
// specific rows land first.
func diffAllowOut(prev, next []allowMirrorEntry) allowOutDiff {
	prevRows := map[lpmKeyV3]netPolicyValueV3{}
	for _, e := range prev {
		key, val, err := e.row()
		if err != nil {
			continue
		}
		prevRows[key] = val
	}
	nextRows := map[lpmKeyV3]netPolicyValueV3{}
	for _, e := range next {
		key, val, err := e.row()
		if err != nil {
			continue
		}
		nextRows[key] = val
	}

	var d allowOutDiff
	for key := range prevRows {
		if _, keep := nextRows[key]; !keep {
			d.del = append(d.del, key)
		}
	}
	for key, val := range nextRows {
		if old, ok := prevRows[key]; ok && old == val {
			continue
		}
		d.upsert = append(d.upsert, allowOutRow{key: key, val: val})
	}
	sort.Slice(d.del, func(i, j int) bool { return lessLPMKeyV3(d.del[i], d.del[j]) })
	sort.Slice(d.upsert, func(i, j int) bool { return lessLPMKeyV3(d.upsert[i].key, d.upsert[j].key) })
	return d
}

func diffDenyOut(prev, next []denyMirrorEntry) denyOutDiff {
	prevKeys := map[lpmKey]struct{}{}
	for _, e := range prev {
		if key, err := parseCIDR(e.CIDR); err == nil {
			prevKeys[key] = struct{}{}
		}
	}
	nextKeys := map[lpmKey]struct{}{}
	for _, e := range next {
		if key, err := parseCIDR(e.CIDR); err == nil {
			nextKeys[key] = struct{}{}
		}
	}

	var d denyOutDiff
	for key := range prevKeys {
		if _, keep := nextKeys[key]; !keep {
			d.del = append(d.del, key)
		}
	}
	for key := range nextKeys {
		if _, exists := prevKeys[key]; !exists {
			d.upsert = append(d.upsert, key)
		}
	}
	sort.Slice(d.del, func(i, j int) bool { return lessLPMKey(d.del[i], d.del[j]) })
	sort.Slice(d.upsert, func(i, j int) bool { return lessLPMKey(d.upsert[i], d.upsert[j]) })
	return d
}

func diffDNSAllow(prev, next []dnsMirrorEntry) dnsAllowDiff {
	prevKeys := map[string]dnsMirrorEntry{}
	for _, e := range prev {
		prevKeys[string(e.Key)] = e
	}
	nextKeys := map[string]dnsMirrorEntry{}
	for _, e := range next {
		nextKeys[string(e.Key)] = e
	}

	var d dnsAllowDiff
	for raw, e := range prevKeys {
		if _, keep := nextKeys[raw]; !keep {
			d.del = append(d.del, dnsAllowKeyFromBytes(e.Key))
		}
	}
	for raw, e := range nextKeys {
		if old, ok := prevKeys[raw]; ok && sameDNSMirror(old, e) {
			continue
		}
		key, value, err := makeDNSAllowRule(e.Domain, e.Flags)
		if err != nil {
			continue
		}
		applyPortsToDNSAllowValue(&value, portsToL7Entries(e.Ports))
		d.upsert = append(d.upsert, dnsAllowRule{key: key, value: value, domain: e.Domain})
	}
	sort.Slice(d.del, func(i, j int) bool { return lessDNSAllowKey(d.del[i], d.del[j]) })
	sort.Slice(d.upsert, func(i, j int) bool { return lessDNSAllowKey(d.upsert[i].key, d.upsert[j].key) })
	return d
}

// applyDiffs issues the map operations in the "close-doors-first" order: no
// intermediate state is more permissive than both the old and the new policy.
//
// classify_egress_flow evaluates allow_out_v3 first, then deny_out, then falls
// back to default-allow. Production expresses the same invariant as four
// phases inside a single verdict-carrying table; here allow and deny live in
// separate maps, so the same ordering becomes a cross-table sequence:
//
//  1. dns_allow_v2 — stop matching revoked domains before anything else, so
//     no new query is tracked under a rule that is going away
//  2. allow_out_v3 deletions — strictly narrowing
//  3. deny_out insertions — strictly narrowing
//  4. deny_out deletions — widening, so only after the desired denies exist
//  5. allow_out_v3 insertions — the only step that opens access, so it is last
func (m *NetPolicyManager) applyDiffs(a allowOutDiff, d denyOutDiff, n dnsAllowDiff) error {
	if a.empty() && d.empty() && n.empty() {
		return nil
	}

	if !n.empty() {
		inner, err := m.innerMap(MapNameDNSAllowV2)
		if err != nil {
			return err
		}
		for i := range n.del {
			if err := inner.Delete(&n.del[i]); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				return fmt.Errorf("%s delete failed: %w", MapNameDNSAllowV2, err)
			}
		}
		for i := range n.upsert {
			r := &n.upsert[i]
			if err := inner.Update(&r.key, &r.value, ebpf.UpdateAny); err != nil {
				return fmt.Errorf("%s update failed: %w, domain: %s", MapNameDNSAllowV2, err, r.domain)
			}
		}
	}

	var allowInner *ebpf.Map
	if !a.empty() {
		var err error
		allowInner, err = m.innerMap(MapNameAllowOutV3)
		if err != nil {
			return err
		}
		for i := range a.del {
			if err := allowInner.Delete(&a.del[i]); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				return fmt.Errorf("%s delete failed: %w", MapNameAllowOutV3, err)
			}
		}
	}

	if !d.empty() {
		inner, err := m.innerMap(MapNameDenyOut)
		if err != nil {
			return err
		}
		val := uint32(netPolicyValueStatic)
		for i := range d.upsert {
			if err := inner.Update(&d.upsert[i], &val, ebpf.UpdateAny); err != nil {
				return fmt.Errorf("%s update failed: %w", MapNameDenyOut, err)
			}
		}
		for i := range d.del {
			if err := inner.Delete(&d.del[i]); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				return fmt.Errorf("%s delete failed: %w", MapNameDenyOut, err)
			}
		}
	}

	for i := range a.upsert {
		r := &a.upsert[i]
		if err := allowInner.Update(&r.key, &r.val, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("%s update failed: %w", MapNameAllowOutV3, err)
		}
	}
	return nil
}

// innerMap returns the cached inner map for this manager's ifindex.
//
// The factory is always nil: a missing inner means the TAP is being torn down,
// and recreating it here would resurrect policy state for a dead sandbox (and
// re-populate the HashOfMaps outer key behind the teardown's back).
func (m *NetPolicyManager) innerMap(name string) (*ebpf.Map, error) {
	outer, err := loadPinnedMap(name)
	if err != nil {
		return nil, err
	}
	defer outer.Close()
	return acquireInnerMap(outer, m.ifindex, name, nil)
}

// ---------------------------------------------------------------------------
// Load / reconcile / persist
// ---------------------------------------------------------------------------

// load reads the on-disk snapshot. A missing or malformed file is an error so
// the caller starts from an empty snapshot; it is never fatal.
func (m *NetPolicyManager) load() error {
	data, err := os.ReadFile(m.path) //nolint:gosec // path is built from an ifindex
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	snap, err := unmarshalSnapshot(data)
	if err != nil {
		return err
	}
	if snap.SchemaVersion != policySnapshotSchemaV1 {
		return fmt.Errorf("unsupported snapshot schema %d", snap.SchemaVersion) //nolint:err113
	}
	if snap.DynamicLearned == nil {
		snap.DynamicLearned = map[string]*DNSEntry{}
	}
	snap.Ifindex = m.ifindex

	m.mu.Lock()
	m.snap = snap
	m.domainRules = buildDomainRuleSetFromPersisted(snap.DomainRules)
	m.mu.Unlock()
	return nil
}

// reconcile diffs the snapshot's desired state against the live BPF maps and
// applies the delta. Called from GetNetPolicyManager after load(). Missing
// pinned maps (unit tests) are a no-op.
//
// An empty snapshot adopts the live maps as the mirror *without wiping them*.
// That branch is load-bearing: a lost or corrupt snapshot file would otherwise
// present "desired is empty" to the diff and delete every row the sandbox has,
// static rules included.
func (m *NetPolicyManager) reconcile() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snap == nil {
		m.snap = newEmptySnapshot(m.ifindex)
	}

	liveAllow, allowErr := dumpAllowOutMirror(m.ifindex)
	liveDeny, denyErr := dumpDenyOutMirror(m.ifindex)
	liveDNS, dnsErr := dumpDNSAllowMirror(m.ifindex)
	if allowErr != nil && denyErr != nil && dnsErr != nil {
		return nil
	}

	if allowErr == nil {
		m.snap.AllowMirror = liveAllow
	}
	if denyErr == nil {
		m.snap.DenyMirror = liveDeny
	}
	if dnsErr == nil {
		m.snap.DNSMirror = liveDNS
	}

	hasState := len(m.snap.AllowOut) > 0 || len(m.snap.DenyOut) > 0 ||
		len(m.snap.DomainRules) > 0 || len(m.snap.DynamicLearned) > 0
	if !hasState {
		return nil
	}
	return m.convergeLocked()
}

// persist writes the snapshot atomically: a temporary file in the same
// directory, fsync-free (tmpfs), then rename over the target.
func (m *NetPolicyManager) persist() error {
	if m.path == "" {
		return nil
	}
	data, err := marshalSnapshot(m.snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod snapshot temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write snapshot temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close snapshot temp: %w", err)
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return nil
}

// unmarshalSnapshot decodes a snapshot, retaining unrecognized top-level keys
// in Extra so they survive a round trip through an older binary.
func unmarshalSnapshot(data []byte) (*PolicySnapshot, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	snap := &PolicySnapshot{}
	if err := json.Unmarshal(data, snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	known := knownSnapshotKeys()
	for key, val := range raw {
		if _, ok := known[key]; ok {
			continue
		}
		if snap.Extra == nil {
			snap.Extra = map[string]json.RawMessage{}
		}
		snap.Extra[key] = val
	}
	return snap, nil
}

func marshalSnapshot(snap *PolicySnapshot) ([]byte, error) {
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	if len(snap.Extra) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	for key, val := range snap.Extra {
		if _, clash := merged[key]; clash {
			continue
		}
		merged[key] = val
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return out, nil
}

func knownSnapshotKeys() map[string]struct{} {
	return map[string]struct{}{
		"schema_version": {}, "ifindex": {}, "generation": {}, "updated_at_ns": {},
		"allow_out": {}, "deny_out": {}, "domain_rules": {},
		"dynamic_learned": {}, "allow_mirror": {},
		"deny_mirror": {}, "dns_mirror": {},
	}
}

// ---------------------------------------------------------------------------
// Live map dumps
// ---------------------------------------------------------------------------

func dumpAllowOutMirror(ifindex uint32) ([]allowMirrorEntry, error) {
	inner, err := lookupPinnedInner(MapNameAllowOutV3, ifindex)
	if err != nil {
		return nil, err
	}
	var (
		key lpmKeyV3
		val netPolicyValueV3
		out []allowMirrorEntry
	)
	iter := inner.Iterate()
	for iter.Next(&key, &val) {
		out = append(out, allowMirrorEntryFromRow(key, val))
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("%s iterate failed: %w", MapNameAllowOutV3, err)
	}
	sortAllowMirror(out)
	return out, nil
}

func dumpDenyOutMirror(ifindex uint32) ([]denyMirrorEntry, error) {
	inner, err := lookupPinnedInner(MapNameDenyOut, ifindex)
	if err != nil {
		return nil, err
	}
	var (
		key lpmKey
		val uint32
		out []denyMirrorEntry
	)
	iter := inner.Iterate()
	for iter.Next(&key, &val) {
		out = append(out, denyMirrorEntry{CIDR: formatLPMKey(key)})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("%s iterate failed: %w", MapNameDenyOut, err)
	}
	sortDenyMirror(out)
	return out, nil
}

func dumpDNSAllowMirror(ifindex uint32) ([]dnsMirrorEntry, error) {
	inner, err := lookupPinnedInner(MapNameDNSAllowV2, ifindex)
	if err != nil {
		return nil, err
	}
	var (
		key dnsAllowKey
		val dnsAllowValue
		out []dnsMirrorEntry
	)
	iter := inner.Iterate()
	for iter.Next(&key, &val) {
		domain, derr := decodeDNSAllowKey(key, val)
		if derr != nil {
			continue
		}
		out = append(out, dnsMirrorEntry{
			Key:    dnsAllowKeyBytes(key),
			Domain: domain,
			Flags:  val.Flags,
			Ports:  l7EntriesToPorts(val.Ports[:val.PortCount]),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("%s iterate failed: %w", MapNameDNSAllowV2, err)
	}
	sortDNSMirror(out)
	return out, nil
}

func lookupPinnedInner(name string, ifindex uint32) (*ebpf.Map, error) {
	outer, err := loadPinnedMap(name)
	if err != nil {
		return nil, err
	}
	defer outer.Close()
	return acquireInnerMap(outer, ifindex, name, nil)
}

// ---------------------------------------------------------------------------
// Row / key conversion helpers
// ---------------------------------------------------------------------------

func allowMirrorEntryFromRow(key lpmKeyV3, val netPolicyValueV3) allowMirrorEntry {
	return allowMirrorEntry{
		IP:          uint32ToIP(key.IP).String(),
		Prefixlen:   key.Prefixlen,
		Port:        ntohsPort(key.Port),
		Flags:       val.Flags,
		Scheme:      val.Scheme,
		ExpiresAtNs: val.ExpiresAtNS,
	}
}

// row rebuilds the (key, value) pair a allowMirrorEntry describes. KeyPrefixlen is
// derived rather than stored: it must equal the key's prefixlen, and that
// equality is what lets same-key merges tell an exact hit from a shorter
// covering entry.
func (e allowMirrorEntry) row() (lpmKeyV3, netPolicyValueV3, error) {
	ip := parseIPv4(e.IP)
	if ip == nil {
		return lpmKeyV3{}, netPolicyValueV3{}, fmt.Errorf("invalid mirror IP: %s", e.IP) //nolint:err113
	}
	key := lpmKeyV3{Prefixlen: e.Prefixlen, IP: ipToUint32(ip), Port: htonsPort(e.Port)}
	val := netPolicyValueV3{
		ExpiresAtNS:  e.ExpiresAtNs,
		Flags:        e.Flags,
		Scheme:       e.Scheme,
		KeyPrefixlen: uint8(e.Prefixlen),
	}
	return key, val, nil
}

func dnsAllowKeyBytes(key dnsAllowKey) []byte {
	out := make([]byte, unsafe.Sizeof(key))
	copy(out, (*(*[unsafe.Sizeof(key)]byte)(unsafe.Pointer(&key)))[:])
	return out
}

func dnsAllowKeyFromBytes(b []byte) dnsAllowKey {
	var key dnsAllowKey
	if len(b) != int(unsafe.Sizeof(key)) {
		return key
	}
	copy((*[unsafe.Sizeof(key)]byte)(unsafe.Pointer(&key))[:], b)
	return key
}

func portsToL7Entries(ports []L7Port) []l7PortEntry {
	if len(ports) == 0 {
		return nil
	}
	out := make([]l7PortEntry, 0, len(ports))
	for _, p := range ports {
		out = append(out, l7PortEntry{Port: htonsPort(p.Port), Scheme: p.Scheme})
	}
	return out
}

func l7EntriesToPorts(ports []l7PortEntry) []L7Port {
	if len(ports) == 0 {
		return nil
	}
	out := make([]L7Port, 0, len(ports))
	for _, p := range ports {
		out = append(out, L7Port{Port: ntohsPort(p.Port), Scheme: p.Scheme})
	}
	return out
}

// formatLPMKey renders an lpmKey as a canonical CIDR string. A zero prefix
// length renders as 0.0.0.0/0 rather than net.IP.String()'s terser form, so
// the JSON round-trips cleanly.
func formatLPMKey(k lpmKey) string {
	return fmt.Sprintf("%s/%d", uint32ToIP(k.IP).String(), k.Prefixlen)
}

func parseIPv4(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	return ip.To4()
}

func sameDNSMirror(a, b dnsMirrorEntry) bool {
	if a.Domain != b.Domain || a.Flags != b.Flags || len(a.Ports) != len(b.Ports) {
		return false
	}
	for i := range a.Ports {
		if a.Ports[i] != b.Ports[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Deterministic ordering
// ---------------------------------------------------------------------------

func sortedQNames(learned map[string]*DNSEntry) []string {
	out := make([]string, 0, len(learned))
	for qname, entry := range learned {
		if entry != nil {
			out = append(out, qname)
		}
	}
	sort.Strings(out)
	return out
}

func sortedIPs(ips map[string]DNSIPTTL) []string {
	out := make([]string, 0, len(ips))
	for ip := range ips {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func lessLPMKeyV3(a, b lpmKeyV3) bool {
	if a.Prefixlen != b.Prefixlen {
		return a.Prefixlen > b.Prefixlen
	}
	if a.IP != b.IP {
		return a.IP < b.IP
	}
	return a.Port < b.Port
}

func lessLPMKey(a, b lpmKey) bool {
	if a.Prefixlen != b.Prefixlen {
		return a.Prefixlen > b.Prefixlen
	}
	return a.IP < b.IP
}

func lessDNSAllowKey(a, b dnsAllowKey) bool {
	if a.Prefixlen != b.Prefixlen {
		return a.Prefixlen > b.Prefixlen
	}
	return string(a.Name[:]) < string(b.Name[:])
}

func sortAllowMirror(entries []allowMirrorEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Prefixlen != b.Prefixlen {
			return a.Prefixlen > b.Prefixlen
		}
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		return a.Port < b.Port
	})
}

func sortDenyMirror(entries []denyMirrorEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].CIDR < entries[j].CIDR })
}

func sortDNSMirror(entries []dnsMirrorEntry) {
	sort.Slice(entries, func(i, j int) bool { return string(entries[i].Key) < string(entries[j].Key) })
}
