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
	recheckProgram *ebpf.Program
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
		recheckProgram: coll.Programs["test_session_policy_revoked"],
		allowOut:       coll.Maps["allow_out_v3"],
		denyOut:        coll.Maps["deny_out"],
		allowInnerSpec: allowInnerSpec,
		denyInnerSpec:  denyInnerSpec,
	}
	if env.program == nil || env.recheckProgram == nil || env.allowOut == nil || env.denyOut == nil {
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

const sessionRecheckCaseLen = 28

// sessionRecheckCase mirrors struct session_recheck_case in
// egress_policy_test.bpf.c.
type sessionRecheckCase struct {
	ifindex           uint32
	daddr             uint32
	sessPolicyVersion uint32
	metaPolicyVersion uint32
	dport             uint16
	packetClass       uint8
	l7Scheme          uint8
}

type sessionRecheckResult struct {
	revoked           bool
	sessPolicyVersion uint32
}

func runSessionRecheckCase(t *testing.T, prog *ebpf.Program, tc sessionRecheckCase) sessionRecheckResult {
	t.Helper()

	data := make([]byte, sessionRecheckCaseLen)
	binary.LittleEndian.PutUint32(data[0:4], tc.ifindex)
	binary.LittleEndian.PutUint32(data[4:8], tc.daddr)
	binary.LittleEndian.PutUint32(data[8:12], tc.sessPolicyVersion)
	binary.LittleEndian.PutUint32(data[12:16], tc.metaPolicyVersion)
	binary.LittleEndian.PutUint16(data[20:22], tc.dport)
	data[22] = tc.packetClass
	data[23] = tc.l7Scheme

	ret, out, err := prog.Test(data)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF policy test-run unavailable: %v", err)
		}
		t.Fatalf("run session recheck test: %v", err)
	}
	if ret != 0 {
		t.Fatalf("test_session_policy_revoked returned %d, want TC_ACT_OK", ret)
	}
	if len(out) < sessionRecheckCaseLen {
		t.Fatalf("test output length=%d, want >=%d", len(out), sessionRecheckCaseLen)
	}
	return sessionRecheckResult{
		revoked:           out[24] != 0,
		sessPolicyVersion: binary.LittleEndian.Uint32(out[16:20]),
	}
}

// TestSessionPolicyRevoked exercises the datapath re-check an update relies on:
// an established flow is judged against the current policy the first time it is
// seen under a new generation, and the decision is then cached.
func TestSessionPolicyRevoked(t *testing.T) {
	const (
		ifindex     = uint32(700)
		snatPort    = uint16(0x5000) // 80, network byte order on little-endian
		otherPort   = uint16(0xBB01) // 443
		packetSNAT  = uint8(0)
		packetL7    = uint8(1)
		schemeNone  = uint8(0)
		schemeHTTP  = uint8(1)
		schemeHTTPS = uint8(2)
	)

	env := loadEgressPolicyTestEnv(t)
	allowInner, denyInner := env.attachInnerMaps(t, ifindex)

	allowed := mustParseCIDRForTest(t, "192.0.2.30")
	l7Host := mustParseCIDRForTest(t, "192.0.2.31")
	denied := mustParseCIDRForTest(t, "192.0.2.32")

	// classify_egress_flow defaults to SNAT, so "no longer allowed" has to be
	// expressed as an explicit deny rather than the absence of an allow.
	denyKey := lpmKey{Prefixlen: 32, IP: denied.IP}
	denyVal := uint32(netPolicyValueStatic)
	if err := denyInner.Update(&denyKey, &denyVal, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed deny: %v", err)
	}

	// A plain allow for one host, and an HTTPS-only L7 rule for another.
	plainKey := lpmKeyV3{Prefixlen: 32, IP: allowed.IP}
	if err := allowInner.Update(&plainKey, &netPolicyValueV3{KeyPrefixlen: 32}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed plain allow: %v", err)
	}
	l7Key := lpmKeyV3{Prefixlen: 48, IP: l7Host.IP, Port: otherPort}
	if err := allowInner.Update(&l7Key, &netPolicyValueV3{
		Flags: netPolicyFlagL7Required, Scheme: L7SchemeHTTPS, KeyPrefixlen: 48,
	}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed L7 allow: %v", err)
	}

	tests := []struct {
		name        string
		tc          sessionRecheckCase
		wantRevoked bool
		wantVersion uint32
	}{
		{
			// Denied by the current policy, but the generation matches, so the
			// flow keeps running on its cached verdict.
			name: "same generation is not re-evaluated",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: denied.IP, dport: snatPort,
				sessPolicyVersion: 5, metaPolicyVersion: 5,
			},
			wantRevoked: false,
			wantVersion: 5,
		},
		{
			name: "still allowed under the new generation is restamped",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: allowed.IP, dport: snatPort,
				sessPolicyVersion: 5, metaPolicyVersion: 6,
			},
			wantRevoked: false,
			wantVersion: 6,
		},
		{
			name: "no longer allowed is revoked",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: denied.IP, dport: snatPort,
				sessPolicyVersion: 5, metaPolicyVersion: 6,
			},
			wantRevoked: true,
			wantVersion: 5,
		},
		{
			// SNAT and L7 disagree on the reply tuple and on who terminates
			// the connection, so a flow cannot migrate between them.
			name: "verdict change SNAT to L7 is revoked",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: l7Host.IP, dport: otherPort,
				packetClass: packetSNAT, l7Scheme: schemeNone,
				sessPolicyVersion: 5, metaPolicyVersion: 6,
			},
			wantRevoked: true,
			wantVersion: 5,
		},
		{
			name: "verdict change L7 scheme is revoked",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: l7Host.IP, dport: otherPort,
				packetClass: packetL7, l7Scheme: schemeHTTP,
				sessPolicyVersion: 5, metaPolicyVersion: 6,
			},
			wantRevoked: true,
			wantVersion: 5,
		},
		{
			name: "unchanged L7 verdict is restamped",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: l7Host.IP, dport: otherPort,
				packetClass: packetL7, l7Scheme: schemeHTTPS,
				sessPolicyVersion: 5, metaPolicyVersion: 6,
			},
			wantRevoked: false,
			wantVersion: 6,
		},
		{
			// 0 is an ordinary generation, not a sentinel. A fresh TAP and the
			// sessions it stamps both sit at 0, as does everything written before
			// the field existed, so an upgrade must re-evaluate none of it: this
			// destination is denied by the current policy and still survives.
			name: "generation 0 on both sides is not re-evaluated",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: denied.IP, dport: snatPort,
				sessPolicyVersion: 0, metaPolicyVersion: 0,
			},
			wantRevoked: false,
			wantVersion: 0,
		},
		{
			name: "generation 0 against a newer one is re-evaluated",
			tc: sessionRecheckCase{
				ifindex: ifindex, daddr: allowed.IP, dport: snatPort,
				sessPolicyVersion: 0, metaPolicyVersion: 1,
			},
			wantRevoked: false,
			wantVersion: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runSessionRecheckCase(t, env.recheckProgram, tt.tc)
			if got.revoked != tt.wantRevoked {
				t.Errorf("revoked=%v, want %v", got.revoked, tt.wantRevoked)
			}
			if got.sessPolicyVersion != tt.wantVersion {
				t.Errorf("session policy_version=%d, want %d", got.sessPolicyVersion, tt.wantVersion)
			}
		})
	}
}
