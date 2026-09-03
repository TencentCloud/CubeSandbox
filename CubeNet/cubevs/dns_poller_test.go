package cubevs

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/miekg/dns"
)

// recordingInjector captures what the poller hands back to the datapath, and
// notes whether the learn had already committed when it was called.
type recordingInjector struct {
	mu       sync.Mutex
	frames   [][]byte
	ifindex  []uint32
	onInject func()
	err      error
}

func (r *recordingInjector) InjectDNSResponse(ifindex uint32, frame []byte) error {
	r.mu.Lock()
	r.frames = append(r.frames, append([]byte(nil), frame...))
	r.ifindex = append(r.ifindex, ifindex)
	hook := r.onInject
	err := r.err
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (r *recordingInjector) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// dnsEventRecord assembles one perfbuf record: the 8-byte prefix followed by
// the frame.
func dnsEventRecord(ifindex uint32, frame []byte) (prefix, body []byte) {
	prefix = make([]byte, dnsEventPrefixLen)
	binary.LittleEndian.PutUint16(prefix[0:2], uint16(len(frame)))
	binary.LittleEndian.PutUint32(prefix[4:8], ifindex)
	return prefix, frame
}

func newTestPoller(injector dnsInjector) *DNSPoller {
	return &DNSPoller{injector: injector}
}

// TestPollerLearnsThenInjects is the ordering guarantee that makes the
// datapath's TC_ACT_SHOT safe: the sandbox must not see a resolved address
// before the row permitting it exists. The injector asserts the row is already
// installed at the moment it is called.
func TestPollerLearnsThenInjects(t *testing.T) {
	const ifindex = uint32(820)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	var rowPresentAtInject bool
	injector := &recordingInjector{}
	injector.onInject = func() {
		_, rowPresentAtInject = allowRow(t, ifindex, "1.2.3.4", 0)
	}

	p := newTestPoller(injector)
	frame := buildDNSReplyFrame(t, dnsReplyOptions{
		qname: "qq.com", id: 0x1234, qtype: dns.TypeA, rcode: dns.RcodeSuccess,
		response: true, questions: 1, answers: []string{"1.2.3.4"},
	})
	p.handleFrame(dnsEventRecord(ifindex, frame))

	if !rowPresentAtInject {
		t.Fatal("frame was injected before the allow_out_v3 row existed")
	}
	if injector.count() != 1 {
		t.Fatalf("injected %d frames, want 1", injector.count())
	}
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestPollerInjectsFrameVerbatim: the datapath uploads the frame *after*
// reverse NAT, so user space must not touch it — it is already byte-for-byte
// what bpf_redirect would have delivered.
func TestPollerInjectsFrameVerbatim(t *testing.T) {
	const ifindex = uint32(821)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	injector := &recordingInjector{}
	p := newTestPoller(injector)
	frame := buildDNSReplyFrame(t, defaultDNSReplyOptions())
	p.handleFrame(dnsEventRecord(ifindex, frame))

	injector.mu.Lock()
	defer injector.mu.Unlock()
	if len(injector.frames) != 1 {
		t.Fatalf("injected %d frames, want 1", len(injector.frames))
	}
	if string(injector.frames[0]) != string(frame) {
		t.Fatal("injected frame differs from the uploaded frame")
	}
	if injector.ifindex[0] != ifindex {
		t.Fatalf("injected on ifindex %d, want %d", injector.ifindex[0], ifindex)
	}
}

// TestPollerInjectsUnlearnableFrames: a reply the learner cannot use is still
// the sandbox's reply. Since the datapath already dropped it, failing to inject
// would lose it outright.
func TestPollerInjectsUnlearnableFrames(t *testing.T) {
	const ifindex = uint32(822)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	cases := []struct {
		name string
		opts dnsReplyOptions
	}{
		{
			name: "unmatched qname",
			opts: dnsReplyOptions{
				qname: "evil.com", id: 1, qtype: dns.TypeA, rcode: dns.RcodeSuccess,
				response: true, questions: 1, answers: []string{"9.9.9.9"},
			},
		},
		{
			name: "nxdomain",
			opts: dnsReplyOptions{
				qname: "qq.com", id: 2, qtype: dns.TypeA, rcode: dns.RcodeNameError,
				response: true, questions: 1,
			},
		},
		{
			name: "nodata",
			opts: dnsReplyOptions{
				qname: "qq.com", id: 3, qtype: dns.TypeA, rcode: dns.RcodeSuccess,
				response: true, questions: 1,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			injector := &recordingInjector{}
			p := newTestPoller(injector)
			p.handleFrame(dnsEventRecord(ifindex, buildDNSReplyFrame(t, tc.opts)))
			if injector.count() != 1 {
				t.Fatalf("injected %d frames, want 1", injector.count())
			}
		})
	}

	requireNoAllowRow(t, ifindex, "9.9.9.9", 0)
}

// TestPollerDropsStructurallyInvalidFrames: a frame that is not even IPv4/UDP
// port 53 has nothing meaningful to deliver, so it is counted and dropped.
func TestPollerDropsStructurallyInvalidFrames(t *testing.T) {
	injector := &recordingInjector{}
	p := newTestPoller(injector)

	p.handleFrame(dnsEventRecord(1, []byte{0x01, 0x02, 0x03}))
	if injector.count() != 0 {
		t.Fatal("a runt frame must not be injected")
	}
	if p.Stats().ParseErrors == 0 {
		t.Fatal("parse errors must be counted")
	}
}

// TestPollerCountsInjectFailures: a TAP torn down between upload and inject
// surfaces as a send error, which must be counted rather than escalated.
func TestPollerCountsInjectFailures(t *testing.T) {
	const ifindex = uint32(823)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"qq.com"}, nil, nil))

	injector := &recordingInjector{err: errors.New("ENODEV")}
	p := newTestPoller(injector)
	p.handleFrame(dnsEventRecord(ifindex, buildDNSReplyFrame(t, defaultDNSReplyOptions())))

	if got := p.Stats().InjectErrors; got != 1 {
		t.Fatalf("inject errors = %d, want 1", got)
	}
	// The learn still happened: the row is what matters for the retry.
	requireAllowRow(t, ifindex, "1.2.3.4", 0)
}

// TestDNSPayloadWalksIPv4Options: the datapath hands us the frame as-is, so the
// payload walk has to honour IHL rather than assume a 20-byte header.
func TestDNSPayloadWalksIPv4Options(t *testing.T) {
	base := buildDNSReplyFrame(t, defaultDNSReplyOptions())
	payload, err := dnsPayload(base)
	if err != nil {
		t.Fatalf("dnsPayload: %v", err)
	}

	// Rebuild with 4 bytes of IPv4 options (IHL 6).
	withOpts := make([]byte, 0, len(base)+4)
	withOpts = append(withOpts, base[:testEthLen]...)
	ip := append([]byte(nil), base[testEthLen:testEthLen+testIPLen]...)
	ip[0] = 0x46 // version 4, IHL 6
	withOpts = append(withOpts, ip...)
	withOpts = append(withOpts, 0x01, 0x01, 0x01, 0x00) // NOP/EOL options
	withOpts = append(withOpts, base[testEthLen+testIPLen:]...)

	gotPayload, err := dnsPayload(withOpts)
	if err != nil {
		t.Fatalf("dnsPayload with options: %v", err)
	}
	if string(gotPayload) != string(payload) {
		t.Fatal("IPv4 options were not skipped correctly")
	}
}

func TestDNSPayloadRejectsNonDNS(t *testing.T) {
	good := buildDNSReplyFrame(t, defaultDNSReplyOptions())

	cases := []struct {
		name   string
		mutate func(f []byte) []byte
	}{
		{name: "runt", mutate: func([]byte) []byte { return []byte{1, 2, 3} }},
		{
			name: "not IPv4 ethertype",
			mutate: func(f []byte) []byte {
				out := append([]byte(nil), f...)
				binary.BigEndian.PutUint16(out[12:14], 0x86dd)
				return out
			},
		},
		{
			name: "not UDP",
			mutate: func(f []byte) []byte {
				out := append([]byte(nil), f...)
				out[testEthLen+9] = 6 // TCP
				return out
			},
		},
		{
			name: "source port not 53",
			mutate: func(f []byte) []byte {
				out := append([]byte(nil), f...)
				binary.BigEndian.PutUint16(out[testEthLen+testIPLen:testEthLen+testIPLen+2], 5353)
				return out
			},
		},
		{
			name: "bad IHL",
			mutate: func(f []byte) []byte {
				out := append([]byte(nil), f...)
				out[testEthLen] = 0x44 // IHL 4 => 16 bytes, below the minimum
				return out
			},
		},
		{
			name: "payload shorter than a DNS header",
			mutate: func(f []byte) []byte {
				return append([]byte(nil), f[:testDNSOff+4]...)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := dnsPayload(tc.mutate(good)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// TestDNSPayloadHonoursUDPLength: an injected frame may be padded (the minimum
// Ethernet frame is 60 bytes), and the padding must not reach the DNS parser.
func TestDNSPayloadHonoursUDPLength(t *testing.T) {
	frame := buildDNSReplyFrame(t, defaultDNSReplyOptions())
	exact, err := dnsPayload(frame)
	if err != nil {
		t.Fatalf("dnsPayload: %v", err)
	}

	padded := append(append([]byte(nil), frame...), make([]byte, 32)...)
	got, err := dnsPayload(padded)
	if err != nil {
		t.Fatalf("dnsPayload(padded): %v", err)
	}
	if len(got) != len(exact) {
		t.Fatalf("padded payload = %d bytes, want %d", len(got), len(exact))
	}
	// It must still parse: trailing padding would make miekg reject it.
	if _, err := parseDNSResponse(got); err != nil {
		t.Fatalf("padded payload did not parse: %v", err)
	}
}

// TestPollerIgnoresZeroIfindex: a record with no owner cannot be attributed to
// a sandbox, so nothing is learned.
func TestPollerIgnoresZeroIfindex(t *testing.T) {
	injector := &recordingInjector{}
	p := newTestPoller(injector)
	p.handleFrame(dnsEventRecord(0, buildDNSReplyFrame(t, defaultDNSReplyOptions())))
	if p.Stats().LearnErrors != 0 {
		t.Fatalf("learn errors = %d, want 0", p.Stats().LearnErrors)
	}
}

// TestSweepOrphanPolicySnapshots: teardown removes a snapshot on the normal
// path, but a SIGKILLed process leaves them behind. Without this sweep tmpfs
// would accumulate one file per ifindex that ever existed.
func TestSweepOrphanPolicySnapshots(t *testing.T) {
	const liveIfindex = uint32(830)
	newPolicyManagerTest(t, liveIfindex)

	dir := currentPolicySnapshotDir()
	mustUpdatePolicy(t, liveIfindex, mvmOptions([]string{"qq.com"}, nil, nil))

	orphan := filepath.Join(dir, "99999.json")
	if err := os.WriteFile(orphan, []byte(`{"schema_version":1,"ifindex":99999}`), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}

	if err := sweepOrphanPolicySnapshots(); err != nil {
		t.Fatalf("sweepOrphanPolicySnapshots: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphaned snapshot not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "830.json")); err != nil {
		t.Fatalf("live snapshot removed: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("non-snapshot file removed: %v", err)
	}
}

// TestRawSenderRejectsBadInput covers the guards that run before the syscall.
func TestRawSenderRejectsBadInput(t *testing.T) {
	var closed *RawSender
	if err := closed.InjectDNSResponse(1, make([]byte, dnsFrameMinLen)); !errors.Is(err, errRawSenderClosed) {
		t.Fatalf("nil sender: err = %v, want errRawSenderClosed", err)
	}

	s := &RawSender{fd: 3}
	if err := s.InjectDNSResponse(0, make([]byte, dnsFrameMinLen)); !errors.Is(err, errNoInjectTarget) {
		t.Fatalf("zero ifindex: err = %v, want errNoInjectTarget", err)
	}
	if err := s.InjectDNSResponse(1, []byte{1, 2, 3}); !errors.Is(err, errFrameTooShort) {
		t.Fatalf("runt frame: err = %v, want errFrameTooShort", err)
	}
}

// TestRawSenderOpensUnboundSocket: one socket serves every TAP, with the target
// carried per frame. Binding to a single device — the shape the production
// sender uses for cube-dev — would not work here.
func TestRawSenderOpensUnboundSocket(t *testing.T) {
	s, err := NewRawSender()
	if err != nil {
		t.Skipf("AF_PACKET unavailable: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if s.fd < 0 {
		t.Fatal("socket not open")
	}
	// Injecting to an ifindex that does not exist must surface as an error
	// rather than panic; the poller counts it and moves on.
	frame := buildDNSReplyFrame(t, defaultDNSReplyOptions())
	if err := s.InjectDNSResponse(1<<30, frame); err == nil {
		t.Fatal("expected an error for a nonexistent ifindex")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

// TestPollerConcurrentFramesAcrossSandboxes: workers run in parallel across
// ifindices, so the per-sandbox lock must keep the learned state consistent.
func TestPollerConcurrentFramesAcrossSandboxes(t *testing.T) {
	const ifindex = uint32(831)
	newPolicyManagerTest(t, ifindex)
	mustUpdatePolicy(t, ifindex, mvmOptions([]string{"*.qq.com"}, nil, nil))

	injector := &recordingInjector{}
	p := newTestPoller(injector)

	var wg sync.WaitGroup
	names := []string{"a.qq.com", "b.qq.com", "c.qq.com", "d.qq.com"}
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			frame := buildDNSReplyFrame(t, dnsReplyOptions{
				qname: name, id: uint16(i + 1), qtype: dns.TypeA,
				rcode: dns.RcodeSuccess, response: true, questions: 1,
				answers: []string{net.IPv4(10, 1, 0, byte(i+1)).String()},
			})
			p.handleFrame(dnsEventRecord(ifindex, frame))
		}(i, name)
	}
	wg.Wait()

	for i := range names {
		requireAllowRow(t, ifindex, net.IPv4(10, 1, 0, byte(i+1)).String(), 0)
	}
	if injector.count() != len(names) {
		t.Fatalf("injected %d frames, want %d", injector.count(), len(names))
	}
}
