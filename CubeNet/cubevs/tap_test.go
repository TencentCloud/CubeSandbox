package cubevs

import (
	"errors"
	"net"
	"testing"

	"github.com/cilium/ebpf"
)

type fakeTapMetadataMap struct {
	entries   map[uint32]uint32
	lookupErr error
	deleteErr error
}

func (m *fakeTapMetadataMap) Lookup(key, valueOut interface{}) error {
	if m.lookupErr != nil {
		return m.lookupErr
	}
	value, ok := m.entries[*key.(*uint32)]
	if !ok {
		return ebpf.ErrKeyNotExist
	}
	*valueOut.(*uint32) = value
	return nil
}

func (m *fakeTapMetadataMap) Delete(key interface{}) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	typedKey := *key.(*uint32)
	if _, ok := m.entries[typedKey]; !ok {
		return ebpf.ErrKeyNotExist
	}
	delete(m.entries, typedKey)
	return nil
}

func TestDeleteTAPDeviceMetadataEntriesDeletesMatchingReverseEntry(t *testing.T) {
	const (
		ifindex = uint32(12)
		mvmIP   = uint32(0x0a000002)
	)
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{ifindex: 0}}
	ipMap := &fakeTapMetadataMap{entries: map[uint32]uint32{mvmIP: ifindex}}

	canDeleteIPState, err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP)
	if err != nil {
		t.Fatal(err)
	}
	if !canDeleteIPState {
		t.Fatal("matching owner should allow deleting IP-scoped state")
	}
	if len(ifindexMap.entries) != 0 || len(ipMap.entries) != 0 {
		t.Fatalf("matching metadata was not fully deleted: ifindex=%#v ip=%#v", ifindexMap.entries, ipMap.entries)
	}
}

func TestDeleteTAPDeviceMetadataEntriesPreservesReusedIP(t *testing.T) {
	const (
		oldIfindex = uint32(12)
		newIfindex = uint32(13)
		mvmIP      = uint32(0x0a000002)
	)
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{oldIfindex: 0}}
	ipMap := &fakeTapMetadataMap{entries: map[uint32]uint32{mvmIP: newIfindex}}

	canDeleteIPState, err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, oldIfindex, mvmIP)
	if err != nil {
		t.Fatal(err)
	}
	if canDeleteIPState {
		t.Fatal("stale owner should not delete IP-scoped state")
	}
	if _, ok := ifindexMap.entries[oldIfindex]; ok {
		t.Fatal("old ifindex metadata was not deleted")
	}
	if got := ipMap.entries[mvmIP]; got != newIfindex {
		t.Fatalf("reused IP mapping changed to ifindex %d, want %d", got, newIfindex)
	}
}

func TestDeleteTAPDeviceMetadataEntriesTreatsMissingReverseEntryAsClean(t *testing.T) {
	const (
		ifindex = uint32(12)
		mvmIP   = uint32(0x0a000002)
	)
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{ifindex: 0}}
	ipMap := &fakeTapMetadataMap{entries: make(map[uint32]uint32)}

	canDeleteIPState, err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP)
	if err != nil {
		t.Fatal(err)
	}
	if !canDeleteIPState {
		t.Fatal("missing reverse entry should allow deleting orphaned IP-scoped state")
	}
	if _, ok := ifindexMap.entries[ifindex]; ok {
		t.Fatal("ifindex metadata was not deleted")
	}
}

func TestDeleteTAPDeviceMetadataEntriesLookupErrorDoesNotPartiallyDelete(t *testing.T) {
	const (
		ifindex = uint32(12)
		mvmIP   = uint32(0x0a000002)
	)
	lookupErr := errors.New("lookup failed")
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{ifindex: 0}}
	ipMap := &fakeTapMetadataMap{
		entries:   make(map[uint32]uint32),
		lookupErr: lookupErr,
	}

	_, err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want %v", err, lookupErr)
	}
	if _, ok := ifindexMap.entries[ifindex]; !ok {
		t.Fatal("ifindex metadata was deleted after reverse lookup failed")
	}
}

func TestTAPMetadataLifecycleContinuesWithoutMetricsMap(t *testing.T) {
	const ifindex = uint32(41)
	// pinPolicyMaps mounts the scratch bpffs. Metadata is pinned on that
	// same mount afterwards; a second mountBpffs would hide it.
	pinPolicyMaps(t, ifindex)
	pinTAPMetadataMapsWithoutMetrics(t)
	ip := net.ParseIP("10.0.0.41")
	if err := AddTAPDevice(ifindex, ip, "sandbox-41", 1, MVMOptions{}); err != nil {
		t.Fatalf("AddTAPDevice failed without metrics map: %v", err)
	}

	device, err := GetTAPDevice(ifindex)
	if err != nil {
		t.Fatalf("GetTAPDevice: %v", err)
	}
	if device.ID != "sandbox-41" || !device.IP.Equal(ip) {
		t.Fatalf("registered TAP=%+v, want sandbox-41 at %s", device, ip)
	}

	if err := DeleteTAPDeviceMetadata(ifindex, ip); err != nil {
		t.Fatalf("DeleteTAPDeviceMetadata failed without metrics map: %v", err)
	}
	if _, err := GetTAPDevice(ifindex); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("GetTAPDevice after delete error=%v, want ErrKeyNotExist", err)
	}
}

func TestDeleteTAPDevicePolicyMapsDropsDNSTrackRateLimit(t *testing.T) {
	const ifindex = uint32(77)
	pinPolicyMaps(t, ifindex)
	pinTAPMetadataMapsWithoutMetrics(t)
	if err := setDNSTrackRateLimit(ifindex); err != nil {
		t.Fatalf("install rate limit: %v", err)
	}

	if err := DeleteTAPDevicePolicyMaps(ifindex); err != nil {
		t.Fatalf("DeleteTAPDevicePolicyMaps: %v", err)
	}
	m, err := loadPinnedMap(MapNameDNSTrackRL)
	if err != nil {
		t.Fatalf("load %s: %v", MapNameDNSTrackRL, err)
	}
	defer m.Close()
	var state dnsTrackRLState
	key := ifindex
	err = m.Lookup(&key, &state)
	if !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("rate-limit entry after destroy: %v, want ErrKeyNotExist", err)
	}

	if err := DeleteTAPDevicePolicyMaps(ifindex); err != nil {
		t.Fatalf("second DeleteTAPDevicePolicyMaps: %v", err)
	}
}

func TestDeleteTAPDeviceMetadataMetricsLifecycle(t *testing.T) {
	t.Run("removes metrics for matching owner", func(t *testing.T) {
		mountBpffs(t)
		pinTAPMetadataMaps(t)

		const ifindex = uint32(30)
		ip := net.ParseIP("10.0.0.30")
		if err := UpsertTAPDeviceMetadata(ifindex, ip, "sandbox-30", 1); err != nil {
			t.Fatalf("register owner: %v", err)
		}
		mustWriteSandboxMetrics(t, ip, sandboxMetricsValue{IngressPackets: 11})

		if err := DeleteTAPDeviceMetadata(ifindex, ip); err != nil {
			t.Fatalf("delete owner: %v", err)
		}
		assertSandboxMetricsMissing(t, ip)
	})

	t.Run("preserves metrics for reused IP", func(t *testing.T) {
		mountBpffs(t)
		pinTAPMetadataMaps(t)

		const (
			oldIfindex = uint32(31)
			newIfindex = uint32(32)
		)
		ip := net.ParseIP("10.0.0.31")
		if err := UpsertTAPDeviceMetadata(oldIfindex, ip, "sandbox-old", 1); err != nil {
			t.Fatalf("register old owner: %v", err)
		}
		if err := UpsertTAPDeviceMetadata(newIfindex, ip, "sandbox-new", 1); err != nil {
			t.Fatalf("register new owner: %v", err)
		}
		wantMetrics := sandboxMetricsValue{IngressPackets: 17}
		mustWriteSandboxMetrics(t, ip, wantMetrics)

		if err := DeleteTAPDeviceMetadata(oldIfindex, ip); err != nil {
			t.Fatalf("delete stale owner: %v", err)
		}
		if got := mustLookupIfindexByIP(t, ip); got != newIfindex {
			t.Fatalf("IP owner=%d, want %d", got, newIfindex)
		}
		if got := mustReadSandboxMetrics(t, ip); got != wantMetrics {
			t.Fatalf("new owner metrics=%+v, want %+v", got, wantMetrics)
		}
	})

	t.Run("removes orphaned metrics without reverse mapping", func(t *testing.T) {
		mountBpffs(t)
		pinTAPMetadataMaps(t)

		const ifindex = uint32(33)
		ip := net.ParseIP("10.0.0.33")
		if err := UpsertTAPDeviceMetadata(ifindex, ip, "sandbox-33", 1); err != nil {
			t.Fatalf("register owner: %v", err)
		}
		mustDeleteTAPIPMapping(t, ip)
		mustWriteSandboxMetrics(t, ip, sandboxMetricsValue{IngressPackets: 19})

		if err := DeleteTAPDeviceMetadata(ifindex, ip); err != nil {
			t.Fatalf("delete owner without reverse mapping: %v", err)
		}
		assertSandboxMetricsMissing(t, ip)
	})
}

func TestUpsertTAPDeviceMetadataReleasesPreviousIPMetrics(t *testing.T) {
	mountBpffs(t)
	pinTAPMetadataMaps(t)

	const ifindex = uint32(21)
	oldIP := net.ParseIP("10.0.0.21")
	newIP := net.ParseIP("10.0.0.22")
	if err := UpsertTAPDeviceMetadata(ifindex, oldIP, "sandbox-21", 1); err != nil {
		t.Fatalf("register old IP: %v", err)
	}
	if err := UpsertTAPDeviceMetadata(ifindex, newIP, "sandbox-21", 2); err != nil {
		t.Fatalf("register new IP: %v", err)
	}

	assertSandboxMetricsMissing(t, oldIP)
	if got := mustLookupIfindexByIP(t, newIP); got != ifindex {
		t.Fatalf("new IP owner=%d, want %d", got, ifindex)
	}
	_ = mustReadSandboxMetrics(t, newIP)
}

func mustWriteSandboxMetrics(t *testing.T, ip net.IP, value sandboxMetricsValue) {
	t.Helper()
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		t.Fatalf("load %s: %v", MapNameSandboxMetrics, err)
	}
	defer m.Close()
	key := ipToUint32(ip)
	if err := m.Update(&key, &value, ebpf.UpdateAny); err != nil {
		t.Fatalf("write metrics for %s: %v", ip, err)
	}
}

func mustReadSandboxMetrics(t *testing.T, ip net.IP) sandboxMetricsValue {
	t.Helper()
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		t.Fatalf("load %s: %v", MapNameSandboxMetrics, err)
	}
	defer m.Close()
	key := ipToUint32(ip)
	var value sandboxMetricsValue
	if err := m.Lookup(&key, &value); err != nil {
		t.Fatalf("read metrics for %s: %v", ip, err)
	}
	return value
}

func mustDeleteTAPIPMapping(t *testing.T, ip net.IP) {
	t.Helper()
	m, err := loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		t.Fatalf("load %s: %v", MapNameMVMIPToIfindex, err)
	}
	defer m.Close()
	key := ipToUint32(ip)
	if err := m.Delete(&key); err != nil {
		t.Fatalf("delete reverse mapping for %s: %v", ip, err)
	}
}

func assertSandboxMetricsMissing(t *testing.T, ip net.IP) {
	t.Helper()
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		t.Fatalf("load %s: %v", MapNameSandboxMetrics, err)
	}
	defer m.Close()
	key := ipToUint32(ip)
	var value sandboxMetricsValue
	if err := m.Lookup(&key, &value); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("metrics lookup for %s error=%v, want ErrKeyNotExist", ip, err)
	}
}

func mustLookupIfindexByIP(t *testing.T, ip net.IP) uint32 {
	t.Helper()
	ifindex, err := LookupIfindexByIP(ip)
	if err != nil {
		t.Fatalf("lookup ifindex for %s: %v", ip, err)
	}
	return ifindex
}

// fakeMvmMetadataMap is an in-memory mvmVersionMapOps for bumpMvmVersion tests.
type fakeMvmMetadataMap struct {
	entries map[uint32]mvmMetadata
}

func (m *fakeMvmMetadataMap) Lookup(key, valueOut interface{}) error {
	v, ok := m.entries[*key.(*uint32)]
	if !ok {
		return ebpf.ErrKeyNotExist
	}
	*valueOut.(*mvmMetadata) = v
	return nil
}

func (m *fakeMvmMetadataMap) Update(key, value any, _ ebpf.MapUpdateFlags) error {
	m.entries[*key.(*uint32)] = *value.(*mvmMetadata)
	return nil
}

func TestBumpMvmVersionIncrementsAndPreservesFields(t *testing.T) {
	const ifindex = uint32(42)
	orig := mvmMetadata{
		Version:        5,
		IP:             0x0a000002,
		DNSPolicyFlags: 3,
	}
	orig.UUID[0] = 'a'
	orig.Reserved[0] = 0x7f
	m := &fakeMvmMetadataMap{entries: map[uint32]mvmMetadata{ifindex: orig}}

	if err := bumpMvmVersion(m, ifindex); err != nil {
		t.Fatal(err)
	}
	got := m.entries[ifindex]
	if got.Version != 6 {
		t.Fatalf("version = %d, want 6 (map RMW +1, immune to a reset counter)", got.Version)
	}
	if got.IP != orig.IP || got.DNSPolicyFlags != orig.DNSPolicyFlags ||
		got.UUID != orig.UUID || got.Reserved != orig.Reserved {
		t.Fatal("bump must preserve IP/UUID/DNSPolicyFlags/Reserved")
	}
}

func TestBumpMvmVersionMissingEntry(t *testing.T) {
	m := &fakeMvmMetadataMap{entries: map[uint32]mvmMetadata{}}
	if err := bumpMvmVersion(m, 99); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("err = %v, want ErrKeyNotExist", err)
	}
}
