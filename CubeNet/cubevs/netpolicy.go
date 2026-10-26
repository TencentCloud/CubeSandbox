package cubevs

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"golang.org/x/sys/unix"
)

const maxNetPolicyEntries = 8192

var alwaysDeniedSandboxCIDRs = []string{
	"10.0.0.0/8",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

var alwaysDeniedSandboxEntries = mustBuildDenyOutPolicyEntries(alwaysDeniedSandboxCIDRs)

type allowOutPolicyEntry struct {
	key   lpmKey
	flags uint8
	// ports is inherited by dns_learn_response_ip into net_policy_value_v3.ports
	// when a domain rule with an explicit (port, scheme) set is learned into
	// allow_out_v3. Empty for legacy port-agnostic rules (the datapath falls
	// back to the default 80/443 set in that case).
	ports  []l7PortEntry
	source string
}

type denyOutPolicyEntry struct {
	key    lpmKey
	source string
}

type netPolicyPlan struct {
	allowOutEntries []allowOutPolicyEntry
	dnsAllowRules   []dnsAllowRule
	denyOutEntries  []denyOutPolicyEntry
	dnsPolicyFlags  uint8
}

// newInnerLPMMap creates a new LPM trie map with uint32 values for deny_out.
func newInnerLPMMap() (*ebpf.Map, error) {
	return newInnerLPMMapWithValueSize(uint32(unsafe.Sizeof(uint32(0))), btfTypeLPMKey, btfTypeU32)
}

func newInnerLPMMapWithValueSize(valueSize uint32, keyType, valueType btf.Type) (*ebpf.Map, error) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(lpmKey{})),
		ValueSize:  valueSize,
		MaxEntries: maxNetPolicyEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
		Key:        keyType,
		Value:      valueType,
	})
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMap(LPMTrie) failed: %w", err)
	}
	return m, nil
}

func newInnerAllowOutMap() (*ebpf.Map, error) {
	return ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(lpmKeyV3{})),
		ValueSize:  uint32(unsafe.Sizeof(netPolicyValueV3{})),
		MaxEntries: maxNetPolicyEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
		Key:        btfTypeLPMKey,
		Value:      btfTypePolicyValue,
	})
}

func ensureAllowOutV3InnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return ensureInnerMapWithFactory(outerMap, ifindex, MapNameAllowOutV3, newInnerAllowOutMap)
}

func ensureDenyOutInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	_, err := acquireInnerMap(outerMap, ifindex, MapNameDenyOut, newInnerLPMMap)
	return err
}

func ensureInnerMapWithFactory(outerMap *ebpf.Map, ifindex uint32, mapName string,
	newInner func() (*ebpf.Map, error),
) error {
	_, err := acquireInnerMap(outerMap, ifindex, mapName, newInner)
	return err
}

// initNetPolicy creates inner LPM trie maps for the given ifindex
// in allow_out_v3, deny_out and dns_allow_v2 hash-of-maps, if not already present.
// This should be called during AttachFilter.
func initNetPolicy(ifindex uint32) error {
	allowOut, err := loadPinnedMap(MapNameAllowOutV3)
	if err != nil {
		return err
	}
	defer allowOut.Close()

	err = ensureAllowOutV3InnerMap(allowOut, ifindex)
	if err != nil {
		return err
	}

	denyOut, err := loadPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}
	defer denyOut.Close()

	err = ensureDenyOutInnerMap(denyOut, ifindex)
	if err != nil {
		return err
	}

	return initDNSAllow(ifindex)
}

// flushInnerMap removes all entries from the inner LPM trie map
// associated with the given ifindex in the outer hash-of-maps.
func flushInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return flushInnerMapWithValue[lpmKey, uint32](outerMap, ifindex, MapNameDenyOut)
}

func flushAllowOutInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return flushInnerMapWithValue[lpmKeyV3, netPolicyValueV3](outerMap, ifindex, MapNameAllowOutV3)
}

func flushInnerMapWithValue[K any, V any](outerMap *ebpf.Map, ifindex uint32, mapName string) error {
	inner, err := acquireInnerMap(outerMap, ifindex, mapName, nil)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return err
	}
	return flushInnerEntries[K, V](inner)
}

func flushInnerEntries[K any, V any](inner *ebpf.Map) error {
	// cilium/ebpf iterators (especially LPM trie) can skip remaining
	// entries if a key is deleted during Iterate. Collect first, then
	// delete: Next overwrites the same key buffer, so append copies K.
	var (
		key   K
		value V
		keys  []K
	)
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		keys = append(keys, key)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("inner map iterate failed: %w", err)
	}
	for i := range keys {
		if err := inner.Delete(&keys[i]); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("inner map delete failed: %w", err)
		}
	}
	return nil
}

// lookupInnerMap returns a cached inner map FD. Callers must not Close it.
func lookupInnerMap(outerMap *ebpf.Map, ifindex uint32, mapName string) (*ebpf.Map, error) {
	return acquireInnerMap(outerMap, ifindex, mapName, nil)
}

// cleanupNetPolicy flushes all entries in the inner LPM trie maps
// for the given ifindex in both allow_out_v3 and deny_out.
// This should be called during DeleteTAPDevice.
func cleanupNetPolicy(ifindex uint32) error {
	allowOut, err := loadPinnedMap(MapNameAllowOutV3)
	if err != nil {
		return err
	}
	defer allowOut.Close()

	err = flushAllowOutInnerMap(allowOut, ifindex)
	if err != nil {
		return fmt.Errorf("flush %s failed: %w", MapNameAllowOutV3, err)
	}

	denyOut, err := loadPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}
	defer denyOut.Close()

	return flushInnerMap(denyOut, ifindex)
}

// CleanupTAPDevicePolicy removes sandbox-specific CubeVS policy residue for one
// TAP ifindex. It does not install reusable-pool defaults and does not touch TAP
// metadata; callers compose those steps explicitly.
//
// Ready-pool reuse must keep the HashOfMaps outer keys (and reinstall default
// deny afterwards). Outer keys are deleted only when the TAP netdev itself is
// destroyed — see DeleteTAPDevicePolicyMaps and GCStaleNetPolicyMaps.
func CleanupTAPDevicePolicy(ifindex uint32) error {
	if err := cleanupNetPolicy(ifindex); err != nil {
		return err
	}
	if err := cleanupDNSAllow(ifindex); err != nil {
		return err
	}
	return cleanupDNSPolicyFlags(ifindex)
}

var netPolicyOuterMaps = []string{
	MapNameAllowOutV3,
	MapNameDenyOut,
	MapNameDNSAllowV2,
}

// DeleteTAPDevicePolicyMaps removes HashOfMaps outer entries for a destroyed TAP.
// Call this only after the host netdev is gone; Ready-pool cleanup must not use it.
func DeleteTAPDevicePolicyMaps(ifindex uint32) error {
	var errs []error
	for _, name := range netPolicyOuterMaps {
		outer, err := loadPinnedMap(name)
		if err != nil {
			// Drop any cached FD even when the pinned outer is unavailable so a
			// later reuse of this ifindex cannot see a stale userspace entry.
			releaseCachedInner(name, ifindex)
			errs = append(errs, err)
			continue
		}
		if err := deleteCachedInnerAndOuter(outer, name, ifindex); err != nil {
			errs = append(errs, fmt.Errorf("delete %s[%d]: %w", name, ifindex, err))
		}
		_ = outer.Close()
	}
	return errors.Join(errs...)
}

// GCStaleNetPolicyMaps deletes HashOfMaps outer keys whose ifindex is not in
// keep. keep must include every live pool TAP (Ready, Cleaning, and Active), not
// only Active sandboxes — Ready TAPs still need deny_out defaults.
//
// stillPresent is optional. When set, each candidate is re-checked immediately
// before delete; if it returns true the key is kept.
//
// onConflict is optional. After a successful delete, stillPresent is checked
// again; if the ifindex is now live, onConflict is invoked so the caller can
// restore default-deny (create raced between the pre-delete check and Delete).
func GCStaleNetPolicyMaps(keep map[uint32]struct{}, stillPresent func(uint32) bool, onConflict func(uint32)) (int, error) {
	if keep == nil {
		keep = map[uint32]struct{}{}
	}
	deleted := 0
	conflicted := make(map[uint32]struct{})
	var errs []error
	for _, name := range netPolicyOuterMaps {
		n, err := gcStaleOuterKeys(name, keep, stillPresent, conflicted)
		deleted += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	if onConflict != nil {
		for ifindex := range conflicted {
			onConflict(ifindex)
		}
	}
	return deleted, errors.Join(errs...)
}

func gcStaleOuterKeys(mapName string, keep map[uint32]struct{}, stillPresent func(uint32) bool, conflicted map[uint32]struct{}) (int, error) {
	outer, err := loadPinnedMap(mapName)
	if err != nil {
		return 0, err
	}
	defer outer.Close()

	var (
		ifindex uint32
		value   uint32
		stale   []uint32
	)
	iter := outer.Iterate()
	for iter.Next(&ifindex, &value) {
		if _, ok := keep[ifindex]; !ok {
			stale = append(stale, ifindex)
		}
	}
	if err := iter.Err(); err != nil {
		return 0, fmt.Errorf("iterate %s failed: %w", mapName, err)
	}

	deleted := 0
	var errs []error
	for _, ifindex := range stale {
		if stillPresent != nil && stillPresent(ifindex) {
			continue
		}
		if err := deleteCachedInnerAndOuter(outer, mapName, ifindex); err != nil {
			errs = append(errs, fmt.Errorf("delete stale %s[%d]: %w", mapName, ifindex, err))
			continue
		}
		deleted++
		// Create may have raced in after the pre-delete stillPresent check.
		if stillPresent != nil && stillPresent(ifindex) {
			conflicted[ifindex] = struct{}{}
		}
	}
	return deleted, errors.Join(errs...)
}

func cleanupDNSPolicyFlags(ifindex uint32) error {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer m.Close()

	var meta mvmMetadata
	if err := m.Lookup(&ifindex, &meta); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	if meta.DNSPolicyFlags == 0 {
		return nil
	}
	meta.DNSPolicyFlags = 0
	if err := m.Update(&ifindex, &meta, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	return nil
}

// InstallTAPDefaultDenyPolicy installs the invariant default deny entries
// without clearing policy maps. Cleanup paths call this only after
// CleanupTAPDevicePolicy has already removed per-sandbox policy residue.
func InstallTAPDefaultDenyPolicy(ifindex uint32) error {
	denyOut, err := loadPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}
	defer denyOut.Close()

	inner, err := acquireInnerMap(denyOut, ifindex, MapNameDenyOut, newInnerLPMMap)
	if err != nil {
		return err
	}
	if err := populateDenyOutInner(inner, alwaysDeniedSandboxEntries); err != nil {
		return fmt.Errorf("populate default %s failed: %w", MapNameDenyOut, err)
	}
	return nil
}

// parseCIDR parses a CIDR string (e.g. "10.0.0.0/8") or a plain IP
// (e.g. "10.1.2.3") into an lpmKey.
func parseCIDR(s string) (lpmKey, error) {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		// Try as a plain IP address (treated as /32).
		ip := net.ParseIP(s)
		if ip == nil {
			return lpmKey{}, fmt.Errorf("invalid CIDR or IP: %s", s) //nolint:err113
		}
		return lpmKey{Prefixlen: 32, IP: ipToUint32(ip)}, nil
	}
	ones, _ := ipNet.Mask.Size()
	return lpmKey{Prefixlen: uint32(ones), IP: ipToUint32(ipNet.IP)}, nil
}

func mustBuildDenyOutPolicyEntries(cidrs []string) []denyOutPolicyEntry {
	entries, err := buildDenyOutPolicyEntries(cidrs)
	if err != nil {
		panic(err)
	}
	return entries
}

func buildAllowOutPolicyEntries(allowOutCIDRs []string) ([]allowOutPolicyEntry, error) {
	entries := make([]allowOutPolicyEntry, 0, len(allowOutCIDRs))
	indexByKey := make(map[lpmKey]int, len(allowOutCIDRs))

	for _, cidr := range allowOutCIDRs {
		key, err := parseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		if _, ok := indexByKey[key]; ok {
			continue
		}
		indexByKey[key] = len(entries)
		entries = append(entries, allowOutPolicyEntry{
			key:    key,
			source: cidr,
		})
	}
	return entries, nil
}

// ntohsPort converts a network-order uint16 back to a host-order integer for
// use in error messages. eBPF stores ports in NBO to avoid conversions on hot
// paths; users see host order in their config.
func ntohsPort(p uint16) uint16 {
	return (p>>8)&0xff | (p<<8)&0xff00
}

// htonsPort converts host-order integer to network byte order for storage
// alongside the eBPF datapath's tcphdr->dest comparisons.
func htonsPort(p uint16) uint16 {
	return (p>>8)&0xff | (p<<8)&0xff00
}

// expandDefaultPortSet returns the (port, scheme) tuples applied when a user
// rule omits both port and scheme: the classic {80/http, 443/https}. Kept as a
// helper so the datapath fallback in mvmtap.bpf.c (port_count==0 branch) and
// the userspace expansion stay in lockstep.
func expandDefaultPortSet() []l7PortEntry {
	return []l7PortEntry{
		{Port: htonsPort(80), Scheme: L7SchemeHTTP},
		{Port: htonsPort(443), Scheme: L7SchemeHTTPS},
	}
}

// l7TargetKind separates L7Target hosts into the two datapath maps: CIDR
// literals go to allow_out_v3 as static entries; domain names go to dns_allow_v2
// so their (port, scheme) set follows learned IPs into allow_out_v3 at
// response time.
type l7TargetKind int

const (
	l7KindCIDR l7TargetKind = iota
	l7KindDomain
)

func classifyL7Target(host string) (l7TargetKind, error) {
	if isIPv4Target(host) {
		// An L7 host must be a single host, not a subnet CIDR: the datapath
		// matches exact (ip, port)/48 pairs and cannot express a subnet+port
		// rule, so reject network blocks here instead of silently narrowing
		// them to the network address downstream.
		key, err := parseCIDR(host)
		if err != nil {
			return 0, err
		}
		if key.Prefixlen < 32 {
			return 0, fmt.Errorf("invalid l7_allow_out host %s: subnet CIDR not supported for L7 rules, use a single host IP or a domain name", host) //nolint:err113
		}
		return l7KindCIDR, nil
	}
	if strings.Contains(host, "/") {
		return 0, fmt.Errorf("invalid l7_allow_out CIDR target: %s", host) //nolint:err113
	}
	if net.ParseIP(host) != nil || isDottedDecimalLikeTarget(host) {
		return 0, fmt.Errorf("unsupported l7_allow_out IP target: %s", host) //nolint:err113
	}
	if !isDNSAllowTarget(host) {
		return 0, fmt.Errorf("invalid l7_allow_out domain target: %s", host) //nolint:err113
	}
	return l7KindDomain, nil
}

// l7GroupKey returns a canonical grouping key for an L7 target host. Raw
// variants that resolve to the same datapath key — DNS names differing only by
// case or a trailing dot, CIDRs differing only by notation (e.g. "1.2.3.4" vs
// "1.2.3.4/32") — map to one key so buildL7Plan aggregates their port sets and
// detects (host, port) scheme conflicts, instead of letting them collide
// later in a last-write-wins same-key merge that silently drops ports and
// bypasses conflict detection.
func l7GroupKey(host string) (string, error) {
	kind, err := classifyL7Target(host)
	if err != nil {
		return "", err
	}
	if kind == l7KindCIDR {
		key, err := parseCIDR(host)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("cidr:%d:%08x", key.Prefixlen, key.IP), nil
	}
	// Match makeDNSAllowRule's case / trailing-dot normalisation.
	return "dns:" + strings.ToLower(strings.TrimSuffix(host, ".")), nil
}

// buildL7Plan merges rule targets by host, enforces (host, port) scheme
// consistency, and produces:
//   - allow_out_v3 static entries for IP/CIDR hosts (with port_count / ports
//     inherited from the merged tuple set),
//   - dns_allow_v2 rules for domain hosts (with the same port set).
//
// A target with Port == 0 && Scheme == L7SchemeNone means "unspecified": it
// expands to {80/http, 443/https} for merging purposes. Empty port_count is
// only preserved when *every* rule for that host is unspecified — the moment a
// user attaches an explicit (port, scheme) the port set becomes explicit and
// the datapath skips the default-set fallback.
func buildL7Plan(targets []L7Target) (l7CIDRs []allowOutPolicyEntry,
	l7DNS []dnsAllowRule, err error) {

	// Group by canonical host key (see l7GroupKey), not the raw host string,
	// so that raw variants resolving to the same datapath key aggregate into
	// one host. Track scheme per port to detect conflicts early.
	type hostState struct {
		raw         string           // first-seen raw host, used for output + errors
		portScheme  map[uint16]uint8 // key: NBO port, value: scheme
		portOrder   []uint16         // first appearance order for deterministic overflow
		anyExplicit bool
	}
	byHost := make(map[string]*hostState, len(targets))
	hostOrder := make([]string, 0, len(targets))

	for _, tgt := range targets {
		host := tgt.Host
		gkey, gerr := l7GroupKey(host)
		if gerr != nil {
			return nil, nil, gerr
		}
		st, ok := byHost[gkey]
		if !ok {
			st = &hostState{raw: host, portScheme: make(map[uint16]uint8)}
			byHost[gkey] = st
			hostOrder = append(hostOrder, gkey)
		}
		if tgt.Port == 0 && tgt.Scheme == L7SchemeNone {
			// Unspecified rule — expand to default set for conflict checking.
			for _, def := range expandDefaultPortSet() {
				if existing, ok := st.portScheme[def.Port]; ok {
					if existing != def.Scheme {
						return nil, nil, fmt.Errorf(
							"l7 rule conflict for %s port %d: scheme %d vs %d (via default set)", //nolint:err113
							host, ntohsPort(def.Port), existing, def.Scheme)
					}
				} else {
					st.portOrder = append(st.portOrder, def.Port)
				}
				st.portScheme[def.Port] = def.Scheme
			}
			continue
		}
		if tgt.Port == 0 || (tgt.Scheme != L7SchemeHTTP && tgt.Scheme != L7SchemeHTTPS) {
			return nil, nil, fmt.Errorf(
				"invalid l7 target %s: port and scheme must both be set (port=%d scheme=%d)", //nolint:err113
				host, tgt.Port, tgt.Scheme)
		}
		port := htonsPort(tgt.Port)
		if existing, ok := st.portScheme[port]; ok {
			if existing != tgt.Scheme {
				return nil, nil, fmt.Errorf(
					"l7 rule conflict for %s port %d: scheme %d vs %d", //nolint:err113
					host, tgt.Port, existing, tgt.Scheme)
			}
		} else {
			st.portOrder = append(st.portOrder, port)
		}
		st.portScheme[port] = tgt.Scheme
		st.anyExplicit = true
	}

	// Materialise per-host port entries. Order host iteration to keep output
	// deterministic (test assertions compare slices directly). hostOrder holds
	// canonical group keys; the first-seen raw host drives classification and
	// key construction (it resolves to the same datapath key as the group).
	for _, gkey := range hostOrder {
		st := byHost[gkey]
		host := st.raw
		var ports []l7PortEntry
		if st.anyExplicit || len(st.portScheme) != len(expandDefaultPortSet()) {
			// Emit the full port list when an explicit (port, scheme) rule is
			// attached. The second disjunct is defensive: it is unreachable
			// today (a pure-default host always has exactly the default set's
			// two tuples) but guards against expandDefaultPortSet() changing
			// cardinality, which would otherwise silently drop a tuple here.
			ports = make([]l7PortEntry, 0, len(st.portOrder))
			for _, p := range st.portOrder {
				ports = append(ports, l7PortEntry{Port: p, Scheme: st.portScheme[p]})
			}
			if len(ports) > maxL7PortsPerHost {
				return nil, nil, fmt.Errorf(
					"l7 rule exceeds %d port tuples for %s", //nolint:err113
					maxL7PortsPerHost, host)
			}
		}
		// ports == nil for pure-default hosts: signal "use default set" to the
		// datapath (port_count = 0).

		kind, cerr := classifyL7Target(host)
		if cerr != nil {
			return nil, nil, cerr
		}
		switch kind {
		case l7KindCIDR:
			key, perr := parseCIDR(host)
			if perr != nil {
				return nil, nil, perr
			}
			l7CIDRs = append(l7CIDRs, allowOutPolicyEntry{
				key:    key,
				flags:  uint8(netPolicyFlagL7Required),
				ports:  ports,
				source: host,
			})
		case l7KindDomain:
			key, value, merr := makeDNSAllowRule(host, uint8(netPolicyFlagL7Required))
			if merr != nil {
				return nil, nil, merr
			}
			applyPortsToDNSAllowValue(&value, ports)
			l7DNS = append(l7DNS, dnsAllowRule{
				key:    key,
				value:  value,
				domain: host,
			})
		}
	}
	return l7CIDRs, l7DNS, nil
}

// applyPortsToDNSAllowValue copies ports into the DNS allow value. len(ports)==0
// leaves PortCount=0 so the datapath falls back to {80,443} at match time.
func applyPortsToDNSAllowValue(v *dnsAllowValue, ports []l7PortEntry) {
	if len(ports) == 0 {
		v.PortCount = 0
		return
	}
	v.PortCount = uint8(len(ports))
	for i, p := range ports {
		v.Ports[i] = p
	}
}

func buildDenyOutPolicyEntries(cidrs []string) ([]denyOutPolicyEntry, error) {
	entries := make([]denyOutPolicyEntry, 0, len(cidrs))
	seen := make(map[lpmKey]struct{}, len(cidrs))
	for _, cidr := range cidrs {
		key, err := parseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, denyOutPolicyEntry{
			key:    key,
			source: cidr,
		})
	}
	return entries, nil
}

func buildNetPolicyPlan(opts MVMOptions) (*netPolicyPlan, error) {
	var allowOut []string
	if opts.AllowOut != nil {
		allowOut = *opts.AllowOut
	}
	allowOutCIDRs, dnsAllowDomains, err := splitAllowOutTargets(allowOut)
	if err != nil {
		return nil, err
	}

	var l7Targets []L7Target
	if opts.L7AllowOut != nil {
		l7Targets = *opts.L7AllowOut
	}
	l7CIDRs, l7DNSRules, err := buildL7Plan(l7Targets)
	if err != nil {
		return nil, err
	}

	baseAllowOutEntries, err := buildAllowOutPolicyEntries(allowOutCIDRs)
	if err != nil {
		return nil, err
	}
	// Merge non-L7 allow_out CIDRs with L7 CIDRs. If the same key appears in
	// both, keep the L7 entry (its flags + ports are strictly a superset).
	allowOutEntries := mergeAllowOutWithL7(baseAllowOutEntries, l7CIDRs)

	// Domain rules: non-L7 domains get flags=0 and no port set; L7 domain
	// rules already carry flags + ports from buildL7Plan. Merge by key.
	dnsAllowRules, err := buildDNSAllowRules(dnsAllowDomains)
	if err != nil {
		return nil, err
	}
	dnsAllowRules = mergeDNSAllowRules(dnsAllowRules, l7DNSRules)
	// Mark each L7 domain rule that is also covered by a plain (non-L7)
	// allow_out domain — an exact same-host entry or a leading-"*." wildcard —
	// so the datapath keeps the plain /32 L3 access alongside the L7 /48
	// interception for that host.
	markL3AllowedByPlainCover(dnsAllowRules, dnsAllowDomains)

	var denyOutEntries []denyOutPolicyEntry
	if opts.AllowInternetAccess != nil && !*opts.AllowInternetAccess {
		denyOutEntries, err = buildDenyOutPolicyEntries([]string{"0.0.0.0/0"})
	} else {
		if opts.DenyOut != nil {
			denyOutEntries, err = buildDenyOutPolicyEntries(*opts.DenyOut)
			if err != nil {
				return nil, err
			}
		}
	}
	if err != nil {
		return nil, err
	}

	plan := &netPolicyPlan{
		allowOutEntries: allowOutEntries,
		dnsAllowRules:   dnsAllowRules,
		denyOutEntries:  denyOutEntries,
		dnsPolicyFlags:  dnsPolicyFlagsForDomains(dnsAllowDomains, l7DNSDomainNames(l7DNSRules)),
	}
	if err := validateNetPolicyPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// l7DNSDomainNames pulls the raw domain string out of each L7 dns rule so
// dnsPolicyFlagsForDomains can decide whether to enable DNS learning. Only the
// count matters — the caller only tests len>0.
func l7DNSDomainNames(rules []dnsAllowRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.domain)
	}
	return out
}

// mergeAllowOutWithL7 combines the non-L7 base entries with the L7 entries.
// When the same LPM key appears in both, the L7 version's flags and port set
// win, and the merged entry is also marked netPolicyFlagL3Allowed so
// populateAllowOutInnerMap writes the plain /32 any-port entry alongside the
// L7 /48 entries — otherwise an L7 rule would silently narrow a same-host
// plain allow_out to only the rule's ports. (Domain hosts get the same
// treatment in mergeDNSAllowRules.)
func mergeAllowOutWithL7(base, l7 []allowOutPolicyEntry) []allowOutPolicyEntry {
	if len(l7) == 0 {
		return base
	}
	byKey := make(map[lpmKey]int, len(base)+len(l7))
	out := make([]allowOutPolicyEntry, 0, len(base)+len(l7))
	for _, e := range base {
		byKey[e.key] = len(out)
		out = append(out, e)
	}
	for _, e := range l7 {
		if idx, ok := byKey[e.key]; ok {
			out[idx].flags |= e.flags | netPolicyFlagL3Allowed
			out[idx].ports = e.ports
			continue
		}
		byKey[e.key] = len(out)
		out = append(out, e)
	}
	return out
}

// mergeDNSAllowRules combines the non-L7 domain rules with the L7 domain rules.
// Same-key entries merge flags (OR) and adopt the L7 rule's port set. The
// netPolicyFlagL3Allowed marker (plain-L3 + L7 coexistence) is applied
// separately by markL3AllowedByPlainCover, which handles both the exact
// same-host case and a leading-"*." wildcard allow_out covering the L7 host.
func mergeDNSAllowRules(base, l7 []dnsAllowRule) []dnsAllowRule {
	if len(l7) == 0 {
		return base
	}
	byKey := make(map[dnsAllowKey]int, len(base)+len(l7))
	out := make([]dnsAllowRule, 0, len(base)+len(l7))
	for _, r := range base {
		byKey[r.key] = len(out)
		out = append(out, r)
	}
	for _, r := range l7 {
		if idx, ok := byKey[r.key]; ok {
			out[idx].value.Flags |= r.value.Flags
			out[idx].value.PortCount = r.value.PortCount
			out[idx].value.Ports = r.value.Ports
			continue
		}
		byKey[r.key] = len(out)
		out = append(out, r)
	}
	return out
}

// l7DomainHasPlainCover reports whether an L7 rule host is covered by a plain
// (non-L7) allow_out domain — either an exact same-host entry or a
// leading-"*." wildcard whose base the host is a subdomain of. Matching follows
// the same semantics as makeDNSAllowRule / domain_match: case-insensitive, a
// trailing dot is ignored, and "*.base" covers any-depth subdomains of base but
// not the apex itself.
func l7DomainHasPlainCover(host string, plainDomains []string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, raw := range plainDomains {
		d := strings.ToLower(strings.TrimSuffix(raw, "."))
		if strings.HasPrefix(d, "*.") {
			base := d[2:]
			if h != base && strings.HasSuffix(h, "."+base) {
				return true
			}
			continue
		}
		if h == d {
			return true
		}
	}
	return false
}

// markL3AllowedByPlainCover sets netPolicyFlagL3Allowed on each L7 dns_allow
// rule whose host is also covered by a plain (non-L7) allow_out domain. This
// is what lets a host keep its plain /32 L3 access (SNAT on non-rule ports)
// alongside the L7 /48 interception on the rule's ports — both when the host
// appears verbatim in allow_out and when a leading-"*." wildcard covers it
// (the exact rule otherwise shadows the wildcard in the DNS LPM match, which
// would silently deny the host's non-rule ports under deny-all).
func markL3AllowedByPlainCover(rules []dnsAllowRule, plainDomains []string) {
	for i := range rules {
		if rules[i].value.Flags&uint8(netPolicyFlagL7Required) == 0 {
			continue
		}
		if rules[i].value.Flags&uint8(netPolicyFlagL3Allowed) != 0 {
			continue
		}
		if l7DomainHasPlainCover(rules[i].domain, plainDomains) {
			rules[i].value.Flags |= uint8(netPolicyFlagL3Allowed)
		}
	}
}

func appendDenyOutPolicyEntries(dst, src []denyOutPolicyEntry) []denyOutPolicyEntry {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[lpmKey]struct{}, len(dst)+len(src))
	for _, entry := range dst {
		seen[entry.key] = struct{}{}
	}
	for _, entry := range src {
		if _, ok := seen[entry.key]; ok {
			continue
		}
		seen[entry.key] = struct{}{}
		dst = append(dst, entry)
	}
	return dst
}

func effectiveDenyOutEntriesForReplace(plan *netPolicyPlan) []denyOutPolicyEntry {
	if plan == nil {
		return nil
	}
	entries := make([]denyOutPolicyEntry, len(plan.denyOutEntries), len(plan.denyOutEntries)+len(alwaysDeniedSandboxEntries))
	copy(entries, plan.denyOutEntries)
	return appendDenyOutPolicyEntries(entries, alwaysDeniedSandboxEntries)
}

// expandedAllowOutEntryCount returns the number of allow_out_v3 inner-map
// entries a plan actually occupies: one per plain (non-L7) allow, and
// len(ports) per L7 rule — or the default {80, 443} set when the L7 rule has
// no explicit ports. The inner LPM trie is bounded by maxNetPolicyEntries, so
// budget validation must use this expanded count, not len(allowOutEntries)
// (which counts one per host and undercounts multi-port L7 rules, letting
// population overflow mid-write with E2BIG and leave a half-populated map).
func expandedAllowOutEntryCount(entries []allowOutPolicyEntry) int {
	defaultPorts := len(expandDefaultPortSet())
	total := 0
	for _, e := range entries {
		if e.flags&netPolicyFlagL7Required != 0 {
			n := len(e.ports)
			if n == 0 {
				n = defaultPorts
			}
			if e.flags&netPolicyFlagL3Allowed != 0 {
				// The host also gets a plain /32 any-port entry (coexistence).
				n++
			}
			total += n
			continue
		}
		total++
	}
	return total
}

func validateNetPolicyPlan(plan *netPolicyPlan) error {
	if err := validateNetPolicyEntryCount("network.allow_out_v3", expandedAllowOutEntryCount(plan.allowOutEntries), maxNetPolicyEntries); err != nil {
		return err
	}
	if err := validateNetPolicyEntryCount("network.dns_allow", len(plan.dnsAllowRules), maxDNSAllowDomains); err != nil {
		return err
	}
	return validateNetPolicyEntryCount("network.deny_out", len(effectiveDenyOutEntriesForReplace(plan)), maxNetPolicyEntries)
}

func validateNetPolicyEntryCounts(allowOutCIDRs, l7AllowOutCIDRs, dnsAllowDomains, l7DNSAllowDomains, denyOut []string) error {
	if count, err := countUniqueLPMEntries(allowOutCIDRs, l7AllowOutCIDRs); err != nil {
		return err
	} else if err := validateNetPolicyEntryCount("network.allow_out_v3", count, maxNetPolicyEntries); err != nil {
		return err
	}

	if count, err := countUniqueDNSAllowEntries(dnsAllowDomains, l7DNSAllowDomains); err != nil {
		return err
	} else if err := validateNetPolicyEntryCount("network.dns_allow", count, maxDNSAllowDomains); err != nil {
		return err
	}

	if count, err := countUniqueLPMEntries(denyOut); err != nil {
		return err
	} else if err := validateNetPolicyEntryCount("network.deny_out", count, maxNetPolicyEntries); err != nil {
		return err
	}

	return nil
}

func countUniqueLPMEntries(groups ...[]string) (int, error) {
	seen := make(map[lpmKey]struct{})
	for _, group := range groups {
		for _, cidr := range group {
			key, err := parseCIDR(cidr)
			if err != nil {
				return 0, err
			}
			seen[key] = struct{}{}
		}
	}
	return len(seen), nil
}

func countUniqueDNSAllowEntries(groups ...[]string) (int, error) {
	seen := make(map[dnsAllowKey]struct{})
	for _, group := range groups {
		for _, domain := range group {
			key, _, err := makeDNSAllowRule(domain, 0)
			if err != nil {
				return 0, err
			}
			seen[key] = struct{}{}
		}
	}
	return len(seen), nil
}

func validateNetPolicyEntryCount(field string, count int, maxEntries int) error {
	if count <= maxEntries {
		return nil
	}
	return fmt.Errorf("%s exceeds maximum entries: got %d, max %d", field, count, maxEntries) //nolint:err113
}

func dnsPolicyFlagsForDomains(allowDomains, l7Domains []string) uint8 {
	if len(allowDomains)+len(l7Domains) == 0 {
		return 0
	}
	return dnsPolicyFlagLearningEnabled
}

func dnsPolicyLearningEnabled(flags uint8) bool {
	return flags&uint8(dnsPolicyFlagLearningEnabled) != 0
}

func setDNSPolicyFlags(ifindex uint32, flags uint8) error {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer m.Close()

	var meta mvmMetadata
	if err := m.Lookup(&ifindex, &meta); err != nil {
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	if meta.DNSPolicyFlags == flags {
		return nil
	}
	meta.DNSPolicyFlags = flags
	if err := m.Update(&ifindex, &meta, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	return nil
}

// splitAllowOutTargets separates user-facing allow_out targets into IPv4/CIDR
// entries for allow_out_v3 and DNS names for dns_allow_v2.
func splitAllowOutTargets(targets []string) ([]string, []string, error) {
	cidrs := make([]string, 0, len(targets))
	domains := make([]string, 0, len(targets))
	for _, rawTarget := range targets {
		target := strings.TrimSpace(rawTarget)
		if target == "" {
			return nil, nil, fmt.Errorf("invalid allow_out target: empty") //nolint:err113
		}
		if isIPv4Target(target) {
			cidrs = append(cidrs, target)
			continue
		}
		if strings.Contains(target, "/") {
			return nil, nil, fmt.Errorf("invalid allow_out CIDR target: %s", target) //nolint:err113
		}
		if net.ParseIP(target) != nil || isDottedDecimalLikeTarget(target) {
			return nil, nil, fmt.Errorf("unsupported allow_out IP target: %s", target) //nolint:err113
		}
		if !IsAllowOutDomainTarget(target) {
			return nil, nil, fmt.Errorf("invalid allow_out domain target: %s", target) //nolint:err113
		}
		domains = append(domains, target)
	}
	return cidrs, domains, nil
}

func isIPv4Target(target string) bool {
	if strings.Contains(target, "/") {
		ip, _, err := net.ParseCIDR(target)
		return err == nil && ip.To4() != nil
	}
	ip := net.ParseIP(target)
	return ip != nil && ip.To4() != nil
}

func isDottedDecimalLikeTarget(target string) bool {
	parts := strings.Split(strings.TrimSuffix(target, "."), ".")
	if len(parts) != net.IPv4len {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

// IsAllowOutDomainTarget reports whether an allow_out target is installed as a
// dns_allow_v2 domain rule rather than an allow_out_v3 CIDR.
//
// IP and CIDR literals take precedence, exactly as splitAllowOutTargets decides
// it: isDNSAllowTarget() alone accepts an all-numeric name like "10.0.0.1"
// because digits are valid DNS label characters, so asking it directly reports a
// bare IPv4 literal as a domain. Callers outside this package need the install
// decision, not the name-shape check, so this is the one they get.
func IsAllowOutDomainTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" || isIPv4Target(target) || strings.Contains(target, "/") {
		return false
	}
	if net.ParseIP(target) != nil || isDottedDecimalLikeTarget(target) {
		return false
	}
	return isDNSAllowTarget(target)
}

func isDNSAllowTarget(target string) bool {
	domain := strings.ToLower(strings.TrimSuffix(target, "."))
	if strings.HasPrefix(domain, "*.") {
		domain = domain[2:]
	} else if strings.Contains(domain, "*") {
		return false
	}
	if domain == "" || len(domain) >= maxDNSNameLen-1 {
		return false
	}
	return isValidDNSDomainName(domain)
}

func isValidDNSDomainName(domain string) bool {
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, ch := range label {
			isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
			if !isAlphaNum && ch != '-' {
				return false
			}
			if ch == '-' && (i == 0 || i == len(label)-1) {
				return false
			}
		}
	}
	return true
}

func populateDenyOutInner(inner *ebpf.Map, entries []denyOutPolicyEntry) error {
	val := uint32(netPolicyValueStatic)
	for _, entry := range entries {
		if err := inner.Update(&entry.key, &val, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("inner map update failed: %w, cidr: %s", err, entry.source)
		}
	}
	return nil
}

// populateAllowOutInnerMap inserts pre-parsed static allow_out_v3 entries.
//
// v3 carries the port in the LPM key, so an L7 entry (flags &
// netPolicyFlagL7Required) is materialised as one exact (ip, port)/48 entry
// per (port, scheme) tuple — a default port set expands to {80/http,
// 443/https}. A plain allow is a single ip-only (or subnet) /32 entry
// with scheme = NONE. When a static (ip, port) key already holds a
// DNS-learned entry, the learned flags are merged in but the static (zero)
// expiry wins — the entry becomes permanent. Keeping the learned TTL
// instead would let the reaper delete the entry at the old TTL and
// silently drop the static verdict; a later DNS refresh preserves the
// static zero expiry (dns_response.h same-key rule).
func populateAllowOutInnerMap(outerMap *ebpf.Map, ifindex uint32, entries []allowOutPolicyEntry) error {
	inner, err := acquireInnerMap(outerMap, ifindex, MapNameAllowOutV3, nil)
	if err != nil {
		return err
	}
	return populateAllowOutInner(inner, entries)
}

// allowOutRow is one inner-map row a plan entry occupies.
type allowOutRow struct {
	key lpmKeyV3
	val netPolicyValueV3
	// mergeLearnedFlags marks the exact (ip, port)/48 rows an L7 rule owns,
	// which are the only keys a DNS-learned entry can also land on. Plain
	// any-port rows overwrite outright: nothing else writes that key, so
	// merging there would just resurrect stale flags.
	mergeLearnedFlags bool
}

// expandAllowOutEntry materialises the rows one plan entry occupies: an L7
// entry becomes one exact (ip, port)/48 row per (port, scheme) tuple — an empty
// port set means the default {80/http, 443/https} — plus, when the host is also
// in plain allow_out (netPolicyFlagL3Allowed), the any-port row that keeps L3
// access on every other port. A plain entry is a single any-port row.
//
// Shared by the populate and diff paths so the set of keys written can never
// drift from the set of keys considered current.
func expandAllowOutEntry(entry allowOutPolicyEntry) []allowOutRow {
	plainRow := func(flags uint8) allowOutRow {
		return allowOutRow{
			key: lpmKeyV3{Prefixlen: entry.key.Prefixlen, IP: entry.key.IP},
			val: netPolicyValueV3{Flags: flags, KeyPrefixlen: uint8(entry.key.Prefixlen)},
		}
	}
	if entry.flags&netPolicyFlagL7Required == 0 {
		return []allowOutRow{plainRow(entry.flags)}
	}

	ports := entry.ports
	if len(ports) == 0 {
		ports = expandDefaultPortSet()
	}
	rows := make([]allowOutRow, 0, len(ports)+1)
	for _, p := range ports {
		rows = append(rows, allowOutRow{
			key:               lpmKeyV3{Prefixlen: 48, IP: entry.key.IP, Port: p.Port},
			val:               netPolicyValueV3{Flags: entry.flags, Scheme: p.Scheme, KeyPrefixlen: 48},
			mergeLearnedFlags: true,
		})
	}
	if entry.flags&netPolicyFlagL3Allowed != 0 {
		// The /48 rows win for the rule's ports (longest prefix); this one
		// covers every other port via plain SNAT. Strip the marker bits so it
		// reads as a plain allow.
		rows = append(rows, plainRow(entry.flags&^(netPolicyFlagL7Required|netPolicyFlagL3Allowed)))
	}
	return rows
}

func populateAllowOutInner(inner *ebpf.Map, entries []allowOutPolicyEntry) error {
	for _, entry := range entries {
		for _, row := range expandAllowOutEntry(entry) {
			val := row.val
			if row.mergeLearnedFlags {
				var oldVal netPolicyValueV3
				switch err := inner.Lookup(&row.key, &oldVal); {
				case err == nil:
					// LPM lookup is longest-prefix: only merge with an entry
					// written under the EXACT same key, never with a shorter
					// covering entry (whose flags would otherwise leak in).
					// Flags only: the static (zero) expiry wins over a learned
					// same-key entry, so the entry becomes permanent rather
					// than ageing out at the old TTL.
					if oldVal.KeyPrefixlen == uint8(row.key.Prefixlen) {
						val.Flags |= oldVal.Flags
					}
				case !errors.Is(err, ebpf.ErrKeyNotExist):
					return fmt.Errorf("inner map lookup failed: %w, cidr: %s", err, entry.source)
				}
			}
			if err := inner.Update(&row.key, &val, ebpf.UpdateAny); err != nil {
				return fmt.Errorf("inner map update failed: %w, cidr: %s", err, entry.source)
			}
		}
	}
	return nil
}

// netPolicyValueV3Expired reports whether a v3 allow entry is a dynamic
// entry whose DNS-learned TTL has expired. Static entries have ExpiresAtNS
// set to 0.
func netPolicyValueV3Expired(value netPolicyValueV3, now uint64) bool {
	return value.ExpiresAtNS != 0 && value.ExpiresAtNS <= now
}

// UpdateTAPDevicePolicy converges an already-registered TAP's egress policy on
// opts, then bumps the sandbox's policy generation so the datapath re-evaluates
// established flows.
//
// It is the third apply mode alongside applyNetPolicy (additive, create path)
// and replaceNetPolicy (flush + refill, recovery path). Neither fits a live
// sandbox: flushing would blank the policy for as long as the refill takes, and
// swapping the inner map would defeat the HashOfMaps inner cache and pay a
// synchronize_rcu() on every update. So this diffs against what is installed
// and issues only the required per-entry writes.
//
// Two ordering rules:
//
//   - The generation bump is last. A failure before it leaves established flows
//     on their cached verdict instead of judging them against a half-applied
//     map — and because a revoked flow is deleted outright, a premature bump
//     could retire flows the finished policy would have allowed.
//   - Within each map, revocations land before additions. No intermediate state
//     is then more permissive than both the old and the new policy.
//
// Nothing is rolled back on failure. The diff is computed from the live maps,
// so replaying the same request converges; and the caller's durable state is
// only written after this returns, so a restart re-applies the previous policy
// in full.
func UpdateTAPDevicePolicy(ifindex uint32, opts MVMOptions) error {
	// Validate the whole desired state before touching anything: a rejected
	// plan must leave both the L3 maps and the caller's L7 push untouched.
	plan, err := buildNetPolicyPlan(opts)
	if err != nil {
		return err
	}

	if err := syncAllowOutInner(ifindex, plan.allowOutEntries); err != nil {
		return fmt.Errorf("sync %s failed: %w", MapNameAllowOutV3, err)
	}
	if err := syncDenyOutInner(ifindex, effectiveDenyOutEntriesForReplace(plan)); err != nil {
		return fmt.Errorf("sync %s failed: %w", MapNameDenyOut, err)
	}
	if err := syncDNSAllowInner(ifindex, plan.dnsAllowRules); err != nil {
		return fmt.Errorf("sync %s failed: %w", MapNameDNSAllowV2, err)
	}
	if err := setDNSPolicyFlags(ifindex, plan.dnsPolicyFlags); err != nil {
		return err
	}
	return bumpPolicyVersion(ifindex)
}

// syncAllowOutInner converges allow_out_v3 for one TAP on the desired entries.
//
// Only static rows are managed. DNS-learned rows (non-zero expiry) are left
// exactly as they are: their lifetime belongs to the TTL and the reaper, and a
// revoked domain rule stops producing new ones as soon as dns_allow_v2 is
// synced. Removing a domain therefore takes effect for already-resolved IPs
// only once they age out.
func syncAllowOutInner(ifindex uint32, entries []allowOutPolicyEntry) error {
	outer, err := loadPinnedMap(MapNameAllowOutV3)
	if err != nil {
		return err
	}
	defer outer.Close()

	inner, err := acquireInnerMap(outer, ifindex, MapNameAllowOutV3, newInnerAllowOutMap)
	if err != nil {
		return err
	}

	desired := make(map[lpmKeyV3]struct{}, len(entries))
	for _, entry := range entries {
		for _, row := range expandAllowOutEntry(entry) {
			desired[row.key] = struct{}{}
		}
	}

	stale, err := staleKeys(inner, desired, func(v *netPolicyValueV3) bool {
		return v.ExpiresAtNS == 0
	})
	if err != nil {
		return err
	}
	if err := deleteKeys(inner, stale); err != nil {
		return err
	}
	return populateAllowOutInner(inner, entries)
}

// syncDenyOutInner converges deny_out for one TAP. Every row is managed, so the
// caller must pass the effective set including the always-denied private and
// link-local ranges — otherwise an update would drop the invariant deny rules.
func syncDenyOutInner(ifindex uint32, entries []denyOutPolicyEntry) error {
	outer, err := loadPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}
	defer outer.Close()

	inner, err := acquireInnerMap(outer, ifindex, MapNameDenyOut, newInnerLPMMap)
	if err != nil {
		return err
	}

	desired := make(map[lpmKey]struct{}, len(entries))
	for _, entry := range entries {
		desired[entry.key] = struct{}{}
	}

	stale, err := staleKeys(inner, desired, func(*uint32) bool { return true })
	if err != nil {
		return err
	}
	if err := deleteKeys(inner, stale); err != nil {
		return err
	}
	return populateDenyOutInner(inner, entries)
}

// staleKeys returns the managed keys currently in inner that desired no longer
// covers. managed decides which rows this policy owns; rows it rejects are left
// alone.
//
// Keys are collected first and deleted afterwards: deleting while iterating a
// BPF hash map can make the cursor skip live entries.
func staleKeys[K comparable, V any](inner *ebpf.Map, desired map[K]struct{}, managed func(*V) bool) ([]K, error) {
	var (
		key   K
		value V
		stale []K
	)
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		if !managed(&value) {
			continue
		}
		if _, keep := desired[key]; !keep {
			stale = append(stale, key)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("inner map iterate failed: %w", err)
	}
	return stale, nil
}

func deleteKeys[K any](inner *ebpf.Map, keys []K) error {
	for i := range keys {
		if err := inner.Delete(&keys[i]); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("inner map delete failed: %w", err)
		}
	}
	return nil
}

// bumpPolicyVersion advances the sandbox's policy generation. Every established
// flow compares its cached copy against this value on the next packet, so this
// is what makes an update reach traffic that already exists.
func bumpPolicyVersion(ifindex uint32) error {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer m.Close()

	var meta mvmMetadata
	if err := m.Lookup(&ifindex, &meta); err != nil {
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	meta.PolicyVersion++
	if err := m.Update(&ifindex, &meta, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	return nil
}

// applyNetPolicy configures egress network policy for the given ifindex
// based on MVMOptions.
//
// Rules:
//   - AllowOut IP/CIDR targets are inserted into allow_out_v3 inner map.
//   - L7AllowOut IP/CIDR targets are inserted into allow_out_v3 with the L7 flag.
//   - AllowOut domain targets are inserted into dns_allow_v2 inner map.
//   - L7AllowOut domain targets are inserted into dns_allow_v2 with the L7 flag.
//   - Default private/link-local DenyOut ranges are preloaded when a TAP enters
//     the free pool. Replace paths replay them after flushing policy maps.
//   - AllowInternetAccess=false: DenyOut is set to "0.0.0.0/0" (deny all).
func applyNetPolicy(ifindex uint32, opts MVMOptions) error {
	return applyNetPolicyWithMode(ifindex, opts, false)
}

// replaceNetPolicy replaces all configured egress policy for an ifindex.
// It is used by TAP upsert/recovery paths so removed policy entries do not
// survive a network-agent restart.
func replaceNetPolicy(ifindex uint32, opts MVMOptions) error {
	return applyNetPolicyWithMode(ifindex, opts, true)
}

func applyNetPolicyWithMode(ifindex uint32, opts MVMOptions, replace bool) error {
	plan, err := buildNetPolicyPlan(opts)
	if err != nil {
		return err
	}

	if replace || len(plan.allowOutEntries) > 0 {
		allowOutMap, err := loadPinnedMap(MapNameAllowOutV3)
		if err != nil {
			return err
		}
		defer allowOutMap.Close()

		inner, err := acquireInnerMap(allowOutMap, ifindex, MapNameAllowOutV3, newInnerAllowOutMap)
		if err != nil {
			return err
		}
		if replace {
			if err := flushInnerEntries[lpmKeyV3, netPolicyValueV3](inner); err != nil {
				return fmt.Errorf("flush %s failed: %w", MapNameAllowOutV3, err)
			}
		}
		if err := populateAllowOutInner(inner, plan.allowOutEntries); err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameAllowOutV3, err)
		}
	}
	if err := applyDNSAllow(ifindex, plan.dnsAllowRules, replace); err != nil {
		return fmt.Errorf("populate %s failed: %w", MapNameDNSAllowV2, err)
	}

	denyOutEntries := plan.denyOutEntries
	if replace {
		denyOutEntries = effectiveDenyOutEntriesForReplace(plan)
	}
	if replace || len(denyOutEntries) > 0 {
		denyOutMap, err := loadPinnedMap(MapNameDenyOut)
		if err != nil {
			return err
		}
		defer denyOutMap.Close()

		inner, err := acquireInnerMap(denyOutMap, ifindex, MapNameDenyOut, newInnerLPMMap)
		if err != nil {
			return err
		}
		if replace {
			if err := flushInnerEntries[lpmKey, uint32](inner); err != nil {
				return fmt.Errorf("flush %s failed: %w", MapNameDenyOut, err)
			}
		}
		if err := populateDenyOutInner(inner, denyOutEntries); err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameDenyOut, err)
		}
	}

	if !replace && plan.dnsPolicyFlags == 0 {
		return nil
	}
	return setDNSPolicyFlags(ifindex, plan.dnsPolicyFlags)
}
