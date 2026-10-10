package cubevs

import (
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"time"

	"github.com/cilium/ebpf"
)

const (
	snatPacketClass = 0
	maxPortEnd      = 65535
)

var (
	readSandboxTrafficMetricsFn = readSandboxTrafficMetrics
	readSNATAllocationMetricsFn = readSNATAllocationMetrics
)

type mapEntryDeleter interface {
	Delete(key interface{}) error
}

// SandboxTrafficMetrics is the Prometheus-friendly view of one sandbox's
// cumulative datapath counters.
type SandboxTrafficMetrics struct {
	SandboxIP          string
	IngressPackets     uint64
	IngressBytes       uint64
	IngressDropPackets uint64
	IngressDropBytes   uint64
	EgressPackets      uint64
	EgressBytes        uint64
	EgressDropPackets  uint64
	EgressDropBytes    uint64
	SNATAllocFailures  uint64
}

// SNATAllocationMetrics reports one SNAT IP's current session and distinct
// source-port usage.
type SNATAllocationMetrics struct {
	SNATIP        string
	Ifindex       uint32
	SessionsInUse uint64
	PortsCapacity uint64
	PortsUnused   uint64
}

// NetworkMetricsSnapshot is the userspace snapshot exported to Prometheus.
type NetworkMetricsSnapshot struct {
	Sandboxes []SandboxTrafficMetrics
	SNATs     []SNATAllocationMetrics
}

type sandboxMetricsValue struct {
	IngressPackets     uint64
	IngressBytes       uint64
	IngressDropPackets uint64
	IngressDropBytes   uint64
	EgressPackets      uint64
	EgressBytes        uint64
	EgressDropPackets  uint64
	EgressDropBytes    uint64
	SNATAllocFailures  uint64
}

// ReadNetworkMetrics reads current sandbox counters and the latest cached SNAT
// allocation usage.
func ReadNetworkMetrics() (*NetworkMetricsSnapshot, error) {
	sandboxes, err := ReadSandboxTrafficMetrics()
	if err != nil {
		return nil, err
	}

	snats, err := ReadSNATAllocationMetrics()
	if err != nil {
		return nil, err
	}

	return &NetworkMetricsSnapshot{
		Sandboxes: sandboxes,
		SNATs:     snats,
	}, nil
}

// ReadSandboxTrafficMetrics reads per-sandbox datapath counters from pinned
// eBPF maps and returns a stable userspace snapshot.
func ReadSandboxTrafficMetrics() ([]SandboxTrafficMetrics, error) {
	return readSandboxTrafficMetricsFn()
}

// DeleteSandboxTrafficMetrics removes all per-CPU traffic counters for one
// sandbox IP. Deleting a missing entry is idempotent.
func DeleteSandboxTrafficMetrics(ip net.IP) error {
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		return err
	}
	defer m.Close()

	key := ipToUint32(ip)
	return deleteSandboxTrafficMetricsEntry(m, key)
}

func deleteSandboxTrafficMetricsEntry(m mapEntryDeleter, sandboxIP uint32) error {
	if err := m.Delete(&sandboxIP); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Delete failed: %w, name: %s, sandbox_ip: %s", err, MapNameSandboxMetrics, uint32ToIP(sandboxIP))
	}
	return nil
}

// ReadSNATAllocationMetrics reads SNAT allocator state from pinned eBPF maps
// and combines it with the latest session-reaper usage snapshot.
func ReadSNATAllocationMetrics() ([]SNATAllocationMetrics, error) {
	return readSNATAllocationMetricsFn()
}

func readSandboxTrafficMetrics() ([]SandboxTrafficMetrics, error) {
	path := pinPath(MapNameSandboxMetrics)
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		log.Printf("cubevs metrics: sandbox traffic load failed: map=%s path=%s err=%v", MapNameSandboxMetrics, path, err)
		return nil, err
	}
	defer m.Close()

	entries := make([]SandboxTrafficMetrics, 0)
	var sandboxIP uint32
	var perCPUValues []sandboxMetricsValue
	iter := m.Iterate()
	for iter.Next(&sandboxIP, &perCPUValues) {
		value := sumSandboxMetricsValues(perCPUValues)
		entries = append(entries, SandboxTrafficMetrics{
			SandboxIP:          uint32ToIP(sandboxIP).String(),
			IngressPackets:     value.IngressPackets,
			IngressBytes:       value.IngressBytes,
			IngressDropPackets: value.IngressDropPackets,
			IngressDropBytes:   value.IngressDropBytes,
			EgressPackets:      value.EgressPackets,
			EgressBytes:        value.EgressBytes,
			EgressDropPackets:  value.EgressDropPackets,
			EgressDropBytes:    value.EgressDropBytes,
			SNATAllocFailures:  value.SNATAllocFailures,
		})
	}
	if err := wrapIterErr(iter.Err(), MapNameSandboxMetrics); err != nil {
		log.Printf("cubevs metrics: sandbox traffic iterate failed: map=%s path=%s err=%v", MapNameSandboxMetrics, path, err)
		return nil, err
	}

	slices.SortFunc(entries, func(a, b SandboxTrafficMetrics) int {
		switch {
		case a.SandboxIP < b.SandboxIP:
			return -1
		case a.SandboxIP > b.SandboxIP:
			return 1
		default:
			return 0
		}
	})

	return entries, nil
}

func sumSandboxMetricsValues(values []sandboxMetricsValue) sandboxMetricsValue {
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

func readSNATAllocationMetrics() ([]SNATAllocationMetrics, error) {
	snatPath := pinPath(mapNameSNATIPList)
	usageByNodeIP, err := readCachedSNATSessionUsage(time.Now())
	if err != nil {
		log.Printf("cubevs metrics: SNAT allocation prerequisite failed: session_cache=%s err=%v", MapNameEgressSessions, err)
		return nil, err
	}

	m, err := loadPinnedMap(mapNameSNATIPList)
	if err != nil {
		log.Printf("cubevs metrics: SNAT allocation load failed: map=%s path=%s err=%v", mapNameSNATIPList, snatPath, err)
		return nil, err
	}
	defer m.Close()

	values := make([]snatIP, 0, maxSNATIPs)
	for i := uint32(0); i < maxSNATIPs; i++ {
		var value snatIP
		if err := m.Lookup(&i, &value); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			log.Printf("cubevs metrics: SNAT allocation lookup failed: map=%s path=%s index=%d err=%v", mapNameSNATIPList, snatPath, i, err)
			return nil, fmt.Errorf("map.Lookup failed: %w, name: %s, index: %d", err, mapNameSNATIPList, i)
		}
		if value.IP == 0 {
			continue
		}
		values = append(values, value)
	}

	entries := buildSNATAllocationMetrics(values, usageByNodeIP)
	return entries, nil
}

func buildSNATAllocationMetrics(values []snatIP, usageByNodeIP map[uint32]snatSessionUsage) []SNATAllocationMetrics {
	capacity := uint64(maxPortEnd - maxPortStart + 1)
	entries := make([]SNATAllocationMetrics, 0, len(values))
	seenIPs := make(map[uint32]struct{}, len(values))
	for _, value := range values {
		if value.IP == 0 {
			continue
		}
		// setSNATIPs replicates a short IP list across allocator shards.
		// Prometheus series are per IP, so emit each logical IP only once.
		if _, ok := seenIPs[value.IP]; ok {
			continue
		}
		seenIPs[value.IP] = struct{}{}

		usage := usageByNodeIP[value.IP]
		unused := uint64(0)
		if usage.portsInUse < capacity {
			unused = capacity - usage.portsInUse
		}

		entries = append(entries, SNATAllocationMetrics{
			SNATIP:        uint32ToIP(value.IP).String(),
			Ifindex:       value.Ifindex,
			SessionsInUse: usage.sessionsInUse,
			PortsCapacity: capacity,
			PortsUnused:   unused,
		})
	}

	slices.SortFunc(entries, func(a, b SNATAllocationMetrics) int {
		switch {
		case a.SNATIP < b.SNATIP:
			return -1
		case a.SNATIP > b.SNATIP:
			return 1
		default:
			return 0
		}
	})

	return entries
}
