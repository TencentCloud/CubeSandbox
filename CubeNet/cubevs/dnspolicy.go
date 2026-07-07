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
	return ensureInnerMapWithFactory(outerMap, ifindex, MapNameDNSAllow, newInnerDNSAllowMap)
}

func initDNSAllow(ifindex uint32) error {
	dnsAllow, err := loadPinnedMap(MapNameDNSAllow)
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

func buildDNSAllowRules(domains, l7Domains []string) ([]dnsAllowRule, error) {
	rules := make([]dnsAllowRule, 0, len(domains)+len(l7Domains))
	indexByKey := make(map[dnsAllowKey]int, len(domains)+len(l7Domains))

	add := func(domains []string, flags uint8) error {
		for _, domain := range domains {
			key, value, err := makeDNSAllowRule(domain, flags)
			if err != nil {
				return err
			}
			if idx, ok := indexByKey[key]; ok {
				rules[idx].value.Flags |= flags
				continue
			}
			indexByKey[key] = len(rules)
			rules = append(rules, dnsAllowRule{
				key:    key,
				value:  value,
				domain: domain,
			})
		}
		return nil
	}

	if err := add(domains, 0); err != nil {
		return nil, err
	}
	if err := add(l7Domains, uint8(netPolicyFlagL7Required)); err != nil {
		return nil, err
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
	inner, err := lookupInnerMap(outerMap, ifindex)
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer inner.Close()

	return flushDNSAllowInnerMap(inner)
}

func updateDNSAllowRule(inner *ebpf.Map, rule dnsAllowRule) error {
	value := rule.value
	var oldValue dnsAllowValue
	if err := inner.Lookup(&rule.key, &oldValue); err == nil {
		value.Flags |= oldValue.Flags
	} else if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("dns allow lookup failed: %w, domain: %s", err, rule.domain)
	}

	if err := inner.Update(&rule.key, &value, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("dns allow update failed: %w, domain: %s", err, rule.domain)
	}
	return nil
}

func flushDNSAllowInnerMap(inner *ebpf.Map) error {
	var oldKey dnsAllowKey
	var oldValue dnsAllowValue
	iter := inner.Iterate()
	for iter.Next(&oldKey, &oldValue) {
		if err := inner.Delete(&oldKey); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("dns allow delete failed: %w", err)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("dns allow iterate failed: %w", err)
	}
	return nil
}

// cleanupDNSAllow clears the sandbox DNS allow inner map while keeping it preallocated.
func cleanupDNSAllow(ifindex uint32) error {
	dnsAllow, err := loadPinnedMap(MapNameDNSAllow)
	if err != nil {
		return err
	}
	defer dnsAllow.Close()

	inner, err := lookupInnerMap(dnsAllow, ifindex)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return err
	}
	defer inner.Close()

	return flushDNSAllowInnerMap(inner)
}

// applyDNSAllow installs DNS allow rules parsed from MVMOptions.
func applyDNSAllow(ifindex uint32, rules []dnsAllowRule, replace bool) error {
	if len(rules) == 0 && !replace {
		return nil
	}

	dnsAllow, err := loadPinnedMap(MapNameDNSAllow)
	if err != nil {
		return err
	}
	defer dnsAllow.Close()

	if len(rules) == 0 {
		return flushDNSAllowForIfindex(dnsAllow, ifindex)
	}
	if err := ensureDNSAllowInnerMap(dnsAllow, ifindex); err != nil {
		return err
	}

	inner, err := lookupInnerMap(dnsAllow, ifindex)
	if err != nil {
		return err
	}
	defer inner.Close()

	if replace {
		if err := flushDNSAllowInnerMap(inner); err != nil {
			return err
		}
	}
	return populateDNSAllowInnerMap(inner, rules)
}
