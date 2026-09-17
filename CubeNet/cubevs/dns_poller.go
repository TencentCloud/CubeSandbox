// DNS event pump: reads the frames the datapath uploads on the dns_events
// perfbuf, parses each response, updates the owning sandbox's learned rules via
// NetPolicyManager, and only then re-injects the frame so the sandbox observes
// the reply.
//
// The ordering is the point. dns_response_finish_prog drops a learnable reply
// from the datapath (TC_ACT_SHOT) after reverse-NATing it, so the sandbox
// cannot see a resolved address before the corresponding allow_out_v3 rows
// exist. Each worker therefore does LearnDNS first and injects second.
//
// One process-wide poller shares the perfbuf across every sandbox — dns_events
// is a global perf event array, not per-ifindex. Each record is a fixed 8-byte
// prefix (frame_len + ifindex) followed by the raw Ethernet frame. The prefix
// carries the ifindex in band because the post-NAT destination is mvm_inner_ip,
// a node-wide constant that cannot identify the sandbox.

package cubevs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cilium/ebpf/perf"
)

const (
	// perfbuf size per CPU for the DNS event ring. Large enough to ride out a
	// burst of replies; the query-side rate limit bounds the sustained rate.
	dnsPerfBufferSize = 4 << 20

	// dnsWorkerPoolSize matches the production pool. It must comfortably
	// exceed the number of sandboxes resolving concurrently: a worker holds a
	// per-ifindex lock across the learn, so too few workers would let one
	// sandbox's slow apply head-of-line block an unrelated sandbox's reply.
	dnsWorkerPoolSize = 100

	// dnsWorkerQueueDepth is the bounded hand-off between the reader and the
	// workers. A full queue blocks the reader, pushing loss into the perf ring
	// where it is counted, rather than silently dropping a frame we already
	// removed from the datapath.
	dnsWorkerQueueDepth = 256

	dnsHdrLen = 12
	ethHdrLen = 14
	// ipv4MinHdrLen is the header length with no options; the real length comes
	// from IHL.
	ipv4MinHdrLen = 20
	udpHdrLen     = 8
)

// dnsInjector abstracts the re-injection sink so tests can substitute a
// recording fake.
type dnsInjector interface {
	InjectDNSResponse(ifindex uint32, frame []byte) error
}

// DNSPoller reads DNS event records and delegates parsing, per-sandbox
// learning and re-injection to a fixed-size worker pool.
type DNSPoller struct {
	reader   *perf.Reader
	injector dnsInjector

	queue    chan []byte
	workers  sync.WaitGroup
	stopOnce sync.Once

	eventsSeen   atomic.Uint64
	eventsLost   atomic.Uint64
	parseErrors  atomic.Uint64
	learnErrors  atomic.Uint64
	injectErrors atomic.Uint64
}

// StartDNSPoller opens the dns_events perfbuf, opens the injector, sweeps
// orphaned policy snapshots, and launches the reader. Call after Init. Keep the
// returned *DNSPoller to read Stats() and to Stop() on shutdown.
func StartDNSPoller() (*DNSPoller, error) {
	sender, err := NewRawSender()
	if err != nil {
		return nil, fmt.Errorf("NewRawSender: %w", err)
	}
	poller, err := startDNSPoller(sender)
	if err != nil {
		_ = sender.Close()
		return nil, err
	}
	// Best-effort: a leftover snapshot only wastes tmpfs, and failing startup
	// over it would be worse than leaving it.
	if err := sweepOrphanPolicySnapshots(); err != nil {
		enqueueEvent(Event{Error: err, Message: "failed to sweep orphaned policy snapshots"})
	}
	return poller, nil
}

// startDNSPoller is the testable core: production supplies a RawSender, tests
// supply a recording injector.
func startDNSPoller(injector dnsInjector) (*DNSPoller, error) {
	m, err := loadPinnedMap(MapNameDNSEvents)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	reader, err := perf.NewReader(m, dnsPerfBufferSize)
	if err != nil {
		return nil, fmt.Errorf("perf.NewReader %s: %w", MapNameDNSEvents, err)
	}

	p := &DNSPoller{
		reader:   reader,
		injector: injector,
		queue:    make(chan []byte, dnsWorkerQueueDepth),
	}
	for i := 0; i < dnsWorkerPoolSize; i++ {
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			for sample := range p.queue {
				p.handleRecord(sample)
			}
		}()
	}
	go p.run()
	return p, nil
}

// Stop closes the perfbuf and waits for the workers to drain. Safe to call
// more than once.
func (p *DNSPoller) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		_ = p.reader.Close()
		// run() closes the queue once the reader returns ErrClosed, so the
		// workers see the channel close and exit.
		p.workers.Wait()
	})
}

// DNSPollerStats is a snapshot of the poller counters.
type DNSPollerStats struct {
	EventsSeen   uint64
	EventsLost   uint64
	ParseErrors  uint64
	LearnErrors  uint64
	InjectErrors uint64
}

// Stats returns a snapshot of the poller counters.
func (p *DNSPoller) Stats() DNSPollerStats {
	return DNSPollerStats{
		EventsSeen:   p.eventsSeen.Load(),
		EventsLost:   p.eventsLost.Load(),
		ParseErrors:  p.parseErrors.Load(),
		LearnErrors:  p.learnErrors.Load(),
		InjectErrors: p.injectErrors.Load(),
	}
}

// run is the reader loop. It exits when the perfbuf is closed, then closes the
// work queue so the pool drains.
func (p *DNSPoller) run() {
	defer close(p.queue)
	for {
		record, err := p.reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			continue
		}
		if record.LostSamples > 0 {
			p.eventsLost.Add(record.LostSamples)
		}
		if len(record.RawSample) < dnsEventPrefixLen {
			p.parseErrors.Add(1)
			continue
		}
		p.eventsSeen.Add(1)
		// Blocking send: backpressure belongs in the perf ring (where loss is
		// visible in LostSamples) rather than here, where a drop would strand
		// a reply the datapath already removed.
		p.queue <- record.RawSample
	}
}

// handleRecord splits one perfbuf record into its prefix and its frame.
//
// The frame must be bounded by the prefix's frame_len, not by the rest of the
// record: perf rounds a raw sample up to 8-byte alignment, so RawSample can
// carry a few bytes of padding past the end of the packet. Injecting those
// trailing bytes would mean the sandbox does not receive the byte-for-byte
// frame the datapath captured. This is what frame_len is for.
func (p *DNSPoller) handleRecord(rawSample []byte) {
	prefix := rawSample[:dnsEventPrefixLen]
	frame := rawSample[dnsEventPrefixLen:]
	if frameLen := int(binary.LittleEndian.Uint16(prefix[0:2])); frameLen <= len(frame) {
		frame = frame[:frameLen]
	}
	p.handleFrame(prefix, frame)
}

// handleFrame processes one record: prefix is the 8-byte dns_event_prefix,
// frame the raw Ethernet frame. Split out from run() so tests can drive it with
// a synthetic record.
func (p *DNSPoller) handleFrame(prefix, frame []byte) {
	ifindex := binary.LittleEndian.Uint32(prefix[4:8])

	payload, err := dnsPayload(frame)
	if err != nil {
		// Structurally invalid frame: there is nothing meaningful to inject.
		p.parseErrors.Add(1)
		return
	}

	resp, _ := parseDNSResponse(payload)
	if resp == nil {
		// Bad DNS content, but the frame itself is fine to deliver — the
		// sandbox's resolver will see it and retry.
		p.parseErrors.Add(1)
	}

	if ifindex != 0 && resp != nil {
		mgr, mgrErr := GetNetPolicyManager(ifindex)
		switch {
		case mgrErr != nil:
			p.learnErrors.Add(1)
		default:
			if err := mgr.LearnDNS(resp.QName, resp.Answers); err != nil {
				p.learnErrors.Add(1)
			}
		}
	}

	// Inject only after the learn has committed, so the sandbox never sees a
	// resolved address before the policy that permits it.
	if p.injector != nil {
		if err := p.injector.InjectDNSResponse(ifindex, frame); err != nil {
			p.injectErrors.Add(1)
		}
	}
}

// dnsPayload walks the Ethernet/IPv4/UDP headers to locate the DNS payload.
// IHL is honoured so a header carrying options is handled; the datapath
// guarantees IPv4/UDP with source port 53, and the checks here are bounds
// safety rather than classification.
func dnsPayload(frame []byte) ([]byte, error) {
	if len(frame) < ethHdrLen+ipv4MinHdrLen+udpHdrLen {
		return nil, fmt.Errorf("%w: frame too short (%d)", ErrDNSMalformed, len(frame))
	}
	if binary.BigEndian.Uint16(frame[12:14]) != unixEthPIP {
		return nil, fmt.Errorf("%w: not IPv4", ErrDNSMalformed)
	}
	ip := frame[ethHdrLen:]
	if ip[0]>>4 != 4 {
		return nil, fmt.Errorf("%w: not IPv4", ErrDNSMalformed)
	}
	ipHdrLen := int(ip[0]&0x0f) * 4
	if ipHdrLen < ipv4MinHdrLen || len(ip) < ipHdrLen+udpHdrLen {
		return nil, fmt.Errorf("%w: bad IPv4 header length (%d)", ErrDNSMalformed, ipHdrLen)
	}
	if ip[9] != unixIPProtoUDP {
		return nil, fmt.Errorf("%w: not UDP", ErrDNSMalformed)
	}
	udp := ip[ipHdrLen:]
	if srcPort := binary.BigEndian.Uint16(udp[0:2]); srcPort != 53 {
		return nil, fmt.Errorf("%w: not a DNS reply (sport=%d)", ErrDNSMalformed, srcPort)
	}
	payload := udp[udpHdrLen:]
	// The UDP length field bounds the payload when the frame carries padding.
	if udpLen := int(binary.BigEndian.Uint16(udp[4:6])); udpLen >= udpHdrLen && udpLen-udpHdrLen <= len(payload) {
		payload = payload[:udpLen-udpHdrLen]
	}
	if len(payload) < dnsHdrLen {
		return nil, fmt.Errorf("%w: DNS payload too short (%d)", ErrDNSMalformed, len(payload))
	}
	return payload, nil
}

const (
	unixEthPIP     = 0x0800
	unixIPProtoUDP = 17
)

// sweepOrphanPolicySnapshots deletes snapshots whose ifindex no longer has TAP
// metadata. CleanupTAPDevicePolicy removes a snapshot on the normal teardown
// path; this catches the ones a SIGKILLed process left behind, which would
// otherwise accumulate in tmpfs for every ifindex that ever existed.
func sweepOrphanPolicySnapshots() error {
	dir := currentPolicySnapshotDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read snapshot dir: %w", err)
	}

	live, err := liveTAPIfindices()
	if err != nil {
		// Without a reliable live set, deleting would risk dropping a good
		// snapshot. Leave everything alone.
		return err
	}

	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		ifindex, convErr := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 32)
		if convErr != nil {
			continue
		}
		if _, ok := live[uint32(ifindex)]; ok {
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, name)); rmErr != nil && !os.IsNotExist(rmErr) {
			errs = append(errs, rmErr)
		}
	}
	return errors.Join(errs...)
}

// liveTAPIfindices returns the ifindices that still have TAP metadata.
func liveTAPIfindices() (map[uint32]struct{}, error) {
	keys, err := outerMapKeys(MapNameIfindexToMVMMetadata, func() any { return new(mvmMetadata) })
	if err != nil {
		return nil, err
	}
	live := make(map[uint32]struct{}, len(keys))
	for _, k := range keys {
		live[k] = struct{}{}
	}
	return live, nil
}
