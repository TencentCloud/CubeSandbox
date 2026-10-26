package cubevs

import (
	"errors"
	"fmt"
	"log"
	"os"
	"unsafe"

	"github.com/cilium/ebpf"
)

const legacyAllowOutValueSize = uint32(4)

// skipUnsupportedL7Subnet reports whether a legacy allow entry is an L7 rule
// keyed by a subnet (prefixlen<32). v3 cannot express subnet+port in a single
// LPM key (its /48 key matches exact (ip, port) pairs), and new L7 rules now
// reject subnet hosts outright (classifyL7Target). Carrying such a legacy rule
// forward would silently narrow it to the network address, so it is dropped
// here with a warning instead; the operator should recreate it as /32 host or
// domain rules.
func skipUnsupportedL7Subnet(key lpmKey, flags uint8, sourceName string) bool {
	if flags&netPolicyFlagL7Required == 0 || key.Prefixlen >= 32 {
		return false
	}
	log.Printf("cubevs migration: dropping unsupported L7 subnet rule %s/%d from %s: v3 cannot express subnet+port; recreate as /32 host or domain rules",
		uint32ToIP(key.IP).String(), key.Prefixlen, sourceName)
	return true
}

type legacyDNSAllowValue struct {
	NameLen  uint32
	Flags    uint8
	Reserved [3]uint8
}

// allowOutV3Entry is one (key, value) pair in the new allow_out_v3
// layout, produced by expanding an older allow_out entry.
type allowOutV3Entry struct {
	key   lpmKeyV3
	value netPolicyValueV3
}

// buildV3Entries expands one legacy (ip CIDR, flags, ports, expires) into
// the new allow_out_v3 entries:
//   - L7 flag set: one exact (ip, port)/48 entry per (port, scheme);
//     a default port set (port_count == 0) expands to {80/http, 443/https}.
//   - otherwise:     one ip-only (or subnet) /32 entry, scheme = NONE.
//
// The legacy net_policy_value_v2 ports[] (if any) are passed by the caller
// as the ports slice; a nil slice triggers the default-set expansion.
func buildV3Entries(ipKey lpmKey, flags uint8, ports []l7PortEntry, expires uint64) []allowOutV3Entry {
	if flags&netPolicyFlagL7Required != 0 {
		if len(ports) == 0 {
			ports = expandDefaultPortSet()
		}
		out := make([]allowOutV3Entry, 0, len(ports))
		for _, p := range ports {
			out = append(out, allowOutV3Entry{
				key:   lpmKeyV3{Prefixlen: 48, IP: ipKey.IP, Port: p.Port},
				value: netPolicyValueV3{Flags: flags, Scheme: p.Scheme, ExpiresAtNS: expires, KeyPrefixlen: 48},
			})
		}
		return out
	}
	return []allowOutV3Entry{{
		key:   lpmKeyV3{Prefixlen: ipKey.Prefixlen, IP: ipKey.IP, Port: 0},
		value: netPolicyValueV3{Flags: flags, ExpiresAtNS: expires, KeyPrefixlen: uint8(ipKey.Prefixlen)},
	}}
}

// applyV3Entries writes expanded entries into a v3 inner LPM trie.
func applyV3Entries(dest *ebpf.Map, entries []allowOutV3Entry) error {
	for _, e := range entries {
		if err := dest.Update(&e.key, &e.value, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update %s inner map failed: %w", MapNameAllowOutV3, err)
		}
	}
	return nil
}

func pinnedMapExists(name string) (bool, error) {
	_, err := os.Stat(pinPath(name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// migratePersistentPolicyMaps performs a one-way migration into the current
// generation. Both legacy maps are migrated first; the legacy pins are only
// unlinked after both have been copied into the current maps. This ordering
// matters for rollback: Init removes the freshly created allow_out_v3 /
// dns_allow_v2 pins when migration fails, so if the allow pin were unlinked
// before the DNS migration ran, a DNS failure would leave the allow_out
// policy unmigratable (legacy pin gone, new pin rolled back). The current
// datapath and userspace only read allow_out_v3 and dns_allow_v2.
func migratePersistentPolicyMaps() error {
	allowSrc, err := allowOutMigrationSource()
	if err != nil {
		return err
	}
	if allowSrc != "" {
		if err := migrateAllowOutMap(allowSrc); err != nil {
			// Leave the legacy pin in place so the migration can be
			// retried on the next restart.
			return err
		}
	}

	if err := migrateDNSAllowMap(MapNameDNSAllow); err != nil {
		// Leave the legacy allow pin in place too, so both migrations are
		// retried together on the next restart (see function comment).
		return err
	}

	// Both migrations succeeded, so the current maps now hold the policy and
	// dropping the legacy pins is pure cleanup. Make it best-effort: a failed
	// unlink must not fail Init (which would roll back the freshly migrated
	// maps and lose the policy); a lingering pin is simply re-migrated
	// (idempotently) on the next restart.
	if allowSrc != "" {
		if err := removePinnedMap(allowSrc); err != nil {
			log.Printf("cubevs migration: leaving legacy pin %s after successful migration: %v", allowSrc, err)
		}
	}
	if err := removePinnedMap(MapNameDNSAllow); err != nil {
		log.Printf("cubevs migration: leaving legacy pin %s after successful migration: %v", MapNameDNSAllow, err)
	}
	return nil
}

// allowOutMigrationSource returns the name of the legacy allow_out pin to
// migrate from, preferring allow_out_v2 and falling back to the v0.2.0
// allow_out pin. It returns "" when no legacy pin is present.
func allowOutMigrationSource() (string, error) {
	v2Exists, err := pinnedMapExists(MapNameAllowOutV2)
	if err != nil {
		return "", err
	}
	if v2Exists {
		return MapNameAllowOutV2, nil
	}
	legacyExists, err := pinnedMapExists(MapNameAllowOut)
	if err != nil {
		return "", err
	}
	if legacyExists {
		return MapNameAllowOut, nil
	}
	return "", nil
}

// removePinnedMap unlinks a bpffs pin. It is safe to call for a map whose
// inner maps are not separately pinned (they live only inside the outer
// hash-of-maps and are released by the kernel once the outer pin is gone),
// so a single-file unlink suffices. A missing pin is treated as success.
func removePinnedMap(name string) error {
	if err := os.Remove(pinPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove legacy pin %s failed: %w", name, err)
	}
	return nil
}

func migrateAllowOutMap(sourceName string) error {
	source, err := ebpf.LoadPinnedMap(pinPath(sourceName), nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load legacy %s failed: %w", sourceName, err)
	}
	defer source.Close()

	if err := verifyMapLayout(source, sourceName, ebpf.HashOfMaps, uint32(unsafe.Sizeof(uint32(0))), uint32(unsafe.Sizeof(uint32(0)))); err != nil {
		return err
	}
	current, err := loadPinnedMap(MapNameAllowOutV3)
	if err != nil {
		return err
	}
	defer current.Close()

	var ifindex uint32
	var innerMapID uint32
	iter := source.Iterate()
	for iter.Next(&ifindex, &innerMapID) {
		if err := migrateAllowOutInnerMap(current, ifindex, sourceName, ebpf.MapID(innerMapID)); err != nil {
			return err
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate legacy %s failed: %w", sourceName, err)
	}
	return nil
}

func migrateAllowOutInnerMap(current *ebpf.Map, ifindex uint32, sourceName string, sourceInnerID ebpf.MapID) error {
	source, err := ebpf.NewMapFromID(sourceInnerID)
	if err != nil {
		return fmt.Errorf("open legacy %s inner map failed: %w, id: %d", sourceName, err, sourceInnerID)
	}
	defer source.Close()
	info, err := source.Info()
	if err != nil {
		return fmt.Errorf("get legacy %s inner map info failed: %w", sourceName, err)
	}
	if info.Type != ebpf.LPMTrie || info.KeySize != uint32(unsafe.Sizeof(lpmKey{})) {
		return fmt.Errorf("%s inner map has incompatible ABI: type=%s key_size=%d", sourceName, info.Type, info.KeySize) //nolint:err113
	}
	if info.ValueSize != legacyAllowOutValueSize &&
		info.ValueSize != uint32(unsafe.Sizeof(netPolicyValueV2{})) {
		return fmt.Errorf("%s inner map has unsupported value_size=%d", sourceName, info.ValueSize) //nolint:err113
	}

	if err := ensureAllowOutV3InnerMap(current, ifindex); err != nil {
		return err
	}
	destination, err := lookupInnerMap(current, ifindex, MapNameAllowOutV3)
	if err != nil {
		return err
	}

	switch info.ValueSize {
	case legacyAllowOutValueSize:
		// v0.2.0 static allow marker: plain /32, no L7.
		var key lpmKey
		var oldValue uint32
		iter := source.Iterate()
		for iter.Next(&key, &oldValue) {
			if err := applyV3Entries(destination, buildV3Entries(key, 0, nil, 0)); err != nil {
				return fmt.Errorf("update %s inner map failed: %w", MapNameAllowOutV3, err)
			}
		}
		return wrapIterErr(iter.Err(), sourceName)
	case uint32(unsafe.Sizeof(netPolicyValueV2{})):
		// 16-byte legacy net_policy_value_v2: flags + expires, no ports.
		var key lpmKey
		var oldValue netPolicyValueV2
		iter := source.Iterate()
		for iter.Next(&key, &oldValue) {
			if skipUnsupportedL7Subnet(key, oldValue.Flags, sourceName) {
				continue
			}
			if err := applyV3Entries(destination, buildV3Entries(key, oldValue.Flags, nil, oldValue.ExpiresAtNS)); err != nil {
				return fmt.Errorf("update %s inner map failed: %w", MapNameAllowOutV3, err)
			}
		}
		return wrapIterErr(iter.Err(), sourceName)
	default:
		return fmt.Errorf("%s inner map has unsupported value_size=%d", sourceName, info.ValueSize) //nolint:err113
	}
}

func migrateDNSAllowMap(sourceName string) error {
	source, err := ebpf.LoadPinnedMap(pinPath(sourceName), nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load legacy %s failed: %w", sourceName, err)
	}
	defer source.Close()
	if err := verifyMapLayout(source, sourceName, ebpf.HashOfMaps, uint32(unsafe.Sizeof(uint32(0))), uint32(unsafe.Sizeof(uint32(0)))); err != nil {
		return err
	}
	current, err := loadPinnedMap(MapNameDNSAllowV2)
	if err != nil {
		return err
	}
	defer current.Close()

	var ifindex uint32
	var innerMapID uint32
	iter := source.Iterate()
	for iter.Next(&ifindex, &innerMapID) {
		if err := migrateDNSAllowInnerMap(current, ifindex, sourceName, ebpf.MapID(innerMapID)); err != nil {
			return err
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate legacy %s failed: %w", sourceName, err)
	}
	return nil
}

func migrateDNSAllowInnerMap(current *ebpf.Map, ifindex uint32, sourceName string, sourceInnerID ebpf.MapID) error {
	source, err := ebpf.NewMapFromID(sourceInnerID)
	if err != nil {
		return fmt.Errorf("open legacy %s inner map failed: %w, id: %d", sourceName, err, sourceInnerID)
	}
	defer source.Close()
	info, err := source.Info()
	if err != nil {
		return err
	}
	if info.Type != ebpf.LPMTrie || info.KeySize != uint32(unsafe.Sizeof(dnsAllowKey{})) {
		return fmt.Errorf("%s inner map has incompatible ABI: type=%s key_size=%d", sourceName, info.Type, info.KeySize) //nolint:err113
	}
	legacySize := uint32(unsafe.Sizeof(legacyDNSAllowValue{}))
	currentSize := uint32(unsafe.Sizeof(dnsAllowValue{}))
	if info.ValueSize != legacySize && info.ValueSize != currentSize {
		return fmt.Errorf("%s inner map has unsupported value_size=%d", sourceName, info.ValueSize) //nolint:err113
	}

	if err := ensureDNSAllowInnerMap(current, ifindex); err != nil {
		return err
	}
	destination, err := lookupInnerMap(current, ifindex, MapNameDNSAllowV2)
	if err != nil {
		return err
	}

	if info.ValueSize == legacySize {
		var key dnsAllowKey
		var oldValue legacyDNSAllowValue
		iter := source.Iterate()
		for iter.Next(&key, &oldValue) {
			value := dnsAllowValue{NameLen: oldValue.NameLen, Flags: oldValue.Flags}
			if err := destination.Update(&key, &value, ebpf.UpdateAny); err != nil {
				return fmt.Errorf("update %s inner map failed: %w", MapNameDNSAllowV2, err)
			}
		}
		return wrapIterErr(iter.Err(), sourceName)
	}

	var key dnsAllowKey
	var value dnsAllowValue
	iter := source.Iterate()
	for iter.Next(&key, &value) {
		if err := destination.Update(&key, &value, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update %s inner map failed: %w", MapNameDNSAllowV2, err)
		}
	}
	return wrapIterErr(iter.Err(), sourceName)
}

func verifyMapLayout(m *ebpf.Map, name string, wantType ebpf.MapType, wantKeySize, wantValueSize uint32) error {
	info, err := m.Info()
	if err != nil {
		return fmt.Errorf("get %s map info failed: %w", name, err)
	}
	if info.Type != wantType || info.KeySize != wantKeySize || info.ValueSize != wantValueSize {
		return fmt.Errorf("%s map has incompatible ABI: type=%s key_size=%d value_size=%d, want type=%s key_size=%d value_size=%d", name, info.Type, info.KeySize, info.ValueSize, wantType, wantKeySize, wantValueSize) //nolint:err113
	}
	return nil
}
