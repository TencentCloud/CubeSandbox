package cubevs

import (
	"errors"
	"net"
	"testing"

	"github.com/miekg/dns"
)

// packDNS encodes msg, failing the test on error.
func packDNS(t *testing.T, msg *dns.Msg) []byte {
	t.Helper()
	payload, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack dns message: %v", err)
	}
	return payload
}

// aReply builds a well-formed A reply for qname with the given answers.
func aReply(qname string, answers ...dns.RR) *dns.Msg {
	msg := new(dns.Msg)
	msg.Id = 0x4242
	msg.Response = true
	msg.Opcode = dns.OpcodeQuery
	msg.Rcode = dns.RcodeSuccess
	msg.Question = []dns.Question{{
		Name: dns.Fqdn(qname), Qtype: dns.TypeA, Qclass: dns.ClassINET,
	}}
	msg.Answer = answers
	return msg
}

func aRecord(qname, addr string, ttl uint32) dns.RR {
	return &dns.A{
		Hdr: dns.RR_Header{Name: dns.Fqdn(qname), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   net.ParseIP(addr),
	}
}

func TestParseDNSResponseAcceptsWellFormedReply(t *testing.T) {
	payload := packDNS(t, aReply("QQ.CoM",
		aRecord("QQ.CoM", "1.2.3.4", 600),
		aRecord("QQ.CoM", "5.6.7.8", 600),
	))

	resp, err := parseDNSResponse(payload)
	if err != nil {
		t.Fatalf("parseDNSResponse: %v", err)
	}
	// The learner keys on the normalised name: lower-case, no trailing dot.
	if resp.QName != "qq.com" {
		t.Fatalf("QName = %q, want %q", resp.QName, "qq.com")
	}
	if len(resp.Answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(resp.Answers))
	}
	if got := resp.Answers[0].IP.String(); got != "1.2.3.4" {
		t.Fatalf("answers[0] = %s, want 1.2.3.4", got)
	}
}

// TestParseDNSResponseRejectsMalformed covers the strict subset the learner
// accepts. Anything else must be reported so the poller counts it and simply
// re-injects the frame without learning.
func TestParseDNSResponseRejectsMalformed(t *testing.T) {
	cases := []struct {
		name    string
		payload func(t *testing.T) []byte
	}{
		{
			name:    "truncated wire data",
			payload: func(*testing.T) []byte { return []byte{0x42} },
		},
		{
			name:    "garbage",
			payload: func(*testing.T) []byte { return []byte("not a dns message at all") },
		},
		{
			name: "not a response",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Response = false
				return packDNS(t, msg)
			},
		},
		{
			name: "non-standard opcode",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Opcode = dns.OpcodeStatus
				return packDNS(t, msg)
			},
		},
		{
			name: "TC bit set",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com", aRecord("qq.com", "1.2.3.4", 600))
				msg.Truncated = true
				return packDNS(t, msg)
			},
		},
		{
			name: "servfail rcode",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Rcode = dns.RcodeServerFailure
				return packDNS(t, msg)
			},
		},
		{
			name: "nxdomain rcode",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Rcode = dns.RcodeNameError
				return packDNS(t, msg)
			},
		},
		{
			name: "zero questions",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Question = nil
				return packDNS(t, msg)
			},
		},
		{
			name: "two questions",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Question = append(msg.Question, msg.Question[0])
				return packDNS(t, msg)
			},
		},
		{
			name: "AAAA question",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Question[0].Qtype = dns.TypeAAAA
				return packDNS(t, msg)
			},
		},
		{
			name: "CHAOS class question",
			payload: func(t *testing.T) []byte {
				msg := aReply("qq.com")
				msg.Question[0].Qclass = dns.ClassCHAOS
				return packDNS(t, msg)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := parseDNSResponse(tc.payload(t))
			if err == nil {
				t.Fatalf("expected rejection, got %+v", resp)
			}
			if !errors.Is(err, ErrDNSMalformed) {
				t.Fatalf("error = %v, want ErrDNSMalformed", err)
			}
		})
	}
}

// TestParseDNSResponseSkipsNonARecords keeps a mixed answer section from
// blocking the A records the learner wants.
func TestParseDNSResponseSkipsNonARecords(t *testing.T) {
	msg := aReply("qq.com",
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: dns.Fqdn("qq.com"), Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 600},
			Target: dns.Fqdn("cdn.qq.com"),
		},
		aRecord("cdn.qq.com", "1.2.3.4", 600),
		&dns.AAAA{
			Hdr:  dns.RR_Header{Name: dns.Fqdn("cdn.qq.com"), Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 600},
			AAAA: net.ParseIP("2001:db8::1"),
		},
	)

	resp, err := parseDNSResponse(packDNS(t, msg))
	if err != nil {
		t.Fatalf("parseDNSResponse: %v", err)
	}
	if len(resp.Answers) != 1 || resp.Answers[0].IP.String() != "1.2.3.4" {
		t.Fatalf("answers = %+v, want only 1.2.3.4", resp.Answers)
	}
}

// TestParseDNSResponseNoDataIsNotAnError: a NOERROR reply with no A records
// (e.g. AAAA-only host) is valid. Nothing is learned, and the poller still
// re-injects the frame.
func TestParseDNSResponseNoDataIsNotAnError(t *testing.T) {
	resp, err := parseDNSResponse(packDNS(t, aReply("qq.com")))
	if err != nil {
		t.Fatalf("parseDNSResponse: %v", err)
	}
	if len(resp.Answers) != 0 {
		t.Fatalf("answers = %+v, want none", resp.Answers)
	}
	if resp.QName != "qq.com" {
		t.Fatalf("QName = %q", resp.QName)
	}
}

// TestParseDNSResponseHasNoAnswerCap: the retired BPF learner capped answers at
// DNS_MAX_RESPONSE_ANSWERS (8) to fit the instruction budget. User space has no
// such limit.
func TestParseDNSResponseHasNoAnswerCap(t *testing.T) {
	const want = 20
	answers := make([]dns.RR, 0, want)
	for i := 0; i < want; i++ {
		answers = append(answers, aRecord("qq.com", net.IPv4(10, 0, 0, byte(i+1)).String(), 600))
	}

	resp, err := parseDNSResponse(packDNS(t, aReply("qq.com", answers...)))
	if err != nil {
		t.Fatalf("parseDNSResponse: %v", err)
	}
	if len(resp.Answers) != want {
		t.Fatalf("answers = %d, want %d (no cap)", len(resp.Answers), want)
	}
}

// TestParseDNSResponseClampsTTL: the floor keeps a very short TTL from
// hot-spinning between reap and learn; the ceiling is new in user space (BPF
// clamped only the floor), so a resolver cannot pin a learned row indefinitely.
func TestParseDNSResponseClampsTTL(t *testing.T) {
	cases := []struct {
		ttl  uint32
		want uint32
	}{
		{ttl: 0, want: dnsMinTTL},
		{ttl: 1, want: dnsMinTTL},
		{ttl: dnsMinTTL - 1, want: dnsMinTTL},
		{ttl: dnsMinTTL, want: dnsMinTTL},
		{ttl: 3600, want: 3600},
		{ttl: dnsMaxTTL, want: dnsMaxTTL},
		{ttl: dnsMaxTTL + 1, want: dnsMaxTTL},
		{ttl: 1 << 30, want: dnsMaxTTL},
	}
	for _, tc := range cases {
		resp, err := parseDNSResponse(packDNS(t, aReply("qq.com", aRecord("qq.com", "1.2.3.4", tc.ttl))))
		if err != nil {
			t.Fatalf("parseDNSResponse(ttl=%d): %v", tc.ttl, err)
		}
		if len(resp.Answers) != 1 {
			t.Fatalf("ttl=%d: answers = %d, want 1", tc.ttl, len(resp.Answers))
		}
		if got := resp.Answers[0].TTLSeconds; got != tc.want {
			t.Fatalf("ttl=%d clamped to %d, want %d", tc.ttl, got, tc.want)
		}
	}
}
