package cubevs

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH egresspolicy ../src/egress_policy_test.bpf.c -- -I../vmlinux/$GOARCH

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
)

const (
	egressPolicyTestCaseLen = 16
	flowVerdictReject       = uint8(0)
	flowVerdictSNAT         = uint8(1)
	flowVerdictHTTP         = uint8(2)
	flowVerdictHTTPS        = uint8(3)
)

type egressPolicyTestEnv struct {
	program        *ebpf.Program
	allowOut       *ebpf.Map
	denyOut        *ebpf.Map
	allowInnerSpec *ebpf.MapSpec
	denyInnerSpec  *ebpf.MapSpec
}

func loadEgressPolicyTestEnv(t *testing.T) *egressPolicyTestEnv {
	t.Helper()

	spec, err := loadEgresspolicy()
	if err != nil {
		t.Fatalf("load egress policy test spec: %v", err)
	}
	allowSpec := spec.Maps["allow_out_v3"]
	denySpec := spec.Maps["deny_out"]
	if allowSpec == nil || allowSpec.InnerMap == nil || denySpec == nil || denySpec.InnerMap == nil {
		t.Fatal("egress policy map specs or inner templates missing")
	}
	allowInnerSpec := allowSpec.InnerMap.Copy()
	denyInnerSpec := denySpec.InnerMap.Copy()

	for name, mapSpec := range spec.Maps {
		switch name {
		case ".rodata", "allow_out_v3", "deny_out":
			mapSpec.Pinning = ebpf.PinNone
		default:
			delete(spec.Maps, name)
		}
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF policy test unavailable: %v", err)
		}
		t.Fatalf("load egress policy test collection: %v", err)
	}
	t.Cleanup(coll.Close)

	env := &egressPolicyTestEnv{
		program:        coll.Programs["test_classify_egress_flow"],
		allowOut:       coll.Maps["allow_out_v3"],
		denyOut:        coll.Maps["deny_out"],
		allowInnerSpec: allowInnerSpec,
		denyInnerSpec:  denyInnerSpec,
	}
	if env.program == nil || env.allowOut == nil || env.denyOut == nil {
		t.Fatal("loaded egress policy program or maps missing")
	}
	return env
}

func newEgressPolicyInnerMap(t *testing.T, spec *ebpf.MapSpec, name string) *ebpf.Map {
	t.Helper()

	innerSpec := spec.Copy()
	innerSpec.Name = name
	innerSpec.Pinning = ebpf.PinNone
	inner, err := ebpf.NewMap(innerSpec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF LPM trie unavailable: %v", err)
		}
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	return inner
}

func (env *egressPolicyTestEnv) attachInnerMaps(t *testing.T, ifindex uint32) (*ebpf.Map, *ebpf.Map) {
	t.Helper()

	allowInner := newEgressPolicyInnerMap(t, env.allowInnerSpec, "allow_test")
	denyInner := newEgressPolicyInnerMap(t, env.denyInnerSpec, "deny_test")
	if err := env.allowOut.Put(&ifindex, allowInner); err != nil {
		t.Fatalf("attach allow inner map: %v", err)
	}
	if err := env.denyOut.Put(&ifindex, denyInner); err != nil {
		t.Fatalf("attach deny inner map: %v", err)
	}
	return allowInner, denyInner
}

func runEgressPolicyCase(t *testing.T, prog *ebpf.Program, ifindex, daddr uint32,
	dport uint16,
) uint8 {
	t.Helper()

	data := make([]byte, egressPolicyTestCaseLen)
	binary.LittleEndian.PutUint32(data[0:4], ifindex)
	binary.LittleEndian.PutUint32(data[4:8], daddr)
	binary.LittleEndian.PutUint16(data[8:10], dport)
	ret, out, err := prog.Test(data)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF policy test-run unavailable: %v", err)
		}
		t.Fatalf("run egress policy test: %v", err)
	}
	if ret != 0 {
		t.Fatalf("test_classify_egress_flow returned %d, want TC_ACT_OK", ret)
	}
	if len(out) < egressPolicyTestCaseLen {
		t.Fatalf("test output length=%d, want >=%d", len(out), egressPolicyTestCaseLen)
	}
	return out[10]
}

func mustParseCIDRForTest(t *testing.T, cidr string) lpmKey {
	t.Helper()

	key, err := parseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse %q: %v", cidr, err)
	}
	return key
}

func TestClassifyEgressFlowLPMFallback(t *testing.T) {
	env := loadEgressPolicyTestEnv(t)
	tests := []struct {
		name      string
		allowCIDR string
		daddr     string
	}{
		{name: "IP rule", allowCIDR: "203.0.113.9/32", daddr: "203.0.113.9"},
		{name: "subnet rule", allowCIDR: "198.51.100.0/24", daddr: "198.51.100.42"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ifindex := uint32(100 + i)
			allowInner, denyInner := env.attachInnerMaps(t, ifindex)
			allowKey := mustParseCIDRForTest(t, tt.allowCIDR)
			if err := allowInner.Put(&lpmKeyV3{
				Prefixlen: allowKey.Prefixlen,
				IP:        allowKey.IP,
			}, &netPolicyValueV3{}); err != nil {
				t.Fatalf("insert allow rule %s: %v", tt.allowCIDR, err)
			}

			// A deny-all backstop proves that FLOW_SNAT came from the /48
			// lookup falling back to the allow rule, not from default allow.
			denyAll := mustParseCIDRForTest(t, "0.0.0.0/0")
			denyValue := uint32(1)
			if err := denyInner.Put(&denyAll, &denyValue); err != nil {
				t.Fatalf("insert deny-all rule: %v", err)
			}

			daddr := mustParseCIDRForTest(t, tt.daddr).IP
			got := runEgressPolicyCase(t, env.program, ifindex, daddr, htonsPort(443))
			if got != flowVerdictSNAT {
				t.Fatalf("verdict=%d, want FLOW_SNAT", got)
			}
		})
	}
}

func TestClassifyEgressFlowExactL7Match(t *testing.T) {
	env := loadEgressPolicyTestEnv(t)
	ifindex := uint32(150)
	allowInner, denyInner := env.attachInnerMaps(t, ifindex)
	daddr := mustParseCIDRForTest(t, "192.0.2.20").IP
	dport := htonsPort(8443)
	allowKey := lpmKeyV3{Prefixlen: 48, IP: daddr, Port: dport}
	allowValue := netPolicyValueV3{
		Flags:  uint8(netPolicyFlagL7Required),
		Scheme: L7SchemeHTTPS,
	}
	if err := allowInner.Put(&allowKey, &allowValue); err != nil {
		t.Fatalf("insert exact L7 allow rule: %v", err)
	}

	denyAll := mustParseCIDRForTest(t, "0.0.0.0/0")
	denyValue := uint32(1)
	if err := denyInner.Put(&denyAll, &denyValue); err != nil {
		t.Fatalf("insert deny-all rule: %v", err)
	}

	if got := runEgressPolicyCase(t, env.program, ifindex, daddr, dport); got != flowVerdictHTTPS {
		t.Fatalf("exact-port verdict=%d, want FLOW_HTTPS", got)
	}
	if got := runEgressPolicyCase(t, env.program, ifindex, daddr, htonsPort(443)); got != flowVerdictReject {
		t.Fatalf("different-port verdict=%d, want FLOW_REJECT", got)
	}
}

// TestClassifyEgressFlowExactL7MatchHTTP covers the plaintext HTTP interception
// verdict (FLOW_HTTP) — the primary new data path the HTTPS-only cases did not
// exercise. eBPF returns FLOW_HTTP for an L7 entry whose scheme is http.
func TestClassifyEgressFlowExactL7MatchHTTP(t *testing.T) {
	env := loadEgressPolicyTestEnv(t)
	ifindex := uint32(152)
	allowInner, denyInner := env.attachInnerMaps(t, ifindex)
	daddr := mustParseCIDRForTest(t, "192.0.2.22").IP
	dport := htonsPort(8080)
	allowKey := lpmKeyV3{Prefixlen: 48, IP: daddr, Port: dport}
	allowValue := netPolicyValueV3{
		Flags:  uint8(netPolicyFlagL7Required),
		Scheme: L7SchemeHTTP,
	}
	if err := allowInner.Put(&allowKey, &allowValue); err != nil {
		t.Fatalf("insert exact L7 HTTP allow rule: %v", err)
	}

	denyAll := mustParseCIDRForTest(t, "0.0.0.0/0")
	denyValue := uint32(1)
	if err := denyInner.Put(&denyAll, &denyValue); err != nil {
		t.Fatalf("insert deny-all rule: %v", err)
	}

	if got := runEgressPolicyCase(t, env.program, ifindex, daddr, dport); got != flowVerdictHTTP {
		t.Fatalf("exact-port verdict=%d, want FLOW_HTTP", got)
	}
	if got := runEgressPolicyCase(t, env.program, ifindex, daddr, htonsPort(80)); got != flowVerdictReject {
		t.Fatalf("different-port verdict=%d, want FLOW_REJECT", got)
	}
}

func TestClassifyEgressFlowL7UnknownSchemeFailsClosed(t *testing.T) {
	env := loadEgressPolicyTestEnv(t)
	ifindex := uint32(151)
	allowInner, denyInner := env.attachInnerMaps(t, ifindex)
	daddr := mustParseCIDRForTest(t, "192.0.2.21").IP
	dport := htonsPort(8443)
	// L7_REQUIRED set but scheme is NONE — a corrupt or half-written entry.
	// Must fail closed (FLOW_REJECT) rather than downgrade to FLOW_SNAT and
	// silently bypass the TPROXY intercept the rule asked for.
	allowKey := lpmKeyV3{Prefixlen: 48, IP: daddr, Port: dport}
	allowValue := netPolicyValueV3{
		Flags:  uint8(netPolicyFlagL7Required),
		Scheme: L7SchemeNone,
	}
	if err := allowInner.Put(&allowKey, &allowValue); err != nil {
		t.Fatalf("insert L7 allow rule with unknown scheme: %v", err)
	}

	denyAll := mustParseCIDRForTest(t, "0.0.0.0/0")
	denyValue := uint32(1)
	if err := denyInner.Put(&denyAll, &denyValue); err != nil {
		t.Fatalf("insert deny-all rule: %v", err)
	}

	if got := runEgressPolicyCase(t, env.program, ifindex, daddr, dport); got != flowVerdictReject {
		t.Fatalf("verdict=%d, want FLOW_REJECT (fail closed on unknown scheme)", got)
	}
}

func TestClassifyEgressFlowExpiredAllow(t *testing.T) {
	env := loadEgressPolicyTestEnv(t)
	tests := []struct {
		name      string
		prefixlen uint32
	}{
		{name: "expired exact L7 then deny", prefixlen: 48},
		{name: "expired fallback IP then deny", prefixlen: 32},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ifindex := uint32(200 + i)
			allowInner, denyInner := env.attachInnerMaps(t, ifindex)
			daddr := mustParseCIDRForTest(t, "192.0.2.10").IP
			dport := htonsPort(443)
			allowKey := lpmKeyV3{Prefixlen: tt.prefixlen, IP: daddr}
			allowValue := netPolicyValueV3{ExpiresAtNS: 1}
			if tt.prefixlen == 48 {
				allowKey.Port = dport
				allowValue.Flags = uint8(netPolicyFlagL7Required)
				allowValue.Scheme = L7SchemeHTTPS
			}
			if err := allowInner.Put(&allowKey, &allowValue); err != nil {
				t.Fatalf("insert expired /%d allow rule: %v", tt.prefixlen, err)
			}

			denyAll := mustParseCIDRForTest(t, "0.0.0.0/0")
			denyValue := uint32(1)
			if err := denyInner.Put(&denyAll, &denyValue); err != nil {
				t.Fatalf("insert deny-all rule: %v", err)
			}

			got := runEgressPolicyCase(t, env.program, ifindex, daddr, dport)
			if got != flowVerdictReject {
				t.Fatalf("verdict=%d, want FLOW_REJECT", got)
			}
		})
	}
}
