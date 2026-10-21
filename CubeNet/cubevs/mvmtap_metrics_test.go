package cubevs

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/cilium/ebpf"
)

const tcActRedirect = uint32(7)

type mvmtapMetricsTestEnv struct {
	fromCube      *ebpf.Program
	mvmMetadata   *ebpf.Map
	sandboxMetric *ebpf.Map
	allowOut      *ebpf.Map
	allowInner    *ebpf.MapSpec
}

func loadMvmtapMetricsTestEnv(t testing.TB) *mvmtapMetricsTestEnv {
	t.Helper()

	spec, err := loadMvmtap()
	if err != nil {
		t.Fatalf("load mvmtap spec: %v", err)
	}
	allowSpec := spec.Maps["allow_out_v3"]
	if allowSpec == nil || allowSpec.InnerMap == nil {
		t.Fatal("allow_out_v3 map or inner template missing")
	}
	allowInner := allowSpec.InnerMap.Copy()
	for name := range spec.Programs {
		if name != "from_cube" {
			delete(spec.Programs, name)
		}
	}
	for _, mapSpec := range spec.Maps {
		mapSpec.Pinning = ebpf.PinNone
		switch mapSpec.Type {
		case ebpf.Hash, ebpf.LRUHash, ebpf.PerCPUHash:
			mapSpec.MaxEntries = 64
		}
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF mvmtap metrics test unavailable: %v", err)
		}
		t.Fatalf("load mvmtap metrics test collection: %v", err)
	}
	t.Cleanup(coll.Close)

	env := &mvmtapMetricsTestEnv{
		fromCube:      coll.Programs["from_cube"],
		mvmMetadata:   coll.Maps["ifindex_to_mvmmeta"],
		sandboxMetric: coll.Maps["sandbox_metrics"],
		allowOut:      coll.Maps["allow_out_v3"],
		allowInner:    allowInner,
	}
	if env.fromCube == nil || env.mvmMetadata == nil || env.sandboxMetric == nil || env.allowOut == nil {
		t.Fatal("mvmtap metrics test program or maps missing")
	}
	return env
}

func (env *mvmtapMetricsTestEnv) registerSandbox(t testing.TB, sandboxIP uint32) {
	t.Helper()
	ifindex := uint32(0) // BPF_PROG_TEST_RUN uses ingress_ifindex 0 by default.
	meta := mvmtapMvmMeta{Ip: sandboxIP}
	if err := env.mvmMetadata.Put(&ifindex, &meta); err != nil {
		t.Fatalf("register sandbox metadata: %v", err)
	}
	value := sandboxMetricsValue{}
	var mapValue interface{} = &value
	if env.sandboxMetric.Type() == ebpf.PerCPUHash {
		mapValue = make([]sandboxMetricsValue, ebpf.MustPossibleCPU())
	}
	if err := env.sandboxMetric.Put(&sandboxIP, mapValue); err != nil {
		t.Fatalf("register sandbox metrics: %v", err)
	}
}

func (env *mvmtapMetricsTestEnv) allowL7(t testing.TB, targetIP uint32, targetPort uint16) {
	t.Helper()
	innerSpec := env.allowInner.Copy()
	innerSpec.Name = "metrics_l7"
	innerSpec.Pinning = ebpf.PinNone
	inner, err := ebpf.NewMap(innerSpec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF L7 policy map unavailable: %v", err)
		}
		t.Fatalf("create L7 policy map: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	key := lpmKeyV3{Prefixlen: 48, IP: targetIP, Port: targetPort}
	value := netPolicyValueV3{Flags: netPolicyFlagL7Required, Scheme: L7SchemeHTTPS}
	if err := inner.Put(&key, &value); err != nil {
		t.Fatalf("insert L7 policy: %v", err)
	}
	ifindex := uint32(0)
	if err := env.allowOut.Put(&ifindex, inner); err != nil {
		t.Fatalf("attach L7 policy map: %v", err)
	}
}

func (env *mvmtapMetricsTestEnv) runAndReadEgress(t testing.TB, packet []byte, sandboxIP uint32) sandboxMetricsValue {
	t.Helper()
	ret, _, err := env.fromCube.Test(packet)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF mvmtap test-run unavailable: %v", err)
		}
		t.Fatalf("run from_cube: %v", err)
	}
	if ret != tcActRedirect {
		t.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}

	if env.sandboxMetric.Type() == ebpf.PerCPUHash {
		values := make([]sandboxMetricsValue, ebpf.MustPossibleCPU())
		if err := env.sandboxMetric.Lookup(&sandboxIP, &values); err != nil {
			t.Fatalf("read per-CPU sandbox metrics: %v", err)
		}
		var total sandboxMetricsValue
		for _, value := range values {
			total.IngressPackets += value.IngressPackets
			total.IngressBytes += value.IngressBytes
			total.IngressDropPackets += value.IngressDropPackets
			total.IngressDropBytes += value.IngressDropBytes
			total.EgressPackets += value.EgressPackets
			total.EgressBytes += value.EgressBytes
			total.EgressDropPackets += value.EgressDropPackets
			total.EgressDropBytes += value.EgressDropBytes
			total.SNATAllocFailures += value.SNATAllocFailures
		}
		return total
	}

	var value sandboxMetricsValue
	if err := env.sandboxMetric.Lookup(&sandboxIP, &value); err != nil {
		t.Fatalf("read sandbox metrics: %v", err)
	}
	return value
}

func ipv4TCPPacket(sourceIP, targetIP net.IP, sourcePort, targetPort uint16, flags byte) []byte {
	const (
		ethernetHeaderLen = 14
		ipv4HeaderLen     = 20
		tcpHeaderLen      = 20
	)
	packet := make([]byte, ethernetHeaderLen+ipv4HeaderLen+tcpHeaderLen)
	binary.BigEndian.PutUint16(packet[12:14], 0x0800)

	ip := packet[ethernetHeaderLen:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], ipv4HeaderLen+tcpHeaderLen)
	ip[8] = 64
	ip[9] = ipprotoTCP
	copy(ip[12:16], sourceIP.To4())
	copy(ip[16:20], targetIP.To4())

	tcp := ip[ipv4HeaderLen:]
	binary.BigEndian.PutUint16(tcp[0:2], sourcePort)
	binary.BigEndian.PutUint16(tcp[2:4], targetPort)
	tcp[12] = 5 << 4
	tcp[13] = flags
	return packet
}

func TestMvmtapSuccessfulSpecialEgressPathsCountTraffic(t *testing.T) {
	sandboxIP := ipToUint32(net.ParseIP("192.168.4.12"))
	tests := []struct {
		name      string
		targetIP  net.IP
		tcpFlags  byte
		setupPath func(*testing.T, *mvmtapMetricsTestEnv, uint32, uint16)
	}{
		{
			name:     "sandbox gateway",
			targetIP: net.ParseIP("169.254.68.5"),
			tcpFlags: 0x10,
		},
		{
			name:     "node local gateway",
			targetIP: net.ParseIP("9.135.10.2"),
			tcpFlags: 0x10,
		},
		{
			name:     "L7 proxy",
			targetIP: net.ParseIP("198.51.100.20"),
			tcpFlags: 0x02,
			setupPath: func(t *testing.T, env *mvmtapMetricsTestEnv, targetIP uint32, targetPort uint16) {
				env.allowL7(t, targetIP, targetPort)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := loadMvmtapMetricsTestEnv(t)
			env.registerSandbox(t, sandboxIP)

			targetIP := ipToUint32(test.targetIP)
			targetPort := htonsPort(443)
			if test.setupPath != nil {
				test.setupPath(t, env, targetIP, targetPort)
			}
			packet := ipv4TCPPacket(
				net.ParseIP("169.254.68.6"),
				test.targetIP,
				12345,
				443,
				test.tcpFlags,
			)

			metrics := env.runAndReadEgress(t, packet, sandboxIP)
			if metrics.EgressPackets != 1 || metrics.EgressBytes != uint64(len(packet)) {
				t.Fatalf("egress metrics packets=%d bytes=%d, want packets=1 bytes=%d",
					metrics.EgressPackets, metrics.EgressBytes, len(packet))
			}
			if metrics.EgressDropPackets != 0 || metrics.EgressDropBytes != 0 {
				t.Fatalf("unexpected egress drop metrics: packets=%d bytes=%d",
					metrics.EgressDropPackets, metrics.EgressDropBytes)
			}
		})
	}
}

// BenchmarkMvmtapForwardedPacket measures the in-kernel from_cube path,
// including the two metric counter updates for a forwarded packet. Run this
// benchmark unchanged on both map implementations for a direct comparison.
func BenchmarkMvmtapForwardedPacket(b *testing.B) {
	env := loadMvmtapMetricsTestEnv(b)
	sandboxIP := ipToUint32(net.ParseIP("192.168.4.12"))
	env.registerSandbox(b, sandboxIP)
	packet := ipv4TCPPacket(
		net.ParseIP("169.254.68.6"),
		net.ParseIP("169.254.68.5"),
		12345,
		443,
		0x10,
	)

	b.ResetTimer()
	ret, duration, err := env.fromCube.Benchmark(packet, b.N, nil)
	if err != nil {
		if bpfTestUnavailable(err) {
			b.Skipf("kernel BPF benchmark unavailable: %v", err)
		}
		b.Fatalf("benchmark from_cube: %v", err)
	}
	if ret != tcActRedirect {
		b.Fatalf("from_cube returned %d, want TC_ACT_REDIRECT", ret)
	}
	b.ReportMetric(float64(duration.Nanoseconds()), "kernel-ns/op")
}
