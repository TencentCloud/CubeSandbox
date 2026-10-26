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

// populateDNSAllowInnerMap installs DNS allow entries for a sandbox.
func populateDNSAllowInnerMap(inner *ebpf.Map, rules []dnsAllowRule) error {
	for _, rule := range rules {
		if err := updateDNSAllowRule(inner, rule); err != nil {
			return err
		}
	}
	return nil
}

func flushDNSAllowForIfindex(outerMap *ebpf.Map, ifindex uint32) error {
	inner, err := lookupInnerMap(outerMap, ifindex, MapNameDNSAllowV2)
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return flushDNSAllowInnerMap(inner)
}

func updateDNSAllowRule(inner *ebpf.Map, rule dnsAllowRule) error {
	value := rule.value
	var oldValue dnsAllowValue
	if err := inner.Lookup(&rule.key, &oldValue); err == nil {
		value.Flags |= oldValue.Flags
		// Preserve any port tuples already installed for this key. Rule
		// order should not cause a later rule with a subset of the port
		// set to clobber an earlier rule's ports. buildL7Plan is the
		// single point where scheme conflicts are detected, so any port
		// present in both rules must agree on scheme by construction.
		mergePortsIntoDNSValue(&value, oldValue.Ports[:oldValue.PortCount])
	} else if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("dns allow lookup failed: %w, domain: %s", err, rule.domain)
	}

	if err := inner.Update(&rule.key, &value, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("dns allow update failed: %w, domain: %s", err, rule.domain)
	}
	return nil
}

// mergePortsIntoDNSValue unions src into v.Ports without exceeding
// maxL7PortsPerHost. Silently drops overflow — buildL7Plan already enforces
// the budget in userspace.
func mergePortsIntoDNSValue(v *dnsAllowValue, src []l7PortEntry) {
	for _, p := range src {
		exists := false
		for i := uint8(0); i < v.PortCount; i++ {
			if v.Ports[i].Port == p.Port {
				exists = true
				break
			}
		}
		if exists || v.PortCount >= maxL7PortsPerHost {
			continue
		}
		v.Ports[v.PortCount] = p
		v.PortCount++
	}
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

// syncDNSAllowInner converges dns_allow_v2 for one TAP on the desired rules.
//
// Values are written as-is rather than through updateDNSAllowRule: that helper
// unions the installed flags and port tuples into the new value, which is right
// when several rules of one apply share a key, but on an update it would keep
// ports the caller just removed. buildNetPolicyPlan already merges same-key
// rules in userspace, so rules holds exactly the desired state.
func syncDNSAllowInner(ifindex uint32, rules []dnsAllowRule) error {
	dnsAllow, err := loadPinnedMap(MapNameDNSAllowV2)
	if err != nil {
		return err
	}
	defer dnsAllow.Close()

	inner, err := acquireInnerMap(dnsAllow, ifindex, MapNameDNSAllowV2, newInnerDNSAllowMap)
	if err != nil {
		return err
	}

	desired := make(map[dnsAllowKey]struct{}, len(rules))
	for _, rule := range rules {
		desired[rule.key] = struct{}{}
	}

	stale, err := staleKeys(inner, desired, func(*dnsAllowValue) bool { return true })
	if err != nil {
		return err
	}
	if err := deleteKeys(inner, stale); err != nil {
		return err
	}

	for _, rule := range rules {
		if err := inner.Update(&rule.key, &rule.value, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("dns allow update failed: %w, domain: %s", err, rule.domain)
		}
	}
	return nil
}

// applyDNSAllow installs DNS allow rules parsed from MVMOptions.
func applyDNSAllow(ifindex uint32, rules []dnsAllowRule, replace bool) error {
	if len(rules) == 0 && !replace {
		return nil
	}

	dnsAllow, err := loadPinnedMap(MapNameDNSAllowV2)
	if err != nil {
		return err
	}
	defer dnsAllow.Close()

	if len(rules) == 0 {
		return flushDNSAllowForIfindex(dnsAllow, ifindex)
	}

	inner, err := acquireInnerMap(dnsAllow, ifindex, MapNameDNSAllowV2, newInnerDNSAllowMap)
	if err != nil {
		return err
	}
	if replace {
		if err := flushDNSAllowInnerMap(inner); err != nil {
			return err
		}
	}
	return populateDNSAllowInnerMap(inner, rules)
}
