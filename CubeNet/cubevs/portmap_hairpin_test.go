package cubevs

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH portmap ../src/portmap_test.bpf.c -- -I../vmlinux/$GOARCH

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/cilium/ebpf"
)

// Same-host port-mapping hairpin tests. They drive the production from_cube
// program (compiled into portmap_test.bpf.c with bpf_redirect recorded, not
// executed) with real Ethernet/IPv4/TCP packets via BPF_PROG_TEST_RUN.
//
// Address plan (all against the shipped .rodata defaults):
//   - node IP:        9.135.10.2  (nodenic_ip), nodenic_ifindex = 2
//   - cube-dev gw:    cubegw0_ifindex = 216
//   - guest inner IP: 169.254.68.6 (mvm_inner_ip)
//   - sandbox A:      192.168.0.10, sandbox B: 192.168.0.20
//   - mapping:        nodeIP:20080 -> B tap (ifindex 100) :80
//   - SNAT ports:     allocated upwards from 30000 (MAX_PORT_START)
const (
	tcActRedirectRet = 7
	bpfFIngressFlag  = 1

	hairpinNodeIP      = "9.135.10.2"
	hairpinNodeIfindex = uint32(2)
	hairpinGwIfindex   = uint32(216)
	hairpinInnerIP     = "169.254.68.6"
	hairpinVmAIP       = "192.168.0.10"
	hairpinVmBIP       = "192.168.0.20"
	hairpinCustomSnat  = "10.254.0.5" // route-aware mode SNAT address
	hairpinExternalIP  = "203.0.113.99"

	hairpinHostPort   = uint16(20080)
	hairpinListenPort = uint16(80)
	hairpinClientPort = uint16(40000)
	hairpinSnatPort   = uint16(30000)
	hairpinBTapIfidx  = uint32(100)

	tcpFlagFIN = 0x01
	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04
	tcpFlagACK = 0x10
)

type portmapTestEnv struct {
	coll       *ebpf.Collection
	fromCube   *ebpf.Program
	mvmMeta    *ebpf.Map
	remote     *ebpf.Map
	local      *ebpf.Map
	egress     *ebpf.Map
	ingress    *ebpf.Map
	snatIPs    *ebpf.Map
	allowOut   *ebpf.Map
	denyOut    *ebpf.Map
	redirect   *ebpf.Map
	allowInner *ebpf.MapSpec
	denyInner  *ebpf.MapSpec
	ifindex    uint32 // ifindex the kernel assigns to test-run skbs
}

func loadPortmapTestEnv(t *testing.T) *portmapTestEnv {
	t.Helper()

	spec, err := loadPortmap()
	if err != nil {
		t.Fatalf("load portmap test spec: %v", err)
	}
	allowSpec := spec.Maps["allow_out_v3"]
	denySpec := spec.Maps["deny_out"]
	if allowSpec == nil || allowSpec.InnerMap == nil || denySpec == nil || denySpec.InnerMap == nil {
		t.Fatal("policy map specs or inner templates missing")
	}
	allowInner := allowSpec.InnerMap.Copy()
	denyInner := denySpec.InnerMap.Copy()

	for _, mapSpec := range spec.Maps {
		mapSpec.Pinning = ebpf.PinNone
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF test unavailable: %v", err)
		}
		t.Fatalf("load portmap test collection: %v", err)
	}
	t.Cleanup(coll.Close)

	env := &portmapTestEnv{
		coll:       coll,
		fromCube:   coll.Programs["from_cube"],
		mvmMeta:    coll.Maps["ifindex_to_mvmmeta"],
		remote:     coll.Maps["remote_port_mapping"],
		local:      coll.Maps["local_port_mapping"],
		egress:     coll.Maps["egress_sessions"],
		ingress:    coll.Maps["ingress_sessions"],
		snatIPs:    coll.Maps["snat_iplist"],
		allowOut:   coll.Maps["allow_out_v3"],
		denyOut:    coll.Maps["deny_out"],
		redirect:   coll.Maps["redirect_log"],
		allowInner: allowInner,
		denyInner:  denyInner,
	}
	if env.fromCube == nil || env.mvmMeta == nil || env.remote == nil || env.local == nil ||
		env.egress == nil || env.ingress == nil || env.snatIPs == nil || env.allowOut == nil ||
		env.denyOut == nil || env.redirect == nil {
		t.Fatal("loaded portmap programs or maps missing")
	}

	probe := coll.Programs["probe_ifindex"]
	if probe == nil {
		t.Fatal("probe_ifindex program missing")
	}
	pkt := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, 1234, 5678, 0, 0, tcpFlagSYN)
	ret, _, err := probe.Test(pkt)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF test-run unavailable: %v", err)
		}
		t.Fatalf("run probe_ifindex: %v", err)
	}
	if ret != 0 {
		t.Fatalf("probe_ifindex returned %d, want TC_ACT_OK", ret)
	}
	env.ifindex = env.readRedirectLog(t).Ifindex
	return env
}

func (env *portmapTestEnv) readRedirectLog(t *testing.T) portmapRedirectRecord {
	t.Helper()
	var rec portmapRedirectRecord
	key := uint32(0)
	if err := env.redirect.Lookup(&key, &rec); err != nil {
		t.Fatalf("read redirect_log: %v", err)
	}
	return rec
}

func (env *portmapTestEnv) resetRedirectLog(t *testing.T) {
	t.Helper()
	key := uint32(0)
	if err := env.redirect.Put(&key, &portmapRedirectRecord{}); err != nil {
		t.Fatalf("reset redirect_log: %v", err)
	}
}

func (env *portmapTestEnv) setMvmMeta(t *testing.T, version uint32, vmIP string) {
	t.Helper()
	meta := portmapMvmMeta{Version: version, Ip: ipToUint32(net.ParseIP(vmIP))}
	if err := env.mvmMeta.Put(&env.ifindex, &meta); err != nil {
		t.Fatalf("set mvm_meta: %v", err)
	}
}

func (env *portmapTestEnv) setRemoteMapping(t *testing.T) {
	t.Helper()
	key := htons(hairpinHostPort)
	value := portmapMvmPort{Ifindex: hairpinBTapIfidx, ListenPort: htons(hairpinListenPort)}
	if err := env.remote.Put(&key, &value); err != nil {
		t.Fatalf("set remote_port_mapping: %v", err)
	}
}

func (env *portmapTestEnv) setLocalMapping(t *testing.T) {
	t.Helper()
	key := portmapMvmPort{Ifindex: env.ifindex, ListenPort: htons(hairpinListenPort)}
	value := htons(hairpinHostPort)
	if err := env.local.Put(&key, &value); err != nil {
		t.Fatalf("set local_port_mapping: %v", err)
	}
}

// setSnatIPs fills every slot: pick_snat_ip_port() hashes the sandbox IP into
// one of MAX_SNAT_IPS slots and tests must not depend on the hash.
func (env *portmapTestEnv) setSnatIPs(t *testing.T, snatIP string, nextPort uint16) {
	t.Helper()
	for i := uint32(0); i < 4; i++ {
		value := portmapSnatIp{Ifindex: hairpinNodeIfindex, Ip: ipToUint32(net.ParseIP(snatIP)), MaxPort: nextPort}
		if err := env.snatIPs.Put(&i, &value); err != nil {
			t.Fatalf("set snat_iplist[%d]: %v", i, err)
		}
	}
}

func (env *portmapTestEnv) newInnerMap(t *testing.T, spec *ebpf.MapSpec, name string) *ebpf.Map {
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

// denyAll attaches a deny_out inner map rejecting every destination.
func (env *portmapTestEnv) denyAll(t *testing.T) {
	t.Helper()
	inner := env.newInnerMap(t, env.denyInner, "deny_all")
	key := lpmKey{Prefixlen: 0, IP: 0}
	value := uint32(1)
	if err := inner.Put(&key, &value); err != nil {
		t.Fatalf("insert deny 0.0.0.0/0: %v", err)
	}
	if err := env.denyOut.Put(&env.ifindex, inner); err != nil {
		t.Fatalf("attach deny inner map: %v", err)
	}
}

// allowNodeIP attaches an allow_out_v3 inner map permitting the node IP.
func (env *portmapTestEnv) allowNodeIP(t *testing.T) {
	t.Helper()
	inner := env.newInnerMap(t, env.allowInner, "allow_node")
	key := lpmKeyV3{Prefixlen: 32, IP: ipToUint32(net.ParseIP(hairpinNodeIP))}
	value := netPolicyValueV3{KeyPrefixlen: 32}
	if err := inner.Put(&key, &value); err != nil {
		t.Fatalf("insert allow node IP: %v", err)
	}
	if err := env.allowOut.Put(&env.ifindex, inner); err != nil {
		t.Fatalf("attach allow inner map: %v", err)
	}
}

func buildTCPPacket(srcIP, dstIP string, sport, dport uint16, seq, ack uint32, tcpFlags uint8) []byte {
	pkt := make([]byte, 14+20+20)
	/* Ethernet: MACs are rewritten by the datapath where relevant; only the
	 * ethertype matters to from_cube. */
	copy(pkt[0:6], []byte{0x20, 0x90, 0x6f, 0xcf, 0xcf, 0xcf})
	copy(pkt[6:12], []byte{0x20, 0x90, 0x6f, 0xfc, 0xfc, 0xfc})
	binary.BigEndian.PutUint16(pkt[12:14], 0x0800)

	ip := pkt[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], 40)
	ip[8] = 64
	ip[9] = 6
	copy(ip[12:16], net.ParseIP(srcIP).To4())
	copy(ip[16:20], net.ParseIP(dstIP).To4())
	/* Checksums stay zero: the datapath only adjusts them incrementally and
	 * no one validates them under BPF_PROG_TEST_RUN. */

	tcp := pkt[34:54]
	binary.BigEndian.PutUint16(tcp[0:2], sport)
	binary.BigEndian.PutUint16(tcp[2:4], dport)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12] = 5 << 4
	tcp[13] = tcpFlags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	return pkt
}

type parsedTCPPacket struct {
	srcIP, dstIP  net.IP
	sport, dport  uint16
	rst, syn, ack bool
}

func parseTCPPacket(t *testing.T, pkt []byte) parsedTCPPacket {
	t.Helper()
	if len(pkt) < 54 {
		t.Fatalf("output packet too short: %d bytes", len(pkt))
	}
	return parsedTCPPacket{
		srcIP: net.IP(pkt[26:30]),
		dstIP: net.IP(pkt[30:34]),
		sport: binary.BigEndian.Uint16(pkt[34:36]),
		dport: binary.BigEndian.Uint16(pkt[36:38]),
		rst:   pkt[47]&tcpFlagRST != 0,
		syn:   pkt[47]&tcpFlagSYN != 0,
		ack:   pkt[47]&tcpFlagACK != 0,
	}
}

// runFromCube resets the redirect recorder and injects pkt into from_cube.
func (env *portmapTestEnv) runFromCube(t *testing.T, pkt []byte) (uint32, parsedTCPPacket, portmapRedirectRecord) {
	t.Helper()
	env.resetRedirectLog(t)
	ret, out, err := env.fromCube.Test(pkt)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF test-run unavailable: %v", err)
		}
		t.Fatalf("run from_cube: %v", err)
	}
	return ret, parseTCPPacket(t, out), env.readRedirectLog(t)
}

func (env *portmapTestEnv) egressSession(t *testing.T, srcIP, dstIP string,
	sport, dport uint16, version uint32,
) (portmapNatSession, bool) {
	t.Helper()
	key := portmapSessionKey{
		SrcIp:    ipToUint32(net.ParseIP(srcIP)),
		DstIp:    ipToUint32(net.ParseIP(dstIP)),
		SrcPort:  htons(sport),
		DstPort:  htons(dport),
		Version:  version,
		Protocol: ipprotoTCP,
	}
	var sess portmapNatSession
	if err := env.egress.Lookup(&key, &sess); err != nil {
		return portmapNatSession{}, false
	}
	return sess, true
}

func (env *portmapTestEnv) hasIngressSession(t *testing.T, srcIP, dstIP string,
	sport, dport uint16,
) bool {
	t.Helper()
	key := portmapSessionKey{
		SrcIp:   ipToUint32(net.ParseIP(srcIP)),
		DstIp:   ipToUint32(net.ParseIP(dstIP)),
		SrcPort: htons(sport),
		DstPort: htons(dport),
		// ingress session keys always carry version 0
		Protocol: ipprotoTCP,
	}
	var isess portmapIngressSession
	return env.ingress.Lookup(&key, &isess) == nil
}

// TestHairpinForwardSNATAndLoopback covers T1: a sandbox SYN to
// nodeIP:hostPort with a mapped port is egress-SNATed and looped back into
// the primary NIC ingress for the port-mapping DNAT.
func TestHairpinForwardSNATAndLoopback(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 7, hairpinVmAIP)
	env.setRemoteMapping(t)
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort) // direct mode: SNAT IP == node IP

	in := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 1000, 0, tcpFlagSYN)
	ret, out, rec := env.runFromCube(t, in)

	if ret != tcActRedirectRet {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}
	if rec.Calls != 1 || rec.Ifindex != hairpinNodeIfindex || rec.Flags != bpfFIngressFlag {
		t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d %d} (nodenic ingress loopback)",
			rec.Calls, rec.Ifindex, rec.Flags, hairpinNodeIfindex, bpfFIngressFlag)
	}
	if got := out.srcIP.String(); got != hairpinNodeIP {
		t.Fatalf("SNAT src IP=%s, want %s", got, hairpinNodeIP)
	}
	if out.sport != hairpinSnatPort {
		t.Fatalf("SNAT src port=%d, want %d", out.sport, hairpinSnatPort)
	}
	if got := out.dstIP.String(); got != hairpinNodeIP || out.dport != hairpinHostPort {
		t.Fatalf("dst=%s:%d, want unchanged %s:%d", out.dstIP.String(), out.dport, hairpinNodeIP, hairpinHostPort)
	}

	sess, ok := env.egressSession(t, hairpinVmAIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 7)
	if !ok {
		t.Fatal("egress session for the hairpin flow missing")
	}
	if sess.NodeIp != ipToUint32(net.ParseIP(hairpinNodeIP)) || ntohs(sess.NodePort) != hairpinSnatPort {
		t.Fatalf("session node tuple=%s:%d, want %s:%d",
			uint32ToIP(sess.NodeIp), ntohs(sess.NodePort), hairpinNodeIP, hairpinSnatPort)
	}
	if sess.VmIfindex != env.ifindex {
		t.Fatalf("session vm_ifindex=%d, want %d", sess.VmIfindex, env.ifindex)
	}
	if !env.hasIngressSession(t, hairpinNodeIP, hairpinNodeIP, hairpinHostPort, hairpinSnatPort) {
		t.Fatal("ingress session for the hairpin flow missing")
	}
}

// TestHairpinForwardDeniedByPolicy covers T2: egress policy applies to
// hairpin traffic; a deny-all sandbox gets an RST, not a silent hairpin.
func TestHairpinForwardDeniedByPolicy(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 7, hairpinVmAIP)
	env.setRemoteMapping(t)
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)
	env.denyAll(t)

	in := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 1000, 0, tcpFlagSYN)
	ret, out, rec := env.runFromCube(t, in)

	if ret != tcActRedirectRet {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT (RST)", ret)
	}
	if rec.Calls != 1 || rec.Ifindex != env.ifindex || rec.Flags != 0 {
		t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d 0} (RST back to the guest TAP)",
			rec.Calls, rec.Ifindex, rec.Flags, env.ifindex)
	}
	if !out.rst {
		t.Fatalf("output is not an RST (flags: syn=%v ack=%v)", out.syn, out.ack)
	}
	if got := out.srcIP.String(); got != hairpinNodeIP || out.sport != hairpinHostPort {
		t.Fatalf("RST src=%s:%d, want %s:%d", out.srcIP.String(), out.sport, hairpinNodeIP, hairpinHostPort)
	}
	if got := out.dstIP.String(); got != hairpinInnerIP || out.dport != hairpinClientPort {
		t.Fatalf("RST dst=%s:%d, want %s:%d", out.dstIP.String(), out.dport, hairpinInnerIP, hairpinClientPort)
	}
	if _, ok := env.egressSession(t, hairpinVmAIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 7); ok {
		t.Fatal("denied flow must not create an egress session")
	}
}

// TestHairpinForwardAllowedByRule covers T3: an allowOut rule for the node IP
// takes precedence over deny-all and lets the hairpin through.
func TestHairpinForwardAllowedByRule(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 7, hairpinVmAIP)
	env.setRemoteMapping(t)
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)
	env.denyAll(t)
	env.allowNodeIP(t)

	in := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 1000, 0, tcpFlagSYN)
	ret, out, rec := env.runFromCube(t, in)

	if ret != tcActRedirectRet {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}
	if rec.Calls != 1 || rec.Ifindex != hairpinNodeIfindex || rec.Flags != bpfFIngressFlag {
		t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d %d}",
			rec.Calls, rec.Ifindex, rec.Flags, hairpinNodeIfindex, bpfFIngressFlag)
	}
	if out.sport != hairpinSnatPort {
		t.Fatalf("SNAT src port=%d, want %d", out.sport, hairpinSnatPort)
	}
}

// TestNodeIPTrafficWithoutMappingUnchanged covers T4: nodeIP traffic whose
// port is not mapped follows the pre-hairpin behavior (injected into the host
// stack via cube-dev, or RST when policy denies it).
func TestNodeIPTrafficWithoutMappingUnchanged(t *testing.T) {
	t.Run("policy allow injects to cubegw0", func(t *testing.T) {
		env := loadPortmapTestEnv(t)
		env.setMvmMeta(t, 7, hairpinVmAIP)
		env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)

		in := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, 9999, 1000, 0, tcpFlagSYN)
		ret, _, rec := env.runFromCube(t, in)

		if ret != tcActRedirectRet {
			t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
		}
		if rec.Calls != 1 || rec.Ifindex != hairpinGwIfindex || rec.Flags != bpfFIngressFlag {
			t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d %d} (cubegw0)",
				rec.Calls, rec.Ifindex, rec.Flags, hairpinGwIfindex, bpfFIngressFlag)
		}
		if _, ok := env.egressSession(t, hairpinVmAIP, hairpinNodeIP, hairpinClientPort, 9999, 7); ok {
			t.Fatal("unmapped nodeIP traffic must not create an egress session")
		}
	})

	t.Run("policy deny answers RST", func(t *testing.T) {
		env := loadPortmapTestEnv(t)
		env.setMvmMeta(t, 7, hairpinVmAIP)
		env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)
		env.denyAll(t)

		in := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, 9999, 1000, 0, tcpFlagSYN)
		ret, out, rec := env.runFromCube(t, in)

		if ret != tcActRedirectRet {
			t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT (RST)", ret)
		}
		if rec.Calls != 1 || rec.Ifindex != env.ifindex || rec.Flags != 0 {
			t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d 0}",
				rec.Calls, rec.Ifindex, rec.Flags, env.ifindex)
		}
		if !out.rst {
			t.Fatal("output is not an RST")
		}
	})
}

// TestHairpinReplyLoopsBack covers T5: a mapped port's reply whose peer is a
// local SNAT address (same-host caller) loops back into the primary NIC
// ingress instead of egressing the wire. Runs direct (SNAT IP == node IP) and
// route-aware (SNAT IP = cubeRouter address) parameterizations.
func TestHairpinReplyLoopsBack(t *testing.T) {
	tests := []struct {
		name   string
		snatIP string
		peerIP string
	}{
		{"direct mode peer is nodenic_ip", hairpinNodeIP, hairpinNodeIP},
		{"route-aware peer is cubeRouter SNAT IP", hairpinCustomSnat, hairpinCustomSnat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := loadPortmapTestEnv(t)
			env.setMvmMeta(t, 3, hairpinVmBIP)
			env.setLocalMapping(t)
			env.setSnatIPs(t, tt.snatIP, hairpinSnatPort)

			in := buildTCPPacket(hairpinInnerIP, tt.peerIP, hairpinListenPort, hairpinSnatPort, 5000, 1001, tcpFlagSYN|tcpFlagACK)
			ret, out, rec := env.runFromCube(t, in)

			if ret != tcActRedirectRet {
				t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
			}
			if rec.Calls != 1 || rec.Ifindex != hairpinNodeIfindex || rec.Flags != bpfFIngressFlag {
				t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d %d} (NIC ingress loopback)",
					rec.Calls, rec.Ifindex, rec.Flags, hairpinNodeIfindex, bpfFIngressFlag)
			}
			if got := out.srcIP.String(); got != hairpinNodeIP || out.sport != hairpinHostPort {
				t.Fatalf("reply src=%s:%d, want %s:%d", out.srcIP.String(), out.sport, hairpinNodeIP, hairpinHostPort)
			}
			if got := out.dstIP.String(); got != tt.peerIP || out.dport != hairpinSnatPort {
				t.Fatalf("reply dst=%s:%d, want unchanged %s:%d", out.dstIP.String(), out.dport, tt.peerIP, hairpinSnatPort)
			}
		})
	}
}

// TestPortMappingReplyToExternalClientUnchanged covers T6: replies to
// external clients keep egressing the physical NIC.
func TestPortMappingReplyToExternalClientUnchanged(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 3, hairpinVmBIP)
	env.setLocalMapping(t)
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)

	in := buildTCPPacket(hairpinInnerIP, hairpinExternalIP, hairpinListenPort, 5000, 5000, 1001, tcpFlagSYN|tcpFlagACK)
	ret, out, rec := env.runFromCube(t, in)

	if ret != tcActRedirectRet {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}
	if rec.Calls != 1 || rec.Ifindex != hairpinNodeIfindex || rec.Flags != 0 {
		t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d 0} (wire egress)",
			rec.Calls, rec.Ifindex, rec.Flags, hairpinNodeIfindex)
	}
	if got := out.srcIP.String(); got != hairpinNodeIP || out.sport != hairpinHostPort {
		t.Fatalf("reply src=%s:%d, want %s:%d", out.srcIP.String(), out.sport, hairpinNodeIP, hairpinHostPort)
	}
}

// TestHairpinReplySYNRejected covers the guest-side guard: a mapped listen
// port must never originate a connection.
func TestHairpinReplySYNRejected(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 3, hairpinVmBIP)
	env.setLocalMapping(t)
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)

	in := buildTCPPacket(hairpinInnerIP, hairpinExternalIP, hairpinListenPort, 5000, 5000, 0, tcpFlagSYN)
	ret, _, rec := env.runFromCube(t, in)

	if ret != 2 { // TC_ACT_SHOT
		t.Fatalf("from_cube returned %d, want TC_ACT_SHOT", ret)
	}
	if rec.Calls != 0 {
		t.Fatalf("redirect recorded %+v, want none", rec)
	}
}

// TestHairpinEstablishedFlowKeepsSession covers T7: subsequent packets reuse
// the session created by the SYN and keep the same SNAT port.
func TestHairpinEstablishedFlowKeepsSession(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 7, hairpinVmAIP)
	env.setRemoteMapping(t)
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort)

	syn := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 1000, 0, tcpFlagSYN)
	if _, _, rec := env.runFromCube(t, syn); rec.Flags != bpfFIngressFlag {
		t.Fatalf("SYN redirect flags=%d, want INGRESS", rec.Flags)
	}

	ack := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 1001, 6001, tcpFlagACK)
	ret, out, rec := env.runFromCube(t, ack)
	if ret != tcActRedirectRet {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}
	if rec.Calls != 1 || rec.Ifindex != hairpinNodeIfindex || rec.Flags != bpfFIngressFlag {
		t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d %d}",
			rec.Calls, rec.Ifindex, rec.Flags, hairpinNodeIfindex, bpfFIngressFlag)
	}
	if out.sport != hairpinSnatPort {
		t.Fatalf("established flow src port=%d, want reused %d", out.sport, hairpinSnatPort)
	}
}

// TestHairpinTimeWaitTupleReuse covers T9: a SYN reusing a TIME_WAIT tuple
// retires the old session pair and allocates a fresh SNAT port.
func TestHairpinTimeWaitTupleReuse(t *testing.T) {
	env := loadPortmapTestEnv(t)
	env.setMvmMeta(t, 7, hairpinVmAIP)
	env.setRemoteMapping(t)
	// Simulate the allocator having advanced past the port held by the
	// TIME_WAIT session.
	env.setSnatIPs(t, hairpinNodeIP, hairpinSnatPort+1)

	oldSess := portmapNatSession{
		NodeIfindex: hairpinNodeIfindex,
		NodeIp:      ipToUint32(net.ParseIP(hairpinNodeIP)),
		VmIfindex:   env.ifindex,
		VmIp:        ipToUint32(net.ParseIP(hairpinVmAIP)),
		NodePort:    htons(hairpinSnatPort),
		VmPort:      htons(hairpinClientPort),
		State:       uint8(tcpCTTimeWait),
		Gen:         7,
	}
	ekey := portmapSessionKey{
		SrcIp:    ipToUint32(net.ParseIP(hairpinVmAIP)),
		DstIp:    ipToUint32(net.ParseIP(hairpinNodeIP)),
		SrcPort:  htons(hairpinClientPort),
		DstPort:  htons(hairpinHostPort),
		Version:  7,
		Protocol: ipprotoTCP,
	}
	if err := env.egress.Put(&ekey, &oldSess); err != nil {
		t.Fatalf("prepopulate TIME_WAIT egress session: %v", err)
	}

	in := buildTCPPacket(hairpinInnerIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 1000, 0, tcpFlagSYN)
	ret, out, rec := env.runFromCube(t, in)

	if ret != tcActRedirectRet {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}
	if rec.Calls != 1 || rec.Ifindex != hairpinNodeIfindex || rec.Flags != bpfFIngressFlag {
		t.Fatalf("redirect={calls:%d ifindex:%d flags:%d}, want {1 %d %d}",
			rec.Calls, rec.Ifindex, rec.Flags, hairpinNodeIfindex, bpfFIngressFlag)
	}
	if out.sport != hairpinSnatPort+1 {
		t.Fatalf("recreated flow src port=%d, want fresh %d", out.sport, hairpinSnatPort+1)
	}
	sess, ok := env.egressSession(t, hairpinVmAIP, hairpinNodeIP, hairpinClientPort, hairpinHostPort, 7)
	if !ok {
		t.Fatal("recreated egress session missing")
	}
	if sess.State == uint8(tcpCTTimeWait) || ntohs(sess.NodePort) != hairpinSnatPort+1 {
		t.Fatalf("session state=%d node_port=%d, want fresh SYN-session on port %d",
			sess.State, ntohs(sess.NodePort), hairpinSnatPort+1)
	}
}
