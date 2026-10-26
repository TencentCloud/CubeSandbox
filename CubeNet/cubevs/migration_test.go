package cubevs

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

func TestSkipUnsupportedL7Subnet(t *testing.T) {
	ip := uint32(0x0a000000) // 10.0.0.0
	tests := []struct {
		name      string
		prefixlen uint32
		flags     uint8
		want      bool
	}{
		{name: "L7 subnet dropped", prefixlen: 24, flags: uint8(netPolicyFlagL7Required), want: true},
		{name: "L7 /32 host kept", prefixlen: 32, flags: uint8(netPolicyFlagL7Required), want: false},
		{name: "plain subnet kept (non-L7)", prefixlen: 24, flags: 0, want: false},
		{name: "plain /32 kept (non-L7)", prefixlen: 32, flags: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := lpmKey{Prefixlen: tt.prefixlen, IP: ip}
			if got := skipUnsupportedL7Subnet(key, tt.flags, "test"); got != tt.want {
				t.Fatalf("skipUnsupportedL7Subnet(prefixlen=%d flags=%#x)=%v, want %v",
					tt.prefixlen, tt.flags, got, tt.want)
			}
		})
	}
}

// newDNSAllowOuterMapWithValueSize creates a dns_allow outer hash-of-maps whose
// inner LPM-trie template carries values of the given size (8-byte legacy or
// 40-byte current).
func newDNSAllowOuterMapWithValueSize(t *testing.T, valueSize uint32) *ebpf.Map {
	t.Helper()
	outer, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.HashOfMaps,
		KeySize:    uint32(unsafe.Sizeof(uint32(0))),
		ValueSize:  uint32(unsafe.Sizeof(uint32(0))),
		MaxEntries: 1024,
		InnerMap: &ebpf.MapSpec{
			Type:       ebpf.LPMTrie,
			KeySize:    uint32(unsafe.Sizeof(dnsAllowKey{})),
			ValueSize:  valueSize,
			MaxEntries: maxDNSAllowEntries,
			Flags:      unix.BPF_F_NO_PREALLOC,
		},
	})
	if err != nil {
		t.Fatalf("create dns_allow outer map (value_size=%d): %v", valueSize, err)
	}
	t.Cleanup(func() { outer.Close() })
	return outer
}

// newDNSAllowOuterMap creates the dns_allow_v2 outer hash-of-maps with an inner
// LPM-trie template matching the current (40-byte) dns_allow_value.
func newDNSAllowOuterMap(t *testing.T) *ebpf.Map {
	return newDNSAllowOuterMapWithValueSize(t, uint32(unsafe.Sizeof(dnsAllowValue{})))
}

// newLPMInner creates a standalone LPM-trie inner map with the given value size.
func newLPMInner(t *testing.T, valueSize uint32) *ebpf.Map {
	t.Helper()
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(dnsAllowKey{})),
		ValueSize:  valueSize,
		MaxEntries: maxDNSAllowEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
	})
	if err != nil {
		t.Fatalf("create LPM inner (value_size=%d): %v", valueSize, err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func mustInnerMapID(t *testing.T, m *ebpf.Map) ebpf.MapID {
	t.Helper()
	info, err := m.Info()
	if err != nil {
		t.Fatalf("get inner map info: %v", err)
	}
	id, ok := info.ID()
	if !ok {
		t.Fatal("inner map has no id")
	}
	return id
}

// TestMigrateDNSAllowInnerMapFromLegacy drives the legacy (8-byte) dns_allow
// inner-map migration: a legacy value (NameLen + Flags, no ports) must land in
// the new dns_allow_v2 inner map as a 40-byte value with the same NameLen and
// Flags and PortCount == 0 (default 80/443 at match time).
func TestMigrateDNSAllowInnerMapFromLegacy(t *testing.T) {
	ifindex := uint32(42)
	current := newDNSAllowOuterMap(t)

	// Seed a legacy (8-byte) inner map with an L7 rule and a plain rule.
	legacy := newLPMInner(t, uint32(unsafe.Sizeof(legacyDNSAllowValue{})))
	type entry struct {
		domain string
		flags  uint8
	}
	entries := []entry{
		{"api.example.com", uint8(netPolicyFlagL7Required)},
		{"static.example.com", 0},
	}
	for _, e := range entries {
		key, val, err := makeDNSAllowRule(e.domain, e.flags)
		if err != nil {
			t.Fatalf("makeDNSAllowRule %s: %v", e.domain, err)
		}
		lv := legacyDNSAllowValue{NameLen: val.NameLen, Flags: val.Flags}
		if err := legacy.Update(&key, &lv, ebpf.UpdateAny); err != nil {
			t.Fatalf("seed legacy entry %s: %v", e.domain, err)
		}
	}

	if err := migrateDNSAllowInnerMap(current, ifindex, "test-legacy", mustInnerMapID(t, legacy)); err != nil {
		t.Fatalf("migrateDNSAllowInnerMap: %v", err)
	}

	dest, err := lookupInnerMap(current, ifindex, MapNameDNSAllowV2)
	if err != nil {
		t.Fatalf("lookupInnerMap: %v", err)
	}

	for _, e := range entries {
		key, want, err := makeDNSAllowRule(e.domain, e.flags)
		if err != nil {
			t.Fatalf("makeDNSAllowRule %s: %v", e.domain, err)
		}
		var got dnsAllowValue
		if err := dest.Lookup(&key, &got); err != nil {
			t.Fatalf("migrated entry %s missing: %v", e.domain, err)
		}
		if got.NameLen != want.NameLen || got.Flags != want.Flags {
			t.Fatalf("entry %s: got NameLen=%d Flags=%d, want NameLen=%d Flags=%d",
				e.domain, got.NameLen, got.Flags, want.NameLen, want.Flags)
		}
		if got.PortCount != 0 {
			t.Fatalf("entry %s: PortCount=%d, want 0 (legacy rules carry no ports)", e.domain, got.PortCount)
		}
	}
}

// TestMigrateDNSAllowInnerMapFromCurrent drives the already-current (40-byte)
// path: an entry carrying an explicit (port, scheme) set must be copied through
// with its ports preserved.
func TestMigrateDNSAllowInnerMapFromCurrent(t *testing.T) {
	ifindex := uint32(43)
	current := newDNSAllowOuterMap(t)

	cur := newLPMInner(t, uint32(unsafe.Sizeof(dnsAllowValue{})))
	key, val, err := makeDNSAllowRule("api.example.com", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule: %v", err)
	}
	val.PortCount = 2
	val.Ports[0] = l7PortEntry{Port: htonsPort(8080), Scheme: L7SchemeHTTP}
	val.Ports[1] = l7PortEntry{Port: htonsPort(8443), Scheme: L7SchemeHTTPS}
	if err := cur.Update(&key, &val, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed current entry: %v", err)
	}

	if err := migrateDNSAllowInnerMap(current, ifindex, "test-current", mustInnerMapID(t, cur)); err != nil {
		t.Fatalf("migrateDNSAllowInnerMap: %v", err)
	}

	dest, err := lookupInnerMap(current, ifindex, MapNameDNSAllowV2)
	if err != nil {
		t.Fatalf("lookupInnerMap: %v", err)
	}

	var got dnsAllowValue
	if err := dest.Lookup(&key, &got); err != nil {
		t.Fatalf("migrated entry missing: %v", err)
	}
	if got.NameLen != val.NameLen || got.Flags != val.Flags || got.PortCount != val.PortCount {
		t.Fatalf("got NameLen=%d Flags=%d PortCount=%d, want NameLen=%d Flags=%d PortCount=%d",
			got.NameLen, got.Flags, got.PortCount, val.NameLen, val.Flags, val.PortCount)
	}
	if got.Ports[0] != val.Ports[0] || got.Ports[1] != val.Ports[1] {
		t.Fatalf("ports not preserved: got %+v, want %+v", got.Ports[:2], val.Ports[:2])
	}
}

// mountBpffs mounts a fresh bpffs at a temp dir, points bpfFSPath at it, and
// registers cleanup to restore the path and unmount. Skips when unavailable.
func mountBpffs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Mount("bpf", dir, "bpf", 0, ""); err != nil {
		t.Skipf("bpffs mount unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(dir, 0) })
	oldPath := bpfFSPath
	bpfFSPath = dir
	t.Cleanup(func() { bpfFSPath = oldPath })
	return dir
}

// TestMigrateDNSAllowMapFailureKeepsLegacyPin covers the rollback path: when
// migration fails partway, the legacy source pin must be preserved so the next
// restart can retry. It pins a legacy dns_allow whose inner uses an
// unsupported 48-byte value layout (the abandoned intermediate format), so
// migrateDNSAllowInnerMap's layout check fails — then asserts
// migratePersistentPolicyMaps errors AND the legacy pin is still present.
func TestMigrateDNSAllowMapFailureKeepsLegacyPin(t *testing.T) {
	mountBpffs(t)
	ifindex := uint32(42)

	// Pin a legacy dns_allow outer whose inner carries an unsupported 48-byte
	// value layout (abandoned intermediate format, not migratable).
	badOuter := newDNSAllowOuterMapWithValueSize(t, 48)
	badInner := newLPMInner(t, 48)
	key, _, err := makeDNSAllowRule("api.example.com", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule: %v", err)
	}
	var raw48 [48]byte
	if err := badInner.Update(&key, &raw48, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed 48-byte entry: %v", err)
	}
	if err := badOuter.Put(&ifindex, badInner); err != nil {
		t.Fatalf("attach bad inner: %v", err)
	}
	badInner.Close()
	if err := badOuter.Pin(pinPath(MapNameDNSAllow)); err != nil {
		t.Fatalf("pin legacy %s: %v", MapNameDNSAllow, err)
	}

	// Pin a fresh dns_allow_v2 outer as the (never-to-be-populated) destination.
	newOuter := newDNSAllowOuterMap(t)
	if err := newOuter.Pin(pinPath(MapNameDNSAllowV2)); err != nil {
		t.Fatalf("pin %s: %v", MapNameDNSAllowV2, err)
	}

	// Migration must fail on the unsupported 48-byte inner layout...
	err = migratePersistentPolicyMaps()
	if err == nil {
		t.Fatal("migratePersistentPolicyMaps succeeded, want failure for unsupported inner value_size")
	}

	// ...and the legacy source pin must be preserved for retry (rollback).
	src, err := ebpf.LoadPinnedMap(pinPath(MapNameDNSAllow), nil)
	if err != nil {
		t.Fatalf("legacy %s pin was removed despite failed migration (rollback broken): %v",
			MapNameDNSAllow, err)
	}
	src.Close()
}

// TestMigrateDNSAllowMapOuterWithBpffs exercises the full outer-map migration:
// a legacy dns_allow hash-of-maps pinned on bpffs (with an 8-byte inner LPM
// trie) is migrated by migrateDNSAllowMap into a fresh dns_allow_v2 outer map,
// and the legacy pin is then removed by removePinnedMap. It mounts a fresh
// bpffs at a temp dir and points bpfFSPath at it, so it runs without touching
// the real /sys/fs/bpf.
func TestMigrateDNSAllowMapOuterWithBpffs(t *testing.T) {
	mountBpffs(t)
	ifindex := uint32(42)

	// Pin a legacy dns_allow outer map (8-byte inner) seeded with two rules.
	legacyOuter := newDNSAllowOuterMapWithValueSize(t, uint32(unsafe.Sizeof(legacyDNSAllowValue{})))
	legacyInner := newLPMInner(t, uint32(unsafe.Sizeof(legacyDNSAllowValue{})))
	type entry struct {
		domain string
		flags  uint8
	}
	entries := []entry{
		{"api.example.com", uint8(netPolicyFlagL7Required)},
		{"static.example.com", 0},
	}
	for _, e := range entries {
		key, val, err := makeDNSAllowRule(e.domain, e.flags)
		if err != nil {
			t.Fatalf("makeDNSAllowRule %s: %v", e.domain, err)
		}
		lv := legacyDNSAllowValue{NameLen: val.NameLen, Flags: val.Flags}
		if err := legacyInner.Update(&key, &lv, ebpf.UpdateAny); err != nil {
			t.Fatalf("seed legacy entry %s: %v", e.domain, err)
		}
	}
	if err := legacyOuter.Put(&ifindex, legacyInner); err != nil {
		t.Fatalf("attach legacy inner: %v", err)
	}
	legacyInner.Close() // inner survives via the outer hash-of-maps reference
	if err := legacyOuter.Pin(pinPath(MapNameDNSAllow)); err != nil {
		t.Fatalf("pin legacy %s: %v", MapNameDNSAllow, err)
	}

	// Pin a fresh dns_allow_v2 outer map (40-byte inner template) for the copy.
	newOuter := newDNSAllowOuterMap(t)
	if err := newOuter.Pin(pinPath(MapNameDNSAllowV2)); err != nil {
		t.Fatalf("pin %s: %v", MapNameDNSAllowV2, err)
	}

	// Run the real outer-map migration.
	if err := migrateDNSAllowMap(MapNameDNSAllow); err != nil {
		t.Fatalf("migrateDNSAllowMap(%s): %v", MapNameDNSAllow, err)
	}

	// The new outer must now hold the migrated rules (NameLen+Flags, PortCount=0).
	dest, err := lookupInnerMap(newOuter, ifindex, MapNameDNSAllowV2)
	if err != nil {
		t.Fatalf("lookupInnerMap: %v", err)
	}
	for _, e := range entries {
		key, want, err := makeDNSAllowRule(e.domain, e.flags)
		if err != nil {
			t.Fatalf("makeDNSAllowRule %s: %v", e.domain, err)
		}
		var got dnsAllowValue
		if err := dest.Lookup(&key, &got); err != nil {
			t.Fatalf("migrated entry %s missing: %v", e.domain, err)
		}
		if got.NameLen != want.NameLen || got.Flags != want.Flags || got.PortCount != 0 {
			t.Fatalf("entry %s: got %+v, want NameLen=%d Flags=%d PortCount=0",
				e.domain, got, want.NameLen, want.Flags)
		}
	}

	// removePinnedMap must delete the legacy pin from the mounted bpffs.
	if err := removePinnedMap(MapNameDNSAllow); err != nil {
		t.Fatalf("removePinnedMap(%s): %v", MapNameDNSAllow, err)
	}
	if _, err := ebpf.LoadPinnedMap(pinPath(MapNameDNSAllow), nil); err == nil {
		t.Fatalf("legacy %s pin still present after removePinnedMap", MapNameDNSAllow)
	}
}

// newAllowOutOuterMapWithValueSize creates an allow_out outer hash-of-maps
// whose inner LPM-trie template carries values of the given size.
func newAllowOutOuterMapWithValueSize(t *testing.T, valueSize uint32) *ebpf.Map {
	t.Helper()
	outer, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.HashOfMaps,
		KeySize:    uint32(unsafe.Sizeof(uint32(0))),
		ValueSize:  uint32(unsafe.Sizeof(uint32(0))),
		MaxEntries: maxNetPolicyEntries,
		InnerMap: &ebpf.MapSpec{
			Type:       ebpf.LPMTrie,
			KeySize:    uint32(unsafe.Sizeof(lpmKey{})),
			ValueSize:  valueSize,
			MaxEntries: maxNetPolicyEntries,
			Flags:      unix.BPF_F_NO_PREALLOC,
		},
	})
	if err != nil {
		t.Fatalf("create allow_out outer map (value_size=%d): %v", valueSize, err)
	}
	t.Cleanup(func() { outer.Close() })
	return outer
}

// newAllowOutLPMInner creates a standalone LPM-trie inner map keyed by lpmKey
// with the given value size.
func newAllowOutLPMInner(t *testing.T, valueSize uint32) *ebpf.Map {
	t.Helper()
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(lpmKey{})),
		ValueSize:  valueSize,
		MaxEntries: maxNetPolicyEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
	})
	if err != nil {
		t.Fatalf("create allow_out LPM inner (value_size=%d): %v", valueSize, err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// newAllowOutV3OuterMap creates the current allow_out_v3 outer hash-of-maps
// with an inner template matching net_policy_value_v3.
func newAllowOutV3OuterMap(t *testing.T) *ebpf.Map {
	t.Helper()
	outer, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.HashOfMaps,
		KeySize:    uint32(unsafe.Sizeof(uint32(0))),
		ValueSize:  uint32(unsafe.Sizeof(uint32(0))),
		MaxEntries: maxNetPolicyEntries,
		InnerMap: &ebpf.MapSpec{
			Type:       ebpf.LPMTrie,
			KeySize:    uint32(unsafe.Sizeof(lpmKeyV3{})),
			ValueSize:  uint32(unsafe.Sizeof(netPolicyValueV3{})),
			MaxEntries: maxNetPolicyEntries,
			Flags:      unix.BPF_F_NO_PREALLOC,
		},
	})
	if err != nil {
		t.Fatalf("create allow_out_v3 outer map: %v", err)
	}
	t.Cleanup(func() { outer.Close() })
	return outer
}

// TestMigrateAllowOutMapFailureKeepsLegacyPin is the allow_out counterpart of
// TestMigrateDNSAllowMapFailureKeepsLegacyPin: a legacy allow_out_v2 outer
// pinned on bpffs whose inner carries the unsupported 48-byte value layout
// (the abandoned intermediate net_policy_value_v2 format) must make
// migratePersistentPolicyMaps fail, and the legacy source pin must survive
// for the next restart's retry (rollback).
func TestMigrateAllowOutMapFailureKeepsLegacyPin(t *testing.T) {
	mountBpffs(t)
	ifindex := uint32(42)

	// Pin a legacy allow_out_v2 outer whose inner carries an unsupported
	// 48-byte value layout (abandoned intermediate format, not migratable).
	badOuter := newAllowOutOuterMapWithValueSize(t, 48)
	badInner := newAllowOutLPMInner(t, 48)
	key := lpmKey{Prefixlen: 32, IP: mustParseCIDRForTest(t, "192.0.2.44/32").IP}
	var raw48 [48]byte
	if err := badInner.Update(&key, &raw48, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed 48-byte entry: %v", err)
	}
	if err := badOuter.Put(&ifindex, badInner); err != nil {
		t.Fatalf("attach bad inner: %v", err)
	}
	badInner.Close() // inner survives via the outer hash-of-maps reference
	if err := badOuter.Pin(pinPath(MapNameAllowOutV2)); err != nil {
		t.Fatalf("pin legacy %s: %v", MapNameAllowOutV2, err)
	}

	// Pin a fresh allow_out_v3 outer as the (never-to-be-populated) destination.
	newOuter := newAllowOutV3OuterMap(t)
	if err := newOuter.Pin(pinPath(MapNameAllowOutV3)); err != nil {
		t.Fatalf("pin %s: %v", MapNameAllowOutV3, err)
	}

	// Migration must fail on the unsupported 48-byte inner layout...
	err := migratePersistentPolicyMaps()
	if err == nil {
		t.Fatal("migratePersistentPolicyMaps succeeded, want failure for unsupported inner value_size")
	}
	if !strings.Contains(err.Error(), "unsupported value_size") {
		t.Fatalf("migration failed for an unexpected reason: %v", err)
	}

	// ...and the legacy source pin must be preserved for retry (rollback).
	src, err := ebpf.LoadPinnedMap(pinPath(MapNameAllowOutV2), nil)
	if err != nil {
		t.Fatalf("legacy %s pin was removed despite failed migration (rollback broken): %v",
			MapNameAllowOutV2, err)
	}
	src.Close()
}

// TestMigrateDNSFailureKeepsAllowOutLegacyPin covers the combined-failure
// rollback case the single-map tests miss: the allow_out migration succeeds
// but the DNS migration then fails. The legacy allow pin must survive the
// failure — if migratePersistentPolicyMaps unlinked it before the DNS
// migration ran, Init's rollback would drop the freshly populated
// allow_out_v3 and the next restart would skip re-migration (legacy pin
// gone), permanently losing the allow_out policy.
func TestMigrateDNSFailureKeepsAllowOutLegacyPin(t *testing.T) {
	mountBpffs(t)
	ifindex := uint32(42)

	// A good legacy allow_out_v2 (16-byte net_policy_value_v2 inner) that
	// migrates cleanly.
	allowOuter := newAllowOutOuterMapWithValueSize(t, uint32(unsafe.Sizeof(netPolicyValueV2{})))
	allowInner := newAllowOutLPMInner(t, uint32(unsafe.Sizeof(netPolicyValueV2{})))
	allowKey := lpmKey{Prefixlen: 32, IP: mustParseCIDRForTest(t, "192.0.2.44/32").IP}
	allowVal := netPolicyValueV2{Flags: 0, ExpiresAtNS: 0}
	if err := allowInner.Update(&allowKey, &allowVal, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed legacy allow entry: %v", err)
	}
	if err := allowOuter.Put(&ifindex, allowInner); err != nil {
		t.Fatalf("attach legacy allow inner: %v", err)
	}
	allowInner.Close()
	if err := allowOuter.Pin(pinPath(MapNameAllowOutV2)); err != nil {
		t.Fatalf("pin legacy %s: %v", MapNameAllowOutV2, err)
	}
	// Destination allow_out_v3 outer.
	newAllowOuter := newAllowOutV3OuterMap(t)
	if err := newAllowOuter.Pin(pinPath(MapNameAllowOutV3)); err != nil {
		t.Fatalf("pin %s: %v", MapNameAllowOutV3, err)
	}

	// A bad legacy dns_allow whose inner carries the unsupported 48-byte value
	// layout, so the DNS migration fails.
	badDNSOuter := newDNSAllowOuterMapWithValueSize(t, 48)
	badDNSInner := newLPMInner(t, 48)
	dnsKey, _, err := makeDNSAllowRule("api.example.com", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule: %v", err)
	}
	var raw48 [48]byte
	if err := badDNSInner.Update(&dnsKey, &raw48, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed 48-byte dns entry: %v", err)
	}
	if err := badDNSOuter.Put(&ifindex, badDNSInner); err != nil {
		t.Fatalf("attach bad dns inner: %v", err)
	}
	badDNSInner.Close()
	if err := badDNSOuter.Pin(pinPath(MapNameDNSAllow)); err != nil {
		t.Fatalf("pin legacy %s: %v", MapNameDNSAllow, err)
	}
	// Destination dns_allow_v2 outer.
	newDNSOuter := newDNSAllowOuterMap(t)
	if err := newDNSOuter.Pin(pinPath(MapNameDNSAllowV2)); err != nil {
		t.Fatalf("pin %s: %v", MapNameDNSAllowV2, err)
	}

	// Migration must fail on the unsupported DNS inner layout...
	err = migratePersistentPolicyMaps()
	if err == nil {
		t.Fatal("migratePersistentPolicyMaps succeeded, want failure for unsupported DNS inner value_size")
	}

	// ...and the legacy allow pin must survive so the next restart retries
	// both migrations together (this is the regression the fix addresses).
	src, err := ebpf.LoadPinnedMap(pinPath(MapNameAllowOutV2), nil)
	if err != nil {
		t.Fatalf("legacy %s pin was removed after allow migration but DNS migration failed; "+
			"allow_out policy would be lost on restart: %v", MapNameAllowOutV2, err)
	}
	src.Close()
}

// TestMigrateAllowOutMapOuterWithBpffs covers the allow_out success path the
// rollback tests miss: a legacy allow_out_v2 outer (16-byte
// net_policy_value_v2 inner, flags+expires but no ports) is migrated into a
// fresh allow_out_v3 outer. An L7 entry expands to the default {80,443} /48
// set — keeping the L7 flag, the per-port scheme, and the expiry — while a
// plain entry becomes a /32 any-port entry.
func TestMigrateAllowOutMapOuterWithBpffs(t *testing.T) {
	mountBpffs(t)
	ifindex := uint32(42)

	l7IP := mustParseCIDRForTest(t, "192.0.2.44/32").IP
	plainIP := mustParseCIDRForTest(t, "192.0.2.45/32").IP
	expires := uint64(999999999)

	// Legacy allow_out_v2 outer with a 16-byte net_policy_value_v2 inner,
	// seeded with one L7 entry and one plain entry (both with an expiry).
	legacyOuter := newAllowOutOuterMapWithValueSize(t, uint32(unsafe.Sizeof(netPolicyValueV2{})))
	legacyInner := newAllowOutLPMInner(t, uint32(unsafe.Sizeof(netPolicyValueV2{})))
	seed := []struct {
		ip    uint32
		flags uint8
	}{
		{l7IP, uint8(netPolicyFlagL7Required)},
		{plainIP, 0},
	}
	for _, s := range seed {
		key := lpmKey{Prefixlen: 32, IP: s.ip}
		val := netPolicyValueV2{Flags: s.flags, ExpiresAtNS: expires}
		if err := legacyInner.Update(&key, &val, ebpf.UpdateAny); err != nil {
			t.Fatalf("seed legacy entry: %v", err)
		}
	}
	if err := legacyOuter.Put(&ifindex, legacyInner); err != nil {
		t.Fatalf("attach legacy inner: %v", err)
	}
	legacyInner.Close()
	if err := legacyOuter.Pin(pinPath(MapNameAllowOutV2)); err != nil {
		t.Fatalf("pin legacy %s: %v", MapNameAllowOutV2, err)
	}

	// Fresh allow_out_v3 outer as the destination.
	newOuter := newAllowOutV3OuterMap(t)
	if err := newOuter.Pin(pinPath(MapNameAllowOutV3)); err != nil {
		t.Fatalf("pin %s: %v", MapNameAllowOutV3, err)
	}

	if err := migrateAllowOutMap(MapNameAllowOutV2); err != nil {
		t.Fatalf("migrateAllowOutMap: %v", err)
	}

	dest, err := lookupInnerMap(newOuter, ifindex, MapNameAllowOutV3)
	if err != nil {
		t.Fatalf("lookupInnerMap: %v", err)
	}

	// The L7 entry expands to the default {80/http, 443/https} /48 set.
	for _, tc := range []struct {
		port   uint16
		scheme uint8
	}{
		{htonsPort(80), L7SchemeHTTP},
		{htonsPort(443), L7SchemeHTTPS},
	} {
		key := lpmKeyV3{Prefixlen: 48, IP: l7IP, Port: tc.port}
		var got netPolicyValueV3
		if err := dest.Lookup(&key, &got); err != nil {
			t.Fatalf("migrated L7 /48 entry for port %d missing: %v", ntohsPort(tc.port), err)
		}
		if got.Flags&uint8(netPolicyFlagL7Required) == 0 {
			t.Fatalf("port %d lost L7 flag: %#x", ntohsPort(tc.port), got.Flags)
		}
		if got.Scheme != tc.scheme {
			t.Fatalf("port %d scheme=%d, want %d", ntohsPort(tc.port), got.Scheme, tc.scheme)
		}
		if got.ExpiresAtNS != expires {
			t.Fatalf("port %d expiry=%d, want %d (preserved)", ntohsPort(tc.port), got.ExpiresAtNS, expires)
		}
	}

	// The plain entry becomes a single /32 any-port entry with no L7 flag.
	plainKey := lpmKeyV3{Prefixlen: 32, IP: plainIP, Port: 0}
	var plain netPolicyValueV3
	if err := dest.Lookup(&plainKey, &plain); err != nil {
		t.Fatalf("migrated plain /32 entry missing: %v", err)
	}
	if plain.Flags&uint8(netPolicyFlagL7Required) != 0 {
		t.Fatalf("plain /32 has unexpected L7 flag: %#x", plain.Flags)
	}
	if plain.Scheme != L7SchemeNone {
		t.Fatalf("plain /32 scheme=%d, want none", plain.Scheme)
	}
	if plain.ExpiresAtNS != expires {
		t.Fatalf("plain /32 expiry=%d, want %d (preserved)", plain.ExpiresAtNS, expires)
	}
}
