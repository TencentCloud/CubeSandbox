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
