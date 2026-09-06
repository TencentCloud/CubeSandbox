// Raw-socket re-injector for DNS replies.
//
// Once the learner has written the allow_out_v3 rows, the uploaded frame has to
// reach the sandbox. It goes onto the TAP netdev's egress direction, which is
// where bpf_redirect() would have put it, and it needs no fixing up:
// dns_response_finish_prog reverse-NATs *before* uploading, so the destination,
// port and MACs are already the guest's.
//
// Injecting via cube-dev (what the production sender does) is not an option
// here. from_envoy picks the TAP by looking the pre-DNAT destination up in
// mvmip_to_ifindex, and neither end of this node's NAT yields an address that
// resolves: before the reverse NAT it is a node SNAT address shared by many
// sandboxes (identity is in the port, which from_envoy does not consult), and
// after it, mvm_inner_ip, a node-wide constant. The record's ifindex is the
// only thing that identifies the sandbox.
//
// One unbound socket serves every TAP: AF_PACKET writes are serialized per
// socket anyway, and sendto carries the target ifindex per frame.

package cubevs

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Static sentinels InjectDNSResponse can return before touching the socket.
var (
	errRawSenderClosed = errors.New("raw sender: closed")
	errFrameTooShort   = errors.New("raw sender: frame too short")
	errNoInjectTarget  = errors.New("raw sender: no target ifindex")
)

// dnsFrameMinLen is the smallest frame InjectDNSResponse will accept:
// Ethernet(14) + minimum IPv4(20) + UDP(8) + DNS header(12). Real frames may be
// larger (IP options), but nothing smaller can carry a DNS payload.
const dnsFrameMinLen = 14 + 20 + 8 + 12

// RawSender wraps a single AF_PACKET socket used to inject frames onto an
// arbitrary TAP.
type RawSender struct {
	fd int
}

// NewRawSender opens the AF_PACKET socket. It is deliberately left unbound so
// one socket can serve every sandbox; the protocol is 0 so the socket receives
// nothing and does not accumulate packets we would never read.
//
// Requires CAP_NET_RAW, which the Cubelet already holds (it attaches TC filters
// and writes sysctls).
func NewRawSender() (*RawSender, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		return nil, fmt.Errorf("AF_PACKET socket: %w", err)
	}
	return &RawSender{fd: fd}, nil
}

// Close releases the underlying socket. Safe to call more than once.
func (s *RawSender) Close() error {
	if s == nil || s.fd < 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	if err != nil {
		return fmt.Errorf("close AF_PACKET socket: %w", err)
	}
	return nil
}

// InjectDNSResponse writes the frame verbatim to the given TAP ifindex.
//
// frame must be a complete L2 packet, which is exactly what the datapath hands
// us: it is the post-NAT frame captured just before bpf_redirect() would have
// sent it. A vanished netdev (the TAP was torn down between upload and inject)
// surfaces as ENODEV/ENXIO and is the caller's cue to count and move on.
func (s *RawSender) InjectDNSResponse(ifindex uint32, frame []byte) error {
	if s == nil || s.fd < 0 {
		return errRawSenderClosed
	}
	if ifindex == 0 {
		return errNoInjectTarget
	}
	if len(frame) < dnsFrameMinLen {
		return fmt.Errorf("%w: %d bytes", errFrameTooShort, len(frame))
	}
	addr := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  int(ifindex),
		Halen:    6,
	}
	// SOCK_RAW does not build an L2 header, so the frame's own destination MAC
	// is what addresses the packet and Halen/Addr are not consulted for it.
	// They are filled in anyway to keep the sockaddr well-formed: the kernel
	// does bounds-check Halen against the supplied address length.
	copy(addr.Addr[:], frame[0:6])

	if err := unix.Sendto(s.fd, frame, 0, addr); err != nil {
		return fmt.Errorf("AF_PACKET sendto ifindex %d: %w", ifindex, err)
	}
	return nil
}
