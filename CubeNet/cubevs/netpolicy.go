package cubevs

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"unsafe"

	"github.com/cilium/ebpf"
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

// newInnerLPMMap creates a new LPM trie map with uint32 values for deny_out.
func newInnerLPMMap() (*ebpf.Map, error) {
	return newInnerLPMMapWithValueSize(uint32(unsafe.Sizeof(uint32(0))))
}

func newInnerLPMMapWithValueSize(valueSize uint32) (*ebpf.Map, error) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(lpmKey{})),
		ValueSize:  valueSize,
		MaxEntries: maxNetPolicyEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
	})
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMap(LPMTrie) failed: %w", err)
	}
	return m, nil
}

func newInnerAllowOutMap() (*ebpf.Map, error) {
	return newInnerLPMMapWithValueSize(uint32(unsafe.Sizeof(netPolicyValueV2{})))
}

func ensureAllowOutV2InnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return ensureInnerMapWithFactory(outerMap, ifindex, MapNameAllowOutV2, newInnerAllowOutMap)
}

func ensureDenyOutInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return ensureInnerMapWithFactory(outerMap, ifindex, MapNameDenyOut, newInnerLPMMap)
}

// ensureInnerMap checks whether the outer hash-of-maps already has an
// inner map for the given ifindex.  If not, it creates one and inserts it.
func ensureInnerMap(outerMap *ebpf.Map, ifindex uint32, mapName string) error {
	return ensureInnerMapWithFactory(outerMap, ifindex, mapName, newInnerLPMMap)
}

func ensureInnerMapWithFactory(outerMap *ebpf.Map, ifindex uint32, mapName string,
	newInner func() (*ebpf.Map, error),
) error {
	// Check if inner map already exists for this ifindex.
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err == nil {
		// Already present, nothing to do.
		return nil
	}
	if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, mapName)
	}

	// Create a new inner LPM trie map and insert it.
	inner, err := newInner()
	if err != nil {
		return err
	}
	defer inner.Close()

	err = outerMap.Put(&ifindex, inner)
	if err != nil {
		return fmt.Errorf("map.Put failed: %w, name: %s", err, mapName)
	}
	return nil
}

// initNetPolicy creates inner LPM trie maps for the given ifindex
// in allow_out_v2, deny_out and dns_allow hash-of-maps, if not already present.
// This should be called during AttachFilter.
func initNetPolicy(ifindex uint32) error {
	allowOut, err := loadPinnedMap(MapNameAllowOutV2)
	if err != nil {
		return err
	}
	defer allowOut.Close()

	err = ensureAllowOutV2InnerMap(allowOut, ifindex)
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
	return flushInnerMapWithValue(outerMap, ifindex, new(uint32))
}

func flushAllowOutInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return flushInnerMapWithValue(outerMap, ifindex, new(netPolicyValueV2))
}

func flushInnerMapWithValue(outerMap *ebpf.Map, ifindex uint32, value any) error {
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("map.Lookup failed: %w", err)
	}

	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	var key lpmKey
	iter := inner.Iterate()
	for iter.Next(&key, value) {
		_ = inner.Delete(&key)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("inner map iterate failed: %w", err)
	}
	return nil
}

func lookupInnerMap(outerMap *ebpf.Map, ifindex uint32) (*ebpf.Map, error) {
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err != nil {
		return nil, fmt.Errorf("map.Lookup failed: %w", err)
	}

	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	return inner, nil
}

// cleanupNetPolicy flushes all entries in the inner LPM trie maps
// for the given ifindex in both allow_out_v2 and deny_out.
// This should be called during DelTAPDevice.
func cleanupNetPolicy(ifindex uint32) error {
	allowOut, err := loadPinnedMap(MapNameAllowOutV2)
	if err != nil {
		return err
	}
	defer allowOut.Close()

	err = flushAllowOutInnerMap(allowOut, ifindex)
	if err != nil {
		return fmt.Errorf("flush %s failed: %w", MapNameAllowOutV2, err)
	}

	denyOut, err := loadPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}
	defer denyOut.Close()

	return flushInnerMap(denyOut, ifindex)
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

func validateNetPolicyEntryCounts(allowOutCIDRs, l7AllowOutCIDRs, dnsAllowDomains, l7DNSAllowDomains, denyOut []string) error {
	if count, err := countUniqueLPMEntries(allowOutCIDRs, l7AllowOutCIDRs); err != nil {
		return err
	} else if err := validateNetPolicyEntryCount("network.allow_out_v2", count, maxNetPolicyEntries); err != nil {
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
// entries for allow_out_v2 and DNS names for dns_allow.
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
		if !isDNSAllowTarget(target) {
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

// populateInnerMap parses the given CIDR list and inserts each entry
// into the inner LPM trie map for the specified ifindex.
func populateInnerMap(outerMap *ebpf.Map, ifindex uint32, cidrs []string) error {
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err != nil {
		return fmt.Errorf("map.Lookup failed: %w", err)
	}

	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	val := uint32(netPolicyValueStatic)
	for _, cidr := range cidrs {
		key, err := parseCIDR(cidr)
		if err != nil {
			return err
		}
		err = inner.Update(&key, &val, ebpf.UpdateAny)
		if err != nil {
			return fmt.Errorf("inner map update failed: %w, cidr: %s", err, cidr)
		}
	}
	return nil
}

// populateAllowOutInnerMap parses the given CIDR list and inserts static allow entries.
func populateAllowOutInnerMap(outerMap *ebpf.Map, ifindex uint32, cidrs []string, flags uint8) error {
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err != nil {
		return fmt.Errorf("map.Lookup failed: %w", err)
	}

	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	for _, cidr := range cidrs {
		key, err := parseCIDR(cidr)
		if err != nil {
			return err
		}

		val := netPolicyValueV2{Flags: flags}
		var oldVal netPolicyValueV2
		if err := inner.Lookup(&key, &oldVal); err == nil {
			// Static allow entries never expire, but they must preserve existing flags.
			val.Flags |= oldVal.Flags
		} else if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("inner map lookup failed: %w, cidr: %s", err, cidr)
		}

		err = inner.Update(&key, &val, ebpf.UpdateAny)
		if err != nil {
			return fmt.Errorf("inner map update failed: %w, cidr: %s", err, cidr)
		}
	}
	return nil
}

// netPolicyValueV2Expired reports whether a v2 allow entry is a dynamic entry
// whose DNS-learned TTL has expired. Static entries have ExpiresAtNS set to 0.
func netPolicyValueV2Expired(value netPolicyValueV2, now uint64) bool {
	return value.ExpiresAtNS != 0 && value.ExpiresAtNS <= now
}

// applyNetPolicy configures egress network policy for the given ifindex
// based on MVMOptions.
//
// Rules:
//   - AllowOut IP/CIDR targets are inserted into allow_out_v2 inner map.
//   - L7AllowOut IP/CIDR targets are inserted into allow_out_v2 with the L7 flag.
//   - AllowOut domain targets are inserted into dns_allow inner map.
//   - L7AllowOut domain targets are inserted into dns_allow with the L7 flag.
//   - DenyOut always includes alwaysDeniedSandboxCIDRs.
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
	// Process allowOut.
	var allowOut []string
	if opts.AllowOut != nil {
		allowOut = *opts.AllowOut
	}
	allowOutCIDRs, dnsAllowDomains, err := splitAllowOutTargets(allowOut)
	if err != nil {
		return err
	}

	var l7AllowOut []string
	if opts.L7AllowOut != nil {
		l7AllowOut = *opts.L7AllowOut
	}
	l7AllowOutCIDRs, l7DNSAllowDomains, err := splitAllowOutTargets(l7AllowOut)
	if err != nil {
		return err
	}

	// Process denyOut: always append alwaysDeniedSandboxCIDRs.
	// If AllowInternetAccess is false, deny all outbound traffic.
	var denyOut []string
	if opts.AllowInternetAccess != nil && !*opts.AllowInternetAccess {
		denyOut = []string{"0.0.0.0/0"}
	} else {
		if opts.DenyOut != nil {
			denyOut = append(*opts.DenyOut, alwaysDeniedSandboxCIDRs...)
		} else {
			denyOut = append(denyOut, alwaysDeniedSandboxCIDRs...)
		}
	}

	if err := validateNetPolicyEntryCounts(allowOutCIDRs, l7AllowOutCIDRs, dnsAllowDomains, l7DNSAllowDomains, denyOut); err != nil {
		return err
	}

	dnsPolicyModeFlags := dnsPolicyFlagsForDomains(dnsAllowDomains, l7DNSAllowDomains)

	if replace || len(allowOutCIDRs)+len(l7AllowOutCIDRs) > 0 {
		allowOutMap, err := loadPinnedMap(MapNameAllowOutV2)
		if err != nil {
			return err
		}
		defer allowOutMap.Close()

		if err := ensureAllowOutV2InnerMap(allowOutMap, ifindex); err != nil {
			return err
		}
		if replace {
			if err := flushAllowOutInnerMap(allowOutMap, ifindex); err != nil {
				return fmt.Errorf("flush %s failed: %w", MapNameAllowOutV2, err)
			}
		}
		err = populateAllowOutInnerMap(allowOutMap, ifindex, allowOutCIDRs, 0)
		if err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameAllowOutV2, err)
		}
		err = populateAllowOutInnerMap(allowOutMap, ifindex, l7AllowOutCIDRs, uint8(netPolicyFlagL7Required))
		if err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameAllowOutV2, err)
		}
	}
	if err := applyDNSAllow(ifindex, dnsAllowDomains, l7DNSAllowDomains); err != nil {
		return fmt.Errorf("populate %s failed: %w", MapNameDNSAllow, err)
	}

	if replace || len(denyOut) > 0 {
		denyOutMap, err := loadPinnedMap(MapNameDenyOut)
		if err != nil {
			return err
		}
		defer denyOutMap.Close()

		if err := ensureDenyOutInnerMap(denyOutMap, ifindex); err != nil {
			return err
		}
		if replace {
			if err := flushInnerMap(denyOutMap, ifindex); err != nil {
				return fmt.Errorf("flush %s failed: %w", MapNameDenyOut, err)
			}
		}
		err = populateInnerMap(denyOutMap, ifindex, denyOut)
		if err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameDenyOut, err)
		}
	}

	return setDNSPolicyFlags(ifindex, dnsPolicyModeFlags)
}
