package cubevs

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const maxDNSAllowDomains = maxDNSAllowEntries

// newInnerDNSAllowMap creates the LPM trie inner map consumed by dns_allow[ifindex].
func newInnerDNSAllowMap() (*ebpf.Map, error) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(dnsAllowKey{})),
		ValueSize:  uint32(unsafe.Sizeof(dnsAllowValue{})),
		MaxEntries: maxDNSAllowEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
		Key:        btfTypeDNSAllowKey,
		Value:      btfTypeDNSAllowValue,
	})
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMap(LPMTrie) failed: %w", err)
	}
	return m, nil
}

// ensureDNSAllowInnerMap creates the per-sandbox DNS allow map when it is absent.
func ensureDNSAllowInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	return ensureInnerMapWithFactory(outerMap, ifindex, MapNameDNSAllowV2, newInnerDNSAllowMap)
}

func initDNSAllow(ifindex uint32) error {
	dnsAllow, err := loadPinnedMap(MapNameDNSAllowV2)
	if err != nil {
		return err
	}
	defer dnsAllow.Close()

	return ensureDNSAllowInnerMap(dnsAllow, ifindex)
}

// makeDNSAllowRule encodes a domain into the reversed LPM-trie key used by eBPF.
// Exact rules end with '\0' ("qq.com" -> "moc.qq\0"), while wildcard rules
// strip "*." and end with '.' ("*.qq.com" -> "moc.qq.") so they only match
// subdomains such as "a.qq.com", not the apex domain "qq.com".
func makeDNSAllowRule(domain string, flags uint8) (dnsAllowKey, dnsAllowValue, error) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if len(domain) == 0 {
		return dnsAllowKey{}, dnsAllowValue{}, fmt.Errorf("invalid DNS allow domain length: %s", domain) //nolint:err113
	}

	value := dnsAllowValue{Flags: flags}
	wildcard := false
	// Only a leading "*." wildcard is supported, matching subdomains only.
	if strings.Contains(domain, "*") {
		if strings.Count(domain, "*") != 1 || !strings.HasPrefix(domain, "*.") || len(domain) <= len("*.") {
			return dnsAllowKey{}, dnsAllowValue{}, fmt.Errorf("invalid DNS allow wildcard domain: %s", domain) //nolint:err113
		}
		domain = domain[2:]
		wildcard = true
	}
	if len(domain) == 0 || len(domain) >= maxDNSNameLen-1 {
		return dnsAllowKey{}, dnsAllowValue{}, fmt.Errorf("invalid DNS allow domain length: %s", domain) //nolint:err113
	}

	var key dnsAllowKey
	// Reverse the domain so LPM lookup can match suffixes as prefixes.
	for i := 0; i < len(domain); i++ {
		key.Name[i] = domain[len(domain)-1-i]
	}
	if wildcard {
		key.Name[len(domain)] = '.'
	} else {
		key.Name[len(domain)] = 0
	}
	value.NameLen = uint32(len(domain) + 1)
	key.Prefixlen = value.NameLen * 8
	return key, value, nil
}

type dnsAllowRule struct {
	key    dnsAllowKey
	value  dnsAllowValue
	domain string
}

// IsDNSCatchAll reports whether a host or SNI is the standalone "*" token.
// Comparison matches the datapath: trim, drop one trailing dot, lowercase.
// "*.example.com" is not a catch-all.
func IsDNSCatchAll(raw string) bool {
	token := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	return token == "*"
}

// makeCatchAllDNSAllowRule builds the single dns_allow_v2 entry for "*".
// prefixlen 0 is the shortest LPM key, so every name matches it unless a
// longer exact or "*." rule wins. An empty ports slice keeps port_count at 0,
// which the datapath expands to {80/http, 443/https}. A non-empty slice writes
// those tuples, the same way a per-host L7 rule does. The rule is L7-only: no
// L3_ALLOWED bit, so learning does not open ports outside this set.
func makeCatchAllDNSAllowRule(ports []DNSCatchAllPort) (dnsAllowRule, error) {
	if len(ports) > maxL7PortsPerHost {
		return dnsAllowRule{}, fmt.Errorf("catch-all exceeds %d L7 port tuples", maxL7PortsPerHost) //nolint:err113
	}
	value := dnsAllowValue{
		NameLen: 1,
		Flags:   uint8(netPolicyFlagL7Required),
	}
	if len(ports) == 0 {
		return dnsAllowRule{value: value, domain: "*"}, nil
	}
	entries := make([]l7PortEntry, 0, len(ports))
	seen := make(map[uint16]uint8, len(ports))
	for _, p := range ports {
		if p.Port == 0 || (p.Scheme != L7SchemeHTTP && p.Scheme != L7SchemeHTTPS) {
			return dnsAllowRule{}, fmt.Errorf("invalid catch-all port %d scheme %d", p.Port, p.Scheme) //nolint:err113
		}
		if existing, ok := seen[p.Port]; ok && existing != p.Scheme {
			return dnsAllowRule{}, fmt.Errorf("catch-all port %d scheme conflict", p.Port) //nolint:err113
		}
		if _, ok := seen[p.Port]; ok {
			continue
		}
		seen[p.Port] = p.Scheme
		entries = append(entries, l7PortEntry{
			Port:   htonsPort(p.Port),
			Scheme: p.Scheme,
		})
	}
	applyPortsToDNSAllowValue(&value, entries)
	return dnsAllowRule{value: value, domain: "*"}, nil
}

// dnsAllowRuleForDomain encodes one persisted domain rule into the dns_allow_v2
// key and value the datapath matches. "*" goes through the catch-all encoder;
// every other pattern goes through makeDNSAllowRule. Both writers of the map
// (desired state and the diff) must use this so a catch-all row is not dropped
// on the way from the plan to the trie.
func dnsAllowRuleForDomain(domain string, flags uint8, ports []L7Port) (dnsAllowKey, dnsAllowValue, error) {
	if IsDNSCatchAll(domain) {
		cp := make([]DNSCatchAllPort, len(ports))
		for i, p := range ports {
			cp[i] = DNSCatchAllPort{Port: p.Port, Scheme: p.Scheme}
		}
		rule, err := makeCatchAllDNSAllowRule(cp)
		if err != nil {
			return dnsAllowKey{}, dnsAllowValue{}, err
		}
		// Keep the flags the plan stored. makeCatchAllDNSAllowRule always
		// writes L7_REQUIRED, which is what a fresh catch-all carries; a
		// snapshot must round-trip whatever flags it actually persisted.
		rule.value.Flags = flags
		return rule.key, rule.value, nil
	}
	key, value, err := makeDNSAllowRule(domain, flags)
	if err != nil {
		return dnsAllowKey{}, dnsAllowValue{}, err
	}
	applyPortsToDNSAllowValue(&value, portsToL7Entries(ports))
	return key, value, nil
}

func buildDNSAllowRules(domains []string) ([]dnsAllowRule, error) {
	rules := make([]dnsAllowRule, 0, len(domains))
	indexByKey := make(map[dnsAllowKey]int, len(domains))

	for _, domain := range domains {
		key, value, err := makeDNSAllowRule(domain, 0)
		if err != nil {
			return nil, err
		}
		if idx, ok := indexByKey[key]; ok {
			rules[idx].value.Flags |= value.Flags
			continue
		}
		indexByKey[key] = len(rules)
		rules = append(rules, dnsAllowRule{
			key:    key,
			value:  value,
			domain: domain,
		})
	}
	return rules, nil
}

func flushDNSAllowInnerMap(inner *ebpf.Map) error {
	return flushInnerEntries[dnsAllowKey, dnsAllowValue](inner)
}

// cleanupDNSAllow clears the sandbox DNS allow inner map while keeping it preallocated.
func cleanupDNSAllow(ifindex uint32) error {
	dnsAllow, err := loadPinnedMap(MapNameDNSAllowV2)
	if err != nil {
		return err
	}
	defer dnsAllow.Close()

	inner, err := lookupInnerMap(dnsAllow, ifindex, MapNameDNSAllowV2)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return err
	}
	return flushDNSAllowInnerMap(inner)
}
