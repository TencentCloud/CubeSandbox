// DNS response parser used by the user-space learning pipeline.
//
// Delegates wire decoding to github.com/miekg/dns, which fully implements
// RFC 1035 (compression pointers, label validation, TTL, etc.) — far more
// robust than a hand-rolled subset, and no longer bounded by what the BPF
// verifier will accept. We keep a thin layer on top to:
//
//   - Enforce the strict subset we actually learn from: standard-query
//     responses with exactly one A/IN question.
//   - Clamp TTLs so a rule cannot outlive the DNS record's real validity, and
//     so a very short TTL cannot hot-spin between reap and learn.
//   - Expose only the fields the learner consumes (QName + IPv4 A answers),
//     keeping the poller decoupled from miekg's richer Msg type.
//
// We learn every A record in a response. The previous BPF implementation
// capped this at DNS_MAX_RESPONSE_ANSWERS (8) because the answer walk had to
// fit the instruction budget; legitimate DNS pools routinely return more, and
// the per-sandbox allow_out_v3 max-entry guard already bounds the learned set.

package cubevs

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/miekg/dns"
)

// TTL clamp bounds. The lower bound matches the floor the BPF learner applied
// before this moved to user space; the upper bound is new — BPF clamped only
// the floor, so a hostile or misconfigured resolver could pin a learned row
// for as long as it liked.
const (
	dnsMinTTL = 300
	dnsMaxTTL = 86400
)

// DNSAnswer is a single learned (IP, TTL) pair.
type DNSAnswer struct {
	IP         net.IP
	TTLSeconds uint32
}

// DNSResponse is the parsed form of a DNS reply.
type DNSResponse struct {
	// QName is the lower-cased dotted domain name from the question section,
	// without a trailing dot (i.e. "example.com" not "example.com.").
	QName   string
	Answers []DNSAnswer
}

// ErrDNSMalformed is returned for any wire-format violation. Callers treat
// this as "drop and let the DNS client retry" — there is no value in trying to
// salvage partial state from a bogus response.
var ErrDNSMalformed = errors.New("dns: malformed response")

// parseDNSResponse decodes a raw DNS reply. Returns ErrDNSMalformed for any
// violation; on success QName is non-empty and Answers may be empty (e.g.
// NODATA for A records where only AAAA exists).
func parseDNSResponse(payload []byte) (*DNSResponse, error) {
	var msg dns.Msg
	if err := msg.Unpack(payload); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDNSMalformed, err)
	}

	// miekg sets Response from the QR bit; require it. Also reject anything
	// that is not a clean standard reply.
	if !msg.Response {
		return nil, fmt.Errorf("%w: not a response", ErrDNSMalformed)
	}
	if msg.Opcode != dns.OpcodeQuery {
		return nil, fmt.Errorf("%w: non-standard opcode", ErrDNSMalformed)
	}
	if msg.Truncated {
		return nil, fmt.Errorf("%w: truncated bit set", ErrDNSMalformed)
	}
	if msg.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("%w: rcode=%d", ErrDNSMalformed, msg.Rcode)
	}
	if len(msg.Question) != 1 {
		return nil, fmt.Errorf("%w: %d questions", ErrDNSMalformed, len(msg.Question))
	}
	q := msg.Question[0]
	if q.Qtype != dns.TypeA || q.Qclass != dns.ClassINET {
		return nil, fmt.Errorf("%w: qtype=%d qclass=%d", ErrDNSMalformed, q.Qtype, q.Qclass)
	}

	// The learner keys rules by the lower-cased question name without a
	// trailing dot; miekg returns the FQDN with one.
	qname := strings.ToLower(dns.Fqdn(q.Name))
	qname = qname[:len(qname)-1]
	if qname == "" {
		return nil, fmt.Errorf("%w: empty question name", ErrDNSMalformed)
	}

	// Only IPv4 A answers contribute; everything else (AAAA, CNAME, OPT, ...)
	// is skipped so it does not block the A records.
	var answers []DNSAnswer
	for _, rr := range msg.Answer {
		a, ok := rr.(*dns.A)
		if !ok || a.A == nil {
			continue
		}
		ip := a.A.To4()
		if ip == nil {
			continue
		}
		answers = append(answers, DNSAnswer{IP: ip, TTLSeconds: clampTTL(a.Hdr.Ttl)})
	}

	return &DNSResponse{QName: qname, Answers: answers}, nil
}

func clampTTL(ttl uint32) uint32 {
	if ttl < dnsMinTTL {
		return dnsMinTTL
	}
	if ttl > dnsMaxTTL {
		return dnsMaxTTL
	}
	return ttl
}
