package cubevs

// direct_neigh_scanner.go — userspace driver for direct-mode on-link neighbor
// resolution.
//
// The datapath (mvmtap prepare_egress_l2) only reports state into direct_neigh
// (fib result + last-activity) and always forwards (gateway MAC fallback when a
// destination is unresolved). This scanner owns all scheduling: it walks the
// map on a fixed interval and, per entry, garbage-collects idle destinations,
// fires learning triggers with exponential backoff for destinations whose last
// fib lookup failed, and fires keepalive triggers for learned destinations so
// the kernel neighbor entry is periodically re-validated. The MAC itself is
// never stored or managed here — it always comes from bpf_fib_lookup.
//
// Field ownership in the shared map entry (see cubevs.h struct direct_neighbor):
// the datapath writes addr/fib_ok/valid_until_ns/last_used_ns; this scanner
// writes step/next_attempt_ns/next_refresh_ns. A lost read-modify-write race on
// either side is bounded and self-healing: it can delay a trigger or an
// activity timestamp by a scan cycle, and — because this scanner writes back
// the whole value — it can transiently resurrect a MAC the datapath had just
// invalidated (for at most the leftover valid_until, ≤ CACHE_TTL), before the
// next datapath fib refresh corrects it. That stays within the documented
// "cache staleness ≤ CACHE_TTL" failure model, so it is accepted, but it is not
// strictly "never a wrong MAC".

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cilium/ebpf"
)

// directNeighbor mirrors struct direct_neighbor in cubevs.h.
type directNeighbor struct {
	Addr          [6]uint8
	FibOk         uint8
	Step          uint8
	Flags         uint16
	_             [2]uint8
	Reserved      uint32
	ValidUntilNs  uint64
	LastUsedNs    uint64
	NextAttemptNs uint64
	NextRefreshNs uint64
}

// Scanner tuning. All durations are in nanoseconds to match the datapath's
// CLOCK_MONOTONIC (bpf_ktime_get_ns) timestamps.
const (
	directNeighScanInterval  = 300 * time.Millisecond
	directNeighGCAfterNs     = uint64(5 * 60 * 1e9) // 5 min idle -> GC
	directNeighRefreshNs     = uint64(16 * 1e9)     // keepalive period (drives NUD re-validation)
	directNeighBackoffBaseNs = uint64(1 * 1e9)      // learning backoff starts at 1s
	directNeighBackoffMaxNs  = uint64(16 * 1e9)     // and caps at 16s
	directNeighTriggerBurst  = 100                  // token bucket capacity
	directNeighTriggerPerSec = 100.0                // token bucket refill rate
	// directNeighTriggerPort is an arbitrary high UDP port; the learning event
	// is the kernel's ARP resolution for the target, independent of L4.
	directNeighTriggerPort = 65534
)

// directNeighStore abstracts the direct_neigh map so the scan logic can be
// unit-tested against an in-memory fake.
type directNeighStore interface {
	forEach(fn func(key uint32, val directNeighbor) (cont bool))
	update(key uint32, val directNeighbor) error
	delete(key uint32) error
}

type directNeighScanner struct {
	store   directNeighStore
	trigger func(targetIP uint32) error
	now     func() uint64 // monotonic ns

	mu     sync.Mutex
	tokens float64
	lastNS uint64
}

func newDirectNeighScanner(store directNeighStore, trigger func(uint32) error, now func() uint64) *directNeighScanner {
	return &directNeighScanner{
		store:   store,
		trigger: trigger,
		now:     now,
		tokens:  directNeighTriggerBurst,
	}
}

// allowTrigger implements the global token bucket that bounds trigger sends.
func (s *directNeighScanner) allowTrigger(nowNs uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastNS != 0 {
		elapsed := float64(nowNs-s.lastNS) / 1e9
		s.tokens += elapsed * directNeighTriggerPerSec
		if s.tokens > directNeighTriggerBurst {
			s.tokens = directNeighTriggerBurst
		}
	}
	s.lastNS = nowNs
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

func (s *directNeighScanner) fireTrigger(key uint32, nowNs uint64) {
	// Best effort: a failed send just means the kernel did not ARP this round;
	// the next scan retries. No logging facility exists in this package.
	_ = s.trigger(key)
}

// scanOnce runs one pass over the map.
func (s *directNeighScanner) scanOnce() {
	nowNs := s.now()
	s.store.forEach(func(key uint32, val directNeighbor) bool {
		// GC: idle destination, stop all triggering.
		if nowNs > val.LastUsedNs+directNeighGCAfterNs {
			s.store.delete(key)
			return true
		}

		if val.FibOk == 0 {
			// Not yet learned: exponential backoff learning trigger. The backoff
			// for the CURRENT level (1s, 2s, 4s, ... capped at 16s) is scheduled,
			// then the level advances.
			if nowNs >= val.NextAttemptNs && s.allowTrigger(nowNs) {
				s.fireTrigger(key, nowNs)
				backoff := directNeighBackoffBaseNs << val.Step
				if backoff > directNeighBackoffMaxNs || backoff == 0 {
					backoff = directNeighBackoffMaxNs
				}
				val.NextAttemptNs = nowNs + backoff
				val.Step++
				s.store.update(key, val)
			}
			return true
		}

		// Learned: keepalive trigger on a fixed period.
		if val.Step != 0 {
			val.Step = 0
			s.store.update(key, val)
		}
		if nowNs >= val.NextRefreshNs && s.allowTrigger(nowNs) {
			s.fireTrigger(key, nowNs)
			val.NextRefreshNs = nowNs + directNeighRefreshNs
			s.store.update(key, val)
		}
		return true
	})
}

// run drives scanOnce on a fixed interval until stop is closed.
func (s *directNeighScanner) run(stop <-chan struct{}) {
	ticker := time.NewTicker(directNeighScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.scanOnce()
		}
	}
}

// ebpfDirectNeighStore adapts the pinned direct_neigh map to directNeighStore.
type ebpfDirectNeighStore struct {
	m *ebpf.Map
}

func (s *ebpfDirectNeighStore) forEach(fn func(uint32, directNeighbor) bool) {
	var key uint32
	var val directNeighbor
	iter := s.m.Iterate()
	for iter.Next(&key, &val) {
		if !fn(key, val) {
			return
		}
	}
}

func (s *ebpfDirectNeighStore) update(key uint32, val directNeighbor) error {
	return s.m.Update(&key, &val, ebpf.UpdateAny)
}

func (s *ebpfDirectNeighStore) delete(key uint32) error {
	return s.m.Delete(&key)
}

// sendUDPTrigger returns a trigger sender that shares one long-lived UDP socket
// bound to the node NIC's source IP, using WriteToUDP per target. This avoids a
// DialUDP+Close cycle per trigger at the token-bucket ceiling. The learning
// event is the kernel's ARP resolution for the target (and its reply); the L4
// payload is irrelevant and no reply is awaited.
func sendUDPTrigger(nodeIP net.IP) (func(uint32) error, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: nodeIP, Port: 0})
	if err != nil {
		return nil, err
	}
	return func(targetIP uint32) error {
		dst := &net.UDPAddr{IP: uint32ToIP(targetIP), Port: directNeighTriggerPort}
		_, err := conn.WriteToUDP([]byte{0}, dst)
		return err
	}, nil
}

// StartDirectNeighScanner starts the periodic scanner for direct-mode on-link
// neighbor resolution. nodeIP is the node NIC IPv4 used as the trigger source.
// Like StartSessionReaper it owns its own lifecycle and runs for the process
// lifetime. It is a best-effort driver of kernel neighbor learning; if it is
// absent the datapath degrades to fib-only resolution plus the gateway
// fallback, never to a frozen stale MAC.
func StartDirectNeighScanner(nodeIP net.IP) error {
	m, err := loadPinnedMap(MapNameDirectNeigh)
	if err != nil {
		return fmt.Errorf("loadPinnedMap %s: %w", MapNameDirectNeigh, err)
	}
	trigger, err := sendUDPTrigger(nodeIP)
	if err != nil {
		return fmt.Errorf("sendUDPTrigger: %w", err)
	}
	scanner := newDirectNeighScanner(&ebpfDirectNeighStore{m: m}, trigger, mustCurrentNS)
	go scanner.run(make(chan struct{})) // never closed: runs for the process lifetime
	return nil
}

// mustCurrentNS returns the current CLOCK_MONOTONIC time in ns; on error it
// falls back to time.Now-based monotonic time. Scan scheduling tolerates the
// fallback because all comparisons are relative.
func mustCurrentNS() uint64 {
	ns, err := currentNS()
	if err != nil {
		return uint64(time.Now().UnixNano())
	}
	return ns
}
