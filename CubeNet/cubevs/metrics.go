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
	maxPortEnd = 65535
)

var (
	readSandboxTrafficMetricsFn = readSandboxTrafficMetrics
	readSNATAllocationMetricsFn = readSNATAllocationMetrics
	readSessionMetricsFn        = readSessionMetrics
)

type mapEntryDeleter interface {
	Delete(key interface{}) error
}

type mapEntryUpdater interface {
	Update(key, value interface{}, flags ebpf.MapUpdateFlags) error
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
	PortsCapacity uint64
	PortsUnused   uint64
}

// SessionMetrics reports the current number of live egress sessions for one
// sandbox and packet class.
type SessionMetrics struct {
	SandboxIP     string
	PacketClass   string
	SessionsInUse uint64
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

// ReadSandboxTrafficMetrics reads per-sandbox datapath counters from pinned
// eBPF maps and returns a stable userspace snapshot.
func ReadSandboxTrafficMetrics() ([]SandboxTrafficMetrics, error) {
	return readSandboxTrafficMetricsFn()
}

// DeleteSandboxTrafficMetrics removes the traffic counters for one sandbox IP.
// Deleting a missing entry is idempotent.
func DeleteSandboxTrafficMetrics(ip net.IP) error {
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		return err
	}
	defer m.Close()

	key := ipToUint32(ip)
	return deleteSandboxTrafficMetricsEntry(m, key)
}

// EnsureSandboxTrafficMetrics creates or resets the counter entry for one
// sandbox IP.
func EnsureSandboxTrafficMetrics(ip net.IP) error {
	m, err := loadPinnedMap(MapNameSandboxMetrics)
	if err != nil {
		return err
	}
	defer m.Close()

	key := ipToUint32(ip)
	return ensureSandboxTrafficMetricsEntry(m, key)
}

func ensureSandboxTrafficMetricsEntry(m mapEntryUpdater, sandboxIP uint32) error {
	value := sandboxMetricsValue{}
	if err := m.Update(&sandboxIP, &value, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s, sandbox_ip: %s", err, MapNameSandboxMetrics, uint32ToIP(sandboxIP))
	}
	return nil
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

// ReadSessionMetrics returns live egress session counts grouped by sandbox IP
// and packet class.
func ReadSessionMetrics() ([]SessionMetrics, error) {
	return readSessionMetricsFn()
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
	var value sandboxMetricsValue
	iter := m.Iterate()
	for iter.Next(&sandboxIP, &value) {
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

func readSNATAllocationMetrics() ([]SNATAllocationMetrics, error) {
	snatPath := pinPath(mapNameSNATIPList)
	snatPortUsageByNodeIP, err := readCachedSNATPortUsage(time.Now())
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

	entries := buildSNATAllocationMetrics(values, snatPortUsageByNodeIP)
	return entries, nil
}

func readSessionMetrics() ([]SessionMetrics, error) {
	sessionsBySandboxIPAndPacketClass, err := readCachedSessionUsage(time.Now())
	if err != nil {
		log.Printf("cubevs metrics: session usage cache read failed: map=%s err=%v", MapNameEgressSessions, err)
		return nil, err
	}

	entries := make([]SessionMetrics, 0, len(sessionsBySandboxIPAndPacketClass))
	for key, sessions := range sessionsBySandboxIPAndPacketClass {
		entries = append(entries, SessionMetrics{
			SandboxIP:     uint32ToIP(key.sandboxIP).String(),
			PacketClass:   packetClassToString(key.packetClass),
			SessionsInUse: sessions,
		})
	}
	slices.SortFunc(entries, func(a, b SessionMetrics) int {
		switch {
		case a.SandboxIP < b.SandboxIP:
			return -1
		case a.SandboxIP > b.SandboxIP:
			return 1
		case a.PacketClass < b.PacketClass:
			return -1
		case a.PacketClass > b.PacketClass:
			return 1
		default:
			return 0
		}
	})
	return entries, nil
}

func buildSNATAllocationMetrics(values []snatIP, snatPortUsageByNodeIP map[uint32]snatPortUsage) []SNATAllocationMetrics {
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

		usage := snatPortUsageByNodeIP[value.IP]
		unused := uint64(0)
		if usage.portsInUse < capacity {
			unused = capacity - usage.portsInUse
		}

		entries = append(entries, SNATAllocationMetrics{
			SNATIP:        uint32ToIP(value.IP).String(),
			Ifindex:       value.Ifindex,
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
