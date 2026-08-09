package cubevs

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH dnslearn ../src/dns_learn_test.bpf.c -- -I../vmlinux/$GOARCH

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
)

const dnsLearnTestCaseLen = 12

type dnsLearnTestEnv struct {
	program        *ebpf.Program
	allowOut       *ebpf.Map
	queryStore     *ebpf.Map
	allowInnerSpec *ebpf.MapSpec
}

func loadDNSLearnTestEnv(t *testing.T) *dnsLearnTestEnv {
	t.Helper()

	spec, err := loadDnslearn()
	if err != nil {
		t.Fatalf("load dns learn test spec: %v", err)
	}
	allowSpec := spec.Maps["allow_out_v3"]
	if allowSpec == nil || allowSpec.InnerMap == nil {
		t.Fatal("allow_out_v3 spec or inner template missing")
	}
	allowInnerSpec := allowSpec.InnerMap.Copy()

	for name, mapSpec := range spec.Maps {
		switch name {
		case ".rodata", "allow_out_v3", "test_query_store":
			mapSpec.Pinning = ebpf.PinNone
		default:
			delete(spec.Maps, name)
		}
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF dns learn test unavailable: %v", err)
		}
		t.Fatalf("load dns learn test collection: %v", err)
	}
	t.Cleanup(coll.Close)

	env := &dnsLearnTestEnv{
		program:        coll.Programs["test_dns_learn"],
		allowOut:       coll.Maps["allow_out_v3"],
		queryStore:     coll.Maps["test_query_store"],
		allowInnerSpec: allowInnerSpec,
	}
	if env.program == nil || env.allowOut == nil || env.queryStore == nil {
		t.Fatal("loaded dns learn program or maps missing")
	}
	return env
}

// runDNSLearn drives dns_learn_response_ip with the given query against the
// sandbox's allow_out_v3 inner map, returning that inner map for assertions.
// seeds (if any) are written into the fresh inner map BEFORE the program
// runs, simulating pre-existing static/learned entries.
func runDNSLearn(t *testing.T, env *dnsLearnTestEnv, ifindex uint32, ip uint32,
	ttl uint32, query dnsQueryTrackValue, seeds ...allowOutV3Entry,
) *ebpf.Map {
	t.Helper()

	innerSpec := env.allowInnerSpec.Copy()
	innerSpec.Name = "allow_dns_learn"
	innerSpec.Pinning = ebpf.PinNone
	inner, err := ebpf.NewMap(innerSpec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF LPM trie unavailable: %v", err)
		}
		t.Fatalf("create allow inner: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	if err := env.allowOut.Put(&ifindex, inner); err != nil {
		t.Fatalf("attach allow inner map: %v", err)
	}

	for i, s := range seeds {
		if err := inner.Update(&s.key, &s.value, ebpf.UpdateAny); err != nil {
			t.Fatalf("seed allow inner entry %d: %v", i, err)
		}
	}

	qkey := uint32(0)
	if err := env.queryStore.Put(&qkey, &query); err != nil {
		t.Fatalf("seed query store: %v", err)
	}

	// Pad the packet to 16 bytes: the skb test-run path rejects very small
	// buffers (a 12-byte packet fails with EINVAL), while the program only
	// reads the leading dns_learn_case struct.
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data[0:4], ifindex)
	binary.LittleEndian.PutUint32(data[4:8], ip)
	binary.LittleEndian.PutUint32(data[8:12], ttl)
	ret, _, err := env.program.Test(data)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF dns learn test-run unavailable: %v", err)
		}
		t.Fatalf("run dns learn test: %v", err)
	}
	if ret != 0 {
		t.Fatalf("test_dns_learn returned %d, want TC_ACT_OK", ret)
	}
	return inner
}

func lookupAllowV3(t *testing.T, inner *ebpf.Map, key lpmKeyV3) (netPolicyValueV3, bool) {
	t.Helper()
	var value netPolicyValueV3
	if err := inner.Lookup(&key, &value); err != nil {
		return netPolicyValueV3{}, false
	}
	return value, true
}

// TestDNSLearnPlainAllowWritesIPOnlyEntry is the B3 regression test: a plain
// (non-L7) domain allow rule must still be learned as a /32 (any-port) entry,
// exactly as the pre-v3 dataplane did. The regression wrote nothing, so the
// resolved IP was rejected by classify_egress_flow under default-deny.
func TestDNSLearnPlainAllowWritesIPOnlyEntry(t *testing.T) {
	env := loadDNSLearnTestEnv(t)
	ifindex := uint32(300)
	ip := mustParseCIDRForTest(t, "192.0.2.50").IP

	// Plain (non-L7) allow: flags=0, port_count=0.
	inner := runDNSLearn(t, env, ifindex, ip, 300, dnsQueryTrackValue{Flags: 0, PortCount: 0})

	value, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 32, IP: ip, Port: 0})
	if !ok {
		t.Fatal("plain allow did not learn a /32 entry (B3 regression)")
	}
	if value.Flags&uint8(netPolicyFlagL7Required) != 0 {
		t.Fatalf("plain allow /32 has unexpected L7 flag: %#x", value.Flags)
	}
	if value.Scheme != L7SchemeNone {
		t.Fatalf("plain allow /32 scheme=%d, want L7SchemeNone", value.Scheme)
	}
	if value.ExpiresAtNS == 0 {
		t.Fatal("plain allow /32 has zero expiry, want temporary (DNS TTL)")
	}
}

// TestDNSLearnL7DefaultPortSet covers the L7 path with no explicit ports:
// it must learn the default {80/http, 443/https} set as /48 entries.
func TestDNSLearnL7DefaultPortSet(t *testing.T) {
	env := loadDNSLearnTestEnv(t)
	ifindex := uint32(301)
	ip := mustParseCIDRForTest(t, "192.0.2.60").IP

	query := dnsQueryTrackValue{Flags: uint8(netPolicyFlagL7Required), PortCount: 0}
	inner := runDNSLearn(t, env, ifindex, ip, 300, query)

	for _, tc := range []struct {
		port   uint16
		scheme uint8
	}{
		{htonsPort(80), L7SchemeHTTP},
		{htonsPort(443), L7SchemeHTTPS},
	} {
		value, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: tc.port})
		if !ok {
			t.Fatalf("L7 default missing /48 entry for port %d", ntohsPort(tc.port))
		}
		if value.Scheme != tc.scheme {
			t.Fatalf("port %d scheme=%d, want %d", ntohsPort(tc.port), value.Scheme, tc.scheme)
		}
		if value.Flags&uint8(netPolicyFlagL7Required) == 0 {
			t.Fatalf("port %d missing L7 flag", ntohsPort(tc.port))
		}
	}
}

// TestDNSLearnL7ExplicitPorts covers the L7 path with explicit ports: only the
// declared (port, scheme) tuples are learned, no others.
func TestDNSLearnL7ExplicitPorts(t *testing.T) {
	env := loadDNSLearnTestEnv(t)
	ifindex := uint32(302)
	ip := mustParseCIDRForTest(t, "192.0.2.70").IP

	query := dnsQueryTrackValue{
		Flags:     uint8(netPolicyFlagL7Required),
		PortCount: 2,
		Ports: [maxL7PortsPerHost]l7PortEntry{
			{Port: htonsPort(8443), Scheme: L7SchemeHTTPS},
			{Port: htonsPort(8080), Scheme: L7SchemeHTTP},
		},
	}
	inner := runDNSLearn(t, env, ifindex, ip, 300, query)

	if _, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(8443)}); !ok {
		t.Fatal("missing /48 entry for explicit port 8443")
	}
	if _, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(8080)}); !ok {
		t.Fatal("missing /48 entry for explicit port 8080")
	}
	if _, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(80)}); ok {
		t.Fatal("unexpected /48 entry for non-configured port 80")
	}
}

// testFlagMarker is a high-bit flag used only by tests to detect improper
// flag inheritance from COVERING entries (it is not a real netPolicyFlag*).
const testFlagMarker = 0x40

// TestDNSLearnCoveringStaticCIDRDoesNotImmortalize is the exact-key-match
// regression test: a static CIDR COVERING the resolved IP must NOT make the
// DNS-learned entry inherit the static zero expiry (never ages) or the
// covering entry's flags. The LPM lookup inside dns_learn_response_ip is
// longest-prefix, so without the key_prefixlen check the covering static
// entry was treated as "an existing entry for the same key".
func TestDNSLearnCoveringStaticCIDRDoesNotImmortalize(t *testing.T) {
	env := loadDNSLearnTestEnv(t)
	ifindex := uint32(303)

	// Static /24 (never expires) covering both test IPs.
	staticSubnet := allowOutV3Entry{
		key:   lpmKeyV3{Prefixlen: 24, IP: mustParseCIDRForTest(t, "192.0.2.0").IP, Port: 0},
		value: netPolicyValueV3{Flags: testFlagMarker, KeyPrefixlen: 24}, // ExpiresAtNS: 0 = static
	}

	// Plain (non-L7) learn of 192.0.2.50 under the covering /24.
	ipPlain := mustParseCIDRForTest(t, "192.0.2.50").IP
	inner := runDNSLearn(t, env, ifindex, ipPlain, 300,
		dnsQueryTrackValue{Flags: 0, PortCount: 0}, staticSubnet)
	value, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 32, IP: ipPlain, Port: 0})
	if !ok {
		t.Fatal("plain allow did not learn a /32 entry")
	}
	if value.KeyPrefixlen != 32 {
		t.Fatalf("learned /32 KeyPrefixlen=%d, want 32 (lookup may have hit the covering /24)", value.KeyPrefixlen)
	}
	if value.ExpiresAtNS == 0 {
		t.Fatal("learned /32 inherited static zero expiry from covering /24 (would never age)")
	}
	if value.Flags&testFlagMarker != 0 {
		t.Fatalf("learned /32 inherited flags from covering /24: %#x", value.Flags)
	}

	// L7 learn of 192.0.2.60 under the covering /24.
	ipL7 := mustParseCIDRForTest(t, "192.0.2.60").IP
	inner = runDNSLearn(t, env, ifindex, ipL7, 300,
		dnsQueryTrackValue{Flags: uint8(netPolicyFlagL7Required), PortCount: 0}, staticSubnet)
	value, ok = lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ipL7, Port: htonsPort(443)})
	if !ok {
		t.Fatal("L7 allow did not learn a /48 entry for 443")
	}
	if value.KeyPrefixlen != 48 {
		t.Fatalf("learned /48 KeyPrefixlen=%d, want 48 (lookup may have hit the covering /24)", value.KeyPrefixlen)
	}
	if value.ExpiresAtNS == 0 {
		t.Fatal("learned /48 inherited static zero expiry from covering /24 (would never age)")
	}
	if value.Flags&testFlagMarker != 0 {
		t.Fatalf("learned /48 inherited flags from covering /24: %#x", value.Flags)
	}
}

// TestDNSLearnExactStaticEntrySurvivesRefresh is the counterpart: an existing
// static entry for the EXACT same (ip, port)/48 key must survive a DNS
// refresh — its zero expiry and its extra flags are preserved. Other ports
// of the same learn get a normal TTL entry.
func TestDNSLearnExactStaticEntrySurvivesRefresh(t *testing.T) {
	env := loadDNSLearnTestEnv(t)
	ifindex := uint32(304)
	ip := mustParseCIDRForTest(t, "192.0.2.70").IP

	staticExact := allowOutV3Entry{
		key:   lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)},
		value: netPolicyValueV3{Flags: uint8(netPolicyFlagL7Required) | testFlagMarker, Scheme: L7SchemeHTTP, KeyPrefixlen: 48},
	}
	inner := runDNSLearn(t, env, ifindex, ip, 300,
		dnsQueryTrackValue{Flags: uint8(netPolicyFlagL7Required), PortCount: 0}, staticExact)

	// Exact static /48(443): zero expiry + marker flag preserved.
	value, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)})
	if !ok {
		t.Fatal("missing /48 entry for 443")
	}
	if value.ExpiresAtNS != 0 {
		t.Fatal("exact static /48 lost its zero expiry on DNS refresh")
	}
	if value.Flags&testFlagMarker == 0 {
		t.Fatalf("exact static /48 lost its flags on DNS refresh: %#x", value.Flags)
	}
	if value.Scheme != L7SchemeHTTPS {
		t.Fatalf("exact static /48 scheme=%d after refresh, want HTTPS (scheme is last-write-wins)", value.Scheme)
	}

	// /48(80) has no exact static entry: normal learned TTL entry.
	value, ok = lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(80)})
	if !ok {
		t.Fatal("missing /48 entry for 80")
	}
	if value.ExpiresAtNS == 0 {
		t.Fatal("/48(80) wrongly became static (no exact static entry exists)")
	}
	if value.Flags&testFlagMarker != 0 {
		t.Fatalf("/48(80) inherited marker flag from the 443 entry: %#x", value.Flags)
	}
}

// TestDNSLearnRefreshRenewsTTLMergesFlagsAndOverwritesScheme covers the
// remaining same-key merge cell: the OLD entry is itself a LEARNED entry for
// the exact same key. The refresh must (a) merge flags (old marker retained),
// (b) RENEW the expiry to the new DNS TTL (not keep the stale one), and
// (c) overwrite the scheme with the latest learn (last write wins).
func TestDNSLearnRefreshRenewsTTLMergesFlagsAndOverwritesScheme(t *testing.T) {
	env := loadDNSLearnTestEnv(t)
	ifindex := uint32(307)
	ip := mustParseCIDRForTest(t, "192.0.2.100").IP

	staleLearned := allowOutV3Entry{
		key: lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)},
		value: netPolicyValueV3{
			ExpiresAtNS:  1, // stale, about to expire
			Flags:        uint8(netPolicyFlagL7Required) | testFlagMarker,
			Scheme:       L7SchemeHTTP, // wrong scheme on 443: refresh must overwrite
			KeyPrefixlen: 48,
		},
	}
	inner := runDNSLearn(t, env, ifindex, ip, 300,
		dnsQueryTrackValue{Flags: uint8(netPolicyFlagL7Required), PortCount: 0}, staleLearned)

	value, ok := lookupAllowV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)})
	if !ok {
		t.Fatal("missing /48 entry for 443")
	}
	if value.KeyPrefixlen != 48 {
		t.Fatalf("KeyPrefixlen=%d, want 48", value.KeyPrefixlen)
	}
	if value.ExpiresAtNS == 0 {
		t.Fatal("refresh of a learned entry must keep a non-zero TTL (not become static)")
	}
	if value.ExpiresAtNS == 1 {
		t.Fatal("refresh kept the stale expiry instead of renewing to the new DNS TTL")
	}
	if value.Flags&testFlagMarker == 0 {
		t.Fatalf("refresh lost old learned flags: %#x", value.Flags)
	}
	if value.Scheme != L7SchemeHTTPS {
		t.Fatalf("scheme=%d after refresh, want HTTPS (last write wins)", value.Scheme)
	}
}
