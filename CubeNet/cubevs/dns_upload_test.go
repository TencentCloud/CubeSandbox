package cubevs

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH dnsupload ../src/dns_upload_test.bpf.c -- -I../vmlinux/$GOARCH

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/perf"
	"github.com/miekg/dns"
)

// Verdicts test_dns_upload reports back.
const (
	uploadVerdictForward = 0 // TC_ACT_OK: ordinary reverse-NAT path
	uploadVerdictUpload  = 2 // TC_ACT_SHOT: handed to user space
	uploadVerdictNoCase  = 3 // harness failed to stage the case
)

// Offsets in the synthetic frame built by buildDNSReplyFrame.
const (
	testEthLen  = 14
	testIPLen   = 20
	testUDPLen  = 8
	testDNSOff  = testEthLen + testIPLen + testUDPLen
	testSrvIP   = "203.0.113.53"
	testGuestIP = "169.254.68.6"
)

type dnsUploadTestEnv struct {
	program   *ebpf.Program
	meta      *ebpf.Map
	track     *ebpf.Map
	caseStore *ebpf.Map
	events    *ebpf.Map
}

func loadDNSUploadTestEnv(t *testing.T) *dnsUploadTestEnv {
	t.Helper()

	spec, err := loadDnsupload()
	if err != nil {
		t.Fatalf("load dns upload test spec: %v", err)
	}
	for name, mapSpec := range spec.Maps {
		switch name {
		case ".rodata", "ifindex_to_mvmmeta", "dns_query_track", "test_upload_case", "dns_events":
			mapSpec.Pinning = ebpf.PinNone
		default:
			delete(spec.Maps, name)
		}
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF dns upload test unavailable: %v", err)
		}
		t.Fatalf("load dns upload test collection: %v", err)
	}
	t.Cleanup(coll.Close)

	env := &dnsUploadTestEnv{
		program:   coll.Programs["test_dns_upload"],
		meta:      coll.Maps["ifindex_to_mvmmeta"],
		track:     coll.Maps["dns_query_track"],
		caseStore: coll.Maps["test_upload_case"],
		events:    coll.Maps["dns_events"],
	}
	if env.program == nil || env.meta == nil || env.track == nil ||
		env.caseStore == nil || env.events == nil {
		t.Fatal("loaded dns upload test program or maps missing")
	}
	return env
}

// dnsReplyOptions describes the DNS message buildDNSReplyFrame should encode.
type dnsReplyOptions struct {
	qname     string
	id        uint16
	qtype     uint16
	rcode     int
	truncated bool
	response  bool
	questions int
	answers   []string // A record addresses
	padTo     int      // pad the frame to at least this many bytes
}

func defaultDNSReplyOptions() dnsReplyOptions {
	return dnsReplyOptions{
		qname:     "qq.com",
		id:        0x1234,
		qtype:     dns.TypeA,
		rcode:     dns.RcodeSuccess,
		response:  true,
		questions: 1,
		answers:   []string{"1.2.3.4"},
	}
}

// buildDNSReplyFrame encodes a complete Ethernet/IPv4/UDP/DNS reply frame,
// matching the post-NAT shape the datapath uploads: source is the resolver,
// destination is the guest.
func buildDNSReplyFrame(t *testing.T, opts dnsReplyOptions) []byte {
	t.Helper()

	msg := new(dns.Msg)
	msg.Id = opts.id
	msg.Response = opts.response
	msg.Opcode = dns.OpcodeQuery
	msg.Rcode = opts.rcode
	msg.Truncated = opts.truncated
	fqdn := dns.Fqdn(opts.qname)
	for i := 0; i < opts.questions; i++ {
		msg.Question = append(msg.Question, dns.Question{
			Name: fqdn, Qtype: opts.qtype, Qclass: dns.ClassINET,
		})
	}
	for _, addr := range opts.answers {
		msg.Answer = append(msg.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: fqdn, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 600},
			A:   net.ParseIP(addr),
		})
	}
	payload, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack dns message: %v", err)
	}

	frame := make([]byte, testDNSOff+len(payload))
	// Ethernet: cubegw0 -> mvm, IPv4.
	copy(frame[0:6], []byte{0x20, 0x90, 0x6f, 0xfc, 0xfc, 0xfc})
	copy(frame[6:12], []byte{0x20, 0x90, 0x6f, 0xcf, 0xcf, 0xcf})
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	// IPv4 header.
	ip := frame[testEthLen:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(testIPLen+testUDPLen+len(payload)))
	ip[8] = 64
	ip[9] = 17 // UDP
	copy(ip[12:16], net.ParseIP(testSrvIP).To4())
	copy(ip[16:20], net.ParseIP(testGuestIP).To4())
	// UDP header: source 53.
	udp := frame[testEthLen+testIPLen:]
	binary.BigEndian.PutUint16(udp[0:2], 53)
	binary.BigEndian.PutUint16(udp[2:4], 40000)
	binary.BigEndian.PutUint16(udp[4:6], uint16(testUDPLen+len(payload)))
	copy(frame[testDNSOff:], payload)

	if opts.padTo > len(frame) {
		frame = append(frame, make([]byte, opts.padTo-len(frame))...)
	}
	return frame
}

// stageUploadCase points the program at one (ifindex, server, port) tuple.
func (env *dnsUploadTestEnv) stageUploadCase(t *testing.T, ifindex uint32, srcPort uint16) {
	t.Helper()
	var buf [16]byte
	binary.LittleEndian.PutUint32(buf[0:4], testDNSOff)
	binary.LittleEndian.PutUint32(buf[4:8], ifindex)
	binary.LittleEndian.PutUint32(buf[8:12], ipToUint32(net.ParseIP(testSrvIP)))
	binary.LittleEndian.PutUint16(buf[12:14], htonsPort(srcPort))
	key := uint32(0)
	if err := env.caseStore.Put(&key, &buf); err != nil {
		t.Fatalf("stage upload case: %v", err)
	}
}

func (env *dnsUploadTestEnv) enableLearning(t *testing.T, ifindex uint32, enabled bool) {
	t.Helper()
	meta := mvmMetadata{IP: ipToUint32(net.ParseIP(testGuestIP))}
	if enabled {
		meta.DNSPolicyFlags = dnsPolicyFlagLearningEnabled
	}
	if err := env.meta.Put(&ifindex, &meta); err != nil {
		t.Fatalf("seed mvm metadata: %v", err)
	}
}

// trackQuery installs the pending-query entry that authorizes a reply. expired
// entries are staged with an expiry in the past.
func (env *dnsUploadTestEnv) trackQuery(t *testing.T, ifindex uint32, srcPort, id uint16,
	qname string, expired bool,
) dnsQueryTrackKey {
	t.Helper()
	now, err := currentNS()
	if err != nil {
		t.Fatalf("currentNS: %v", err)
	}
	key := dnsQueryTrackKey{
		Ifindex:    ifindex,
		ServerIP:   ipToUint32(net.ParseIP(testSrvIP)),
		SourcePort: htonsPort(srcPort),
		DNSID:      htons(id),
		QnameHash:  dnsWireQNameHash(qname),
	}
	value := dnsQueryTrackValue{ExpiresAtNS: now + uint64(nsecPerSec)}
	if expired {
		value.ExpiresAtNS = now - 1
	}
	if err := env.track.Put(&key, &value); err != nil {
		t.Fatalf("seed dns_query_track: %v", err)
	}
	return key
}

func (env *dnsUploadTestEnv) run(t *testing.T, frame []byte) int {
	t.Helper()
	ret, _, err := env.program.Test(frame)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF dns upload test-run unavailable: %v", err)
		}
		t.Fatalf("run dns upload test: %v", err)
	}
	return int(int32(ret))
}

// dnsWireQNameHash reproduces dns_parser.h's FNV-1a over the wire-format
// QNAME. The datapath hashes the raw bytes with no case folding, which is why
// the query and its echoed response agree.
func dnsWireQNameHash(qname string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	hash := uint64(offset)
	step := func(b byte) {
		hash ^= uint64(b)
		hash *= prime
	}
	for _, label := range splitDNSLabels(qname) {
		step(byte(len(label)))
		for i := 0; i < len(label); i++ {
			step(label[i])
		}
	}
	step(0)
	return hash
}

func splitDNSLabels(qname string) []string {
	var out []string
	start := 0
	for i := 0; i < len(qname); i++ {
		if qname[i] == '.' {
			if i > start {
				out = append(out, qname[start:i])
			}
			start = i + 1
		}
	}
	if start < len(qname) {
		out = append(out, qname[start:])
	}
	return out
}

// TestDNSUploadTrackedReplyIsHandedToUserspace is the positive case: a reply
// that answers a tracked query is withheld from the sandbox so user space can
// learn from it first, and the pending query is consumed.
func TestDNSUploadTrackedReplyIsHandedToUserspace(t *testing.T) {
	env := loadDNSUploadTestEnv(t)
	const ifindex, srcPort, id = uint32(41), uint16(40000), uint16(0x1234)

	env.enableLearning(t, ifindex, true)
	key := env.trackQuery(t, ifindex, srcPort, id, "qq.com", false)
	env.stageUploadCase(t, ifindex, srcPort)

	got := env.run(t, buildDNSReplyFrame(t, defaultDNSReplyOptions()))
	if got != uploadVerdictUpload {
		t.Fatalf("verdict = %d, want upload (%d)", got, uploadVerdictUpload)
	}

	var value dnsQueryTrackValue
	if err := env.track.Lookup(&key, &value); err == nil {
		t.Fatal("pending query must be consumed once its reply is uploaded")
	}
}

// TestDNSUploadRecordCarriesTheWholeFrame is the one test that exercises how
// the packet actually reaches user space.
//
// bpf_perf_event_output's data argument only carries the 8-byte prefix. The
// frame rides along because the upper 32 bits of flags request that many bytes
// of packet data be appended to the sample — so the record is
// [prefix][frame], which is exactly where the poller splits it. Nothing else
// in the suite covers that seam: the poller tests synthesise records by hand,
// so if this mechanism were wrong every one of them would still pass while
// production learned nothing and injected nothing.
func TestDNSUploadRecordCarriesTheWholeFrame(t *testing.T) {
	env := loadDNSUploadTestEnv(t)
	const ifindex, srcPort, id = uint32(44), uint16(40000), uint16(0x1234)

	reader, err := perf.NewReader(env.events, 1<<20)
	if err != nil {
		t.Fatalf("perf.NewReader: %v", err)
	}
	defer reader.Close()

	env.enableLearning(t, ifindex, true)
	env.trackQuery(t, ifindex, srcPort, id, "qq.com", false)
	env.stageUploadCase(t, ifindex, srcPort)

	frame := buildDNSReplyFrame(t, defaultDNSReplyOptions())
	if got := env.run(t, frame); got != uploadVerdictUpload {
		t.Fatalf("verdict = %d, want upload (%d)", got, uploadVerdictUpload)
	}

	record, err := reader.Read()
	if err != nil {
		t.Fatalf("read perf record: %v", err)
	}
	if record.LostSamples != 0 {
		t.Fatalf("lost %d samples", record.LostSamples)
	}

	// perf rounds a raw sample up to 8-byte alignment, so the record holds at
	// least prefix+frame and may carry a few bytes of padding beyond it. That
	// is precisely why the prefix has to carry frame_len.
	least := dnsEventPrefixLen + len(frame)
	if len(record.RawSample) < least {
		t.Fatalf("record = %d bytes, want at least %d (8-byte prefix + %d-byte frame)",
			len(record.RawSample), least, len(frame))
	}

	prefix := record.RawSample[:dnsEventPrefixLen]
	if got := binary.LittleEndian.Uint16(prefix[0:2]); int(got) != len(frame) {
		t.Fatalf("prefix frame_len = %d, want %d", got, len(frame))
	}
	if got := binary.LittleEndian.Uint32(prefix[4:8]); got != ifindex {
		t.Fatalf("prefix ifindex = %d, want %d", got, ifindex)
	}

	// Bounded by frame_len, the appended bytes must be the frame exactly.
	body := record.RawSample[dnsEventPrefixLen : dnsEventPrefixLen+len(frame)]
	if string(body) != string(frame) {
		t.Fatal("appended packet bytes differ from the frame the program saw")
	}

	// Drive the real record through the poller: this is what proves the split
	// honours frame_len instead of handing the padding on to the injector.
	injector := &recordingInjector{}
	p := newTestPoller(injector)
	p.handleRecord(record.RawSample)

	injector.mu.Lock()
	defer injector.mu.Unlock()
	if len(injector.frames) != 1 {
		t.Fatalf("poller injected %d frames, want 1", len(injector.frames))
	}
	if len(injector.frames[0]) != len(frame) {
		t.Fatalf("poller injected %d bytes, want %d — trailing perf padding was not trimmed",
			len(injector.frames[0]), len(frame))
	}
	if string(injector.frames[0]) != string(frame) {
		t.Fatal("poller injected a frame that differs from the uploaded one")
	}
	if injector.ifindex[0] != ifindex {
		t.Fatalf("poller injected on ifindex %d, want %d", injector.ifindex[0], ifindex)
	}
}

// TestDNSUploadEmitsNothingWhenForwarding: a reply that stays on the datapath
// must not produce a record, or user space would inject a duplicate.
func TestDNSUploadEmitsNothingWhenForwarding(t *testing.T) {
	env := loadDNSUploadTestEnv(t)
	const ifindex, srcPort = uint32(45), uint16(40000)

	reader, err := perf.NewReader(env.events, 1<<20)
	if err != nil {
		t.Fatalf("perf.NewReader: %v", err)
	}
	defer reader.Close()

	// Learning enabled but no tracked query: must forward, not upload.
	env.enableLearning(t, ifindex, true)
	env.stageUploadCase(t, ifindex, srcPort)
	if got := env.run(t, buildDNSReplyFrame(t, defaultDNSReplyOptions())); got != uploadVerdictForward {
		t.Fatalf("verdict = %d, want forward", got)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	if _, err := reader.Read(); !errors.Is(err, perf.ErrClosed) {
		t.Fatalf("expected no record before close, got err=%v", err)
	}
}

// TestDNSUploadForwardsWhenNotLearnable pins the cases that must keep taking
// the ordinary reverse-NAT path: the sandbox still gets its reply, it is just
// not learned from.
func TestDNSUploadForwardsWhenNotLearnable(t *testing.T) {
	const srcPort, id = uint16(40000), uint16(0x1234)

	cases := []struct {
		name     string
		learning bool
		track    bool
		expired  bool
		// trackQName seeds the pending query under a different name to force a
		// qname_hash mismatch.
		trackQName string
		opts       func(o *dnsReplyOptions)
	}{
		{name: "untracked", learning: true, track: false},
		{name: "learning disabled", learning: false, track: true},
		{name: "expired track entry", learning: true, track: true, expired: true},
		{name: "qname mismatch", learning: true, track: true, trackQName: "other.com"},
		{
			name: "non-A question", learning: true, track: true,
			opts: func(o *dnsReplyOptions) { o.qtype = dns.TypeAAAA },
		},
		{
			name: "two questions", learning: true, track: true,
			opts: func(o *dnsReplyOptions) { o.questions = 2 },
		},
		{
			name: "servfail", learning: true, track: true,
			opts: func(o *dnsReplyOptions) { o.rcode = dns.RcodeServerFailure },
		},
		{
			name: "truncated", learning: true, track: true,
			opts: func(o *dnsReplyOptions) { o.truncated = true },
		},
		{
			name: "not a response", learning: true, track: true,
			opts: func(o *dnsReplyOptions) { o.response = false },
		},
		{
			name: "oversized frame", learning: true, track: true,
			opts: func(o *dnsReplyOptions) { o.padTo = dnsEventMaxFrame + 64 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := loadDNSUploadTestEnv(t)
			ifindex := uint32(42)

			env.enableLearning(t, ifindex, tc.learning)
			if tc.track {
				name := "qq.com"
				if tc.trackQName != "" {
					name = tc.trackQName
				}
				env.trackQuery(t, ifindex, srcPort, id, name, tc.expired)
			}
			env.stageUploadCase(t, ifindex, srcPort)

			opts := defaultDNSReplyOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			got := env.run(t, buildDNSReplyFrame(t, opts))
			if got != uploadVerdictForward {
				t.Fatalf("verdict = %d, want forward (%d)", got, uploadVerdictForward)
			}
		})
	}
}
