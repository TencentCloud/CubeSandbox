package cubevs

import (
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestReadNetworkMetricsCombinesGranularReaders(t *testing.T) {
	originalSandboxRead := readSandboxTrafficMetricsFn
	originalSNATRead := readSNATAllocationMetricsFn
	t.Cleanup(func() {
		readSandboxTrafficMetricsFn = originalSandboxRead
		readSNATAllocationMetricsFn = originalSNATRead
	})

	wantSandboxes := []SandboxTrafficMetrics{{
		SandboxIP:      "192.168.0.10",
		IngressPackets: 11,
	}}
	wantSNATs := []SNATAllocationMetrics{{
		SNATIP:        "203.0.113.2",
		SessionsInUse: 17,
	}}

	readSandboxTrafficMetricsFn = func() ([]SandboxTrafficMetrics, error) {
		return wantSandboxes, nil
	}
	readSNATAllocationMetricsFn = func() ([]SNATAllocationMetrics, error) {
		return wantSNATs, nil
	}

	got, err := ReadNetworkMetrics()
	if err != nil {
		t.Fatalf("ReadNetworkMetrics returned error: %v", err)
	}
	if got == nil {
		t.Fatal("ReadNetworkMetrics returned nil snapshot")
	}
	if !reflect.DeepEqual(got.Sandboxes, wantSandboxes) {
		t.Fatalf("sandboxes mismatch: got=%#v want=%#v", got.Sandboxes, wantSandboxes)
	}
	if !reflect.DeepEqual(got.SNATs, wantSNATs) {
		t.Fatalf("snats mismatch: got=%#v want=%#v", got.SNATs, wantSNATs)
	}
}

func TestReadNetworkMetricsPropagatesSandboxReadError(t *testing.T) {
	originalSandboxRead := readSandboxTrafficMetricsFn
	originalSNATRead := readSNATAllocationMetricsFn
	t.Cleanup(func() {
		readSandboxTrafficMetricsFn = originalSandboxRead
		readSNATAllocationMetricsFn = originalSNATRead
	})

	wantErr := errors.New("sandbox read failed")
	readSandboxTrafficMetricsFn = func() ([]SandboxTrafficMetrics, error) {
		return nil, wantErr
	}
	readSNATAllocationMetricsFn = func() ([]SNATAllocationMetrics, error) {
		t.Fatal("snat reader should not run after sandbox read failure")
		return nil, nil
	}

	if _, err := ReadNetworkMetrics(); !errors.Is(err, wantErr) {
		t.Fatalf("ReadNetworkMetrics error=%v want=%v", err, wantErr)
	}
}

func TestReadSNATAllocationMetricsUsesConfiguredReader(t *testing.T) {
	original := readSNATAllocationMetricsFn
	t.Cleanup(func() {
		readSNATAllocationMetricsFn = original
	})

	want := []SNATAllocationMetrics{{
		SNATIP:             "203.0.113.9",
		PortsCapacity:      35536,
		PortsEstimatedFree: 35535,
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

func TestSumSandboxMetricsValues(t *testing.T) {
	values := []sandboxMetricsValue{
		{
			IngressPackets:     1,
			IngressBytes:       2,
			IngressDropPackets: 3,
			IngressDropBytes:   4,
			EgressPackets:      5,
			EgressBytes:        6,
			EgressDropPackets:  7,
			EgressDropBytes:    8,
			SNATAllocFailures:  9,
		},
		{
			IngressPackets:     10,
			IngressBytes:       20,
			IngressDropPackets: 30,
			IngressDropBytes:   40,
			EgressPackets:      50,
			EgressBytes:        60,
			EgressDropPackets:  70,
			EgressDropBytes:    80,
			SNATAllocFailures:  90,
		},
	}

	got := sumSandboxMetricsValues(values)
	want := sandboxMetricsValue{
		IngressPackets:     11,
		IngressBytes:       22,
		IngressDropPackets: 33,
		IngressDropBytes:   44,
		EgressPackets:      55,
		EgressBytes:        66,
		EgressDropPackets:  77,
		EgressDropBytes:    88,
		SNATAllocFailures:  99,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox metrics sum mismatch: got=%#v want=%#v", got, want)
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

	got := buildSNATAllocationMetrics(values, map[uint32]uint64{ip: 17})
	if len(got) != 1 {
		t.Fatalf("metrics entries=%d, want 1: %#v", len(got), got)
	}

	want := SNATAllocationMetrics{
		SNATIP:             "203.0.113.9",
		Ifindex:            7,
		SessionsInUse:      17,
		PortsCapacity:      35536,
		PortsEstimatedFree: 35519,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("metrics mismatch: got=%#v want=%#v", got[0], want)
	}
}

func TestReadCachedSNATSessionUsageRequiresSnapshot(t *testing.T) {
	original := cachedSNATSessionUsage.Load()
	cachedSNATSessionUsage.Store(nil)
	t.Cleanup(func() {
		cachedSNATSessionUsage.Store(original)
	})

	if _, err := readCachedSNATSessionUsage(time.Now()); err == nil {
		t.Fatal("readCachedSNATSessionUsage returned nil error without a snapshot")
	}
}

func TestSNATSessionUsageSnapshotIsImmutable(t *testing.T) {
	original := cachedSNATSessionUsage.Load()
	t.Cleanup(func() {
		cachedSNATSessionUsage.Store(original)
	})

	now := time.Now()
	source := map[uint32]uint64{1: 7}
	publishSNATSessionUsage(source, now)
	source[1] = 99

	first, err := readCachedSNATSessionUsage(now)
	if err != nil {
		t.Fatalf("readCachedSNATSessionUsage returned error: %v", err)
	}
	if first[1] != 7 {
		t.Fatalf("cached count=%d, want 7", first[1])
	}

	first[1] = 42
	second, err := readCachedSNATSessionUsage(now)
	if err != nil {
		t.Fatalf("second readCachedSNATSessionUsage returned error: %v", err)
	}
	if second[1] != 7 {
		t.Fatalf("cached count after caller mutation=%d, want 7", second[1])
	}
}

func TestReadCachedSNATSessionUsageRejectsStaleSnapshot(t *testing.T) {
	original := cachedSNATSessionUsage.Load()
	t.Cleanup(func() {
		cachedSNATSessionUsage.Store(original)
	})

	now := time.Now()
	publishSNATSessionUsage(map[uint32]uint64{1: 7}, now.Add(-snatUsageMaxAge-time.Nanosecond))

	if _, err := readCachedSNATSessionUsage(now); err == nil {
		t.Fatal("readCachedSNATSessionUsage returned nil error for a stale snapshot")
	}
}
