package cubevs

import (
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/cilium/ebpf"
)

type fakeMetricsUpdater struct {
	value sandboxMetricsValue
	flags ebpf.MapUpdateFlags
	err   error
}

func (f *fakeMetricsUpdater) Update(_ interface{}, value interface{}, flags ebpf.MapUpdateFlags) error {
	f.value = *value.(*sandboxMetricsValue)
	f.flags = flags
	return f.err
}

type fakeMetricsDeleter struct {
	entries   map[uint32]struct{}
	deleteErr error
}

func (f *fakeMetricsDeleter) Delete(key interface{}) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	typedKey := *key.(*uint32)
	if _, ok := f.entries[typedKey]; !ok {
		return ebpf.ErrKeyNotExist
	}
	delete(f.entries, typedKey)
	return nil
}

func TestReadSNATAllocationMetricsUsesConfiguredReader(t *testing.T) {
	original := readSNATAllocationMetricsFn
	t.Cleanup(func() {
		readSNATAllocationMetricsFn = original
	})

	want := []SNATAllocationMetrics{{
		SNATIP:        "203.0.113.9",
		PortsCapacity: 35536,
		PortsUnused:   35535,
	}}
	readSNATAllocationMetricsFn = func() ([]SNATAllocationMetrics, error) {
		return want, nil
	}

	got, err := ReadSNATAllocationMetrics()
	if err != nil {
		t.Fatalf("ReadSNATAllocationMetrics returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadSNATAllocationMetrics mismatch: got=%#v want=%#v", got, want)
	}
}

func TestReadSessionMetricsUsesConfiguredReader(t *testing.T) {
	original := readSessionMetricsFn
	t.Cleanup(func() {
		readSessionMetricsFn = original
	})

	want := []SessionMetrics{{SandboxIP: "192.0.2.10", PacketClass: "l7_proxy", SessionsInUse: 3}}
	readSessionMetricsFn = func() ([]SessionMetrics, error) {
		return want, nil
	}

	got, err := ReadSessionMetrics()
	if err != nil {
		t.Fatalf("ReadSessionMetrics returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadSessionMetrics mismatch: got=%#v want=%#v", got, want)
	}
}

func TestReadSessionMetricsGroupsBySandboxIPAndPacketClass(t *testing.T) {
	original := cachedSessionUsage.Load()
	t.Cleanup(func() {
		cachedSessionUsage.Store(original)
	})

	sandboxIP1 := ipToUint32(net.ParseIP("192.0.2.1"))
	sandboxIP2 := ipToUint32(net.ParseIP("192.0.2.2"))
	publishSessionUsage(nil, map[sessionUsageKey]uint64{
		{sandboxIP: sandboxIP2, packetClass: packetClassSNAT}:    3,
		{sandboxIP: sandboxIP1, packetClass: packetClassSNAT}:    2,
		{sandboxIP: sandboxIP1, packetClass: packetClassL7Proxy}: 1,
	}, time.Now())

	got, err := readSessionMetrics()
	if err != nil {
		t.Fatalf("readSessionMetrics returned error: %v", err)
	}
	want := []SessionMetrics{
		{SandboxIP: "192.0.2.1", PacketClass: "l7_proxy", SessionsInUse: 1},
		{SandboxIP: "192.0.2.1", PacketClass: "snat", SessionsInUse: 2},
		{SandboxIP: "192.0.2.2", PacketClass: "snat", SessionsInUse: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session metrics mismatch: got=%#v want=%#v", got, want)
	}
}

func TestDeleteSandboxTrafficMetricsEntry(t *testing.T) {
	const sandboxIP = uint32(42)
	m := &fakeMetricsDeleter{entries: map[uint32]struct{}{sandboxIP: {}}}

	if err := deleteSandboxTrafficMetricsEntry(m, sandboxIP); err != nil {
		t.Fatalf("deleteSandboxTrafficMetricsEntry returned error: %v", err)
	}
	if _, exists := m.entries[sandboxIP]; exists {
		t.Fatal("sandbox metrics entry still exists after delete")
	}
	if err := deleteSandboxTrafficMetricsEntry(m, sandboxIP); err != nil {
		t.Fatalf("repeated delete returned error: %v", err)
	}
}

func TestDeleteSandboxTrafficMetricsEntryPropagatesError(t *testing.T) {
	wantErr := errors.New("delete failed")
	m := &fakeMetricsDeleter{deleteErr: wantErr}

	if err := deleteSandboxTrafficMetricsEntry(m, 42); !errors.Is(err, wantErr) {
		t.Fatalf("delete error=%v, want=%v", err, wantErr)
	}
}

func TestEnsureSandboxTrafficMetricsEntryResetsToZero(t *testing.T) {
	m := &fakeMetricsUpdater{
		value: sandboxMetricsValue{IngressPackets: 7},
	}

	if err := ensureSandboxTrafficMetricsEntry(m, 42); err != nil {
		t.Fatalf("ensureSandboxTrafficMetricsEntry returned error: %v", err)
	}
	if m.flags != ebpf.UpdateAny {
		t.Fatalf("update flags=%d, want UpdateAny", m.flags)
	}
	if m.value != (sandboxMetricsValue{}) {
		t.Fatalf("metrics=%+v, want zero", m.value)
	}
}

func TestEnsureSandboxTrafficMetricsEntryPropagatesError(t *testing.T) {
	wantErr := errors.New("update failed")
	m := &fakeMetricsUpdater{err: wantErr}

	if err := ensureSandboxTrafficMetricsEntry(m, 42); !errors.Is(err, wantErr) {
		t.Fatalf("update error=%v, want=%v", err, wantErr)
	}
}

func TestBuildSNATAllocationMetricsDeduplicatesReplicatedIPSlots(t *testing.T) {
	ip := ipToUint32(net.ParseIP("203.0.113.9"))
	values := []snatIP{
		{Ifindex: 7, IP: ip, MaxPort: 30001},
		{Ifindex: 7, IP: ip, MaxPort: 30002},
		{Ifindex: 7, IP: ip, MaxPort: 30003},
		{Ifindex: 7, IP: ip, MaxPort: 30004},
	}

	got := buildSNATAllocationMetrics(values, map[uint32]snatPortUsage{
		ip: {
			portsInUse: 5,
		},
	})
	if len(got) != 1 {
		t.Fatalf("metrics entries=%d, want 1: %#v", len(got), got)
	}

	want := SNATAllocationMetrics{
		SNATIP:        "203.0.113.9",
		Ifindex:       7,
		PortsCapacity: 35536,
		PortsUnused:   35531,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("metrics mismatch: got=%#v want=%#v", got[0], want)
	}
}

func TestBuildSNATAllocationMetricsDoesNotSubtractTotalSessions(t *testing.T) {
	ip := ipToUint32(net.ParseIP("203.0.113.9"))
	values := []snatIP{{Ifindex: 7, IP: ip, MaxPort: 30001}}
	usage := map[uint32]snatPortUsage{
		ip: {
			portsInUse: 20000,
		},
	}

	got := buildSNATAllocationMetrics(values, usage)
	if len(got) != 1 {
		t.Fatalf("metrics entries=%d, want 1: %#v", len(got), got)
	}
	if got[0].PortsUnused != 15536 {
		t.Fatalf("unused ports=%d, want 15536", got[0].PortsUnused)
	}
}

func TestRecordSessionUsageCountsClassesAndDistinctSNATPorts(t *testing.T) {
	const (
		nodeIP     = uint32(1)
		sandboxIP1 = uint32(2)
		sandboxIP2 = uint32(3)
	)
	snatPortUsageByNodeIP := make(map[uint32]snatPortUsage)
	sessionsBySandboxIPAndPacketClass := make(map[sessionUsageKey]uint64)
	portsInUse := make(map[snatPort]struct{})
	sessions := []natSession{
		{NodeIP: nodeIP, NodePort: 30000, VMIP: sandboxIP1, PacketClass: packetClassSNAT},
		{NodeIP: nodeIP, NodePort: 30001, VMIP: sandboxIP1, PacketClass: packetClassSNAT},
		{NodeIP: nodeIP, NodePort: 30000, VMIP: sandboxIP2, PacketClass: packetClassSNAT},
		{NodeIP: nodeIP, NodePort: 30002, VMIP: sandboxIP1, PacketClass: packetClassHairpin},
		{NodeIP: nodeIP, NodePort: 30001, VMIP: sandboxIP2, PacketClass: packetClassHairpin},
		{NodeIP: nodeIP, NodePort: 30003, VMIP: sandboxIP1, PacketClass: packetClassL7Proxy},
		{NodeIP: nodeIP, NodePort: 30004, VMIP: sandboxIP2, PacketClass: packetClassPortMapping},
	}
	for i := range sessions {
		recordSessionUsage(snatPortUsageByNodeIP, sessionsBySandboxIPAndPacketClass, portsInUse, &sessions[i])
	}

	want := snatPortUsage{portsInUse: 3}
	if got := snatPortUsageByNodeIP[nodeIP]; got != want {
		t.Fatalf("usage=%+v, want=%+v", got, want)
	}
	wantSessions := map[sessionUsageKey]uint64{
		{sandboxIP: sandboxIP1, packetClass: packetClassSNAT}:        2,
		{sandboxIP: sandboxIP1, packetClass: packetClassL7Proxy}:     1,
		{sandboxIP: sandboxIP1, packetClass: packetClassHairpin}:     1,
		{sandboxIP: sandboxIP2, packetClass: packetClassSNAT}:        1,
		{sandboxIP: sandboxIP2, packetClass: packetClassPortMapping}: 1,
		{sandboxIP: sandboxIP2, packetClass: packetClassHairpin}:     1,
	}
	if !reflect.DeepEqual(sessionsBySandboxIPAndPacketClass, wantSessions) {
		t.Fatalf("sessions by sandbox IP and packet class=%v, want=%v", sessionsBySandboxIPAndPacketClass, wantSessions)
	}
}

func TestReadCachedSNATPortUsageRequiresSnapshot(t *testing.T) {
	original := cachedSessionUsage.Load()
	invalidateSessionUsage()
	t.Cleanup(func() {
		cachedSessionUsage.Store(original)
	})

	if _, err := readCachedSNATPortUsage(time.Now()); err == nil {
		t.Fatal("readCachedSNATPortUsage returned nil error without a snapshot")
	}
}

func TestInvalidateSessionUsageDropsPublishedSnapshot(t *testing.T) {
	original := cachedSessionUsage.Load()
	t.Cleanup(func() {
		cachedSessionUsage.Store(original)
	})

	now := time.Now()
	publishSessionUsage(map[uint32]snatPortUsage{
		1: {portsInUse: 3},
	}, map[sessionUsageKey]uint64{{sandboxIP: 2, packetClass: packetClassSNAT}: 7}, now)
	invalidateSessionUsage()

	if _, err := readCachedSNATPortUsage(now); err == nil {
		t.Fatal("readCachedSNATPortUsage returned nil error after invalidation")
	}
}

func TestSessionUsageSnapshotIsImmutable(t *testing.T) {
	original := cachedSessionUsage.Load()
	t.Cleanup(func() {
		cachedSessionUsage.Store(original)
	})

	now := time.Now()
	source := map[uint32]snatPortUsage{
		1: {portsInUse: 3},
	}
	sessionKey := sessionUsageKey{sandboxIP: 2, packetClass: packetClassSNAT}
	sessionSource := map[sessionUsageKey]uint64{sessionKey: 7}
	publishSessionUsage(source, sessionSource, now)
	source[1] = snatPortUsage{portsInUse: 99}
	sessionSource[sessionKey] = 99

	first, err := readCachedSNATPortUsage(now)
	if err != nil {
		t.Fatalf("readCachedSNATPortUsage returned error: %v", err)
	}
	if first[1] != (snatPortUsage{portsInUse: 3}) {
		t.Fatalf("cached usage=%+v, want ports=3", first[1])
	}

	first[1] = snatPortUsage{portsInUse: 42}
	second, err := readCachedSNATPortUsage(now)
	if err != nil {
		t.Fatalf("second readCachedSNATPortUsage returned error: %v", err)
	}
	if second[1] != (snatPortUsage{portsInUse: 3}) {
		t.Fatalf("cached usage after caller mutation=%+v, want ports=3", second[1])
	}

	sessions, err := readCachedSessionUsage(now)
	if err != nil {
		t.Fatalf("readCachedSessionUsage returned error: %v", err)
	}
	if sessions[sessionKey] != 7 {
		t.Fatalf("cached sessions=%v, want snat=7", sessions)
	}
}

func TestReadCachedSessionUsageRejectsStaleSnapshot(t *testing.T) {
	original := cachedSessionUsage.Load()
	t.Cleanup(func() {
		cachedSessionUsage.Store(original)
	})

	now := time.Now()
	publishSessionUsage(map[uint32]snatPortUsage{
		1: {portsInUse: 3},
	}, map[sessionUsageKey]uint64{{sandboxIP: 2, packetClass: packetClassSNAT}: 7}, now.Add(-sessionUsageMaxAge-time.Nanosecond))

	if _, err := readCachedSNATPortUsage(now); err == nil {
		t.Fatal("readCachedSNATPortUsage returned nil error for a stale snapshot")
	}
}
