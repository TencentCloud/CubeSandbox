package cubevs

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const (
	MapNameIngressSessions = "ingress_sessions"
	MapNameEgressSessions  = "egress_sessions"
)

const (
	reapSessionsInterval = time.Second * 5
	maxSessions          = 1048576
	maxSessionPercentage = 0.8
)

type tcpConntrackState uint8

// enum tcp_conntrack in kernel.
const (
	tcpCTNone        tcpConntrackState = 0
	tcpCTSynSent     tcpConntrackState = 1
	tcpCTSynRecv     tcpConntrackState = 2
	tcpCTEstablished tcpConntrackState = 3
	tcpCTFinWait     tcpConntrackState = 4
	tcpCTCloseWait   tcpConntrackState = 5
	tcpCTLastAck     tcpConntrackState = 6
	tcpCTTimeWait    tcpConntrackState = 7
	tcpCTClose       tcpConntrackState = 8
	tcpCTSynSent2    tcpConntrackState = 9  // listen
	tcpCTInvalid     tcpConntrackState = 10 // max
	tcpCTIgnored     tcpConntrackState = 11
)

type udpConntrackState uint8

// enum udp_conntrack in kernel.
const (
	udpCTUnreplied udpConntrackState = 0
	udpCTReplied   udpConntrackState = 1
)

// icmpConntrackState mirrors ICMP_CT_* constants in icmp.h.
type icmpConntrackState uint8

const (
	icmpCTUnreplied icmpConntrackState = 0
	icmpCTReplied   icmpConntrackState = 1
)

func (s tcpConntrackState) String() string {
	switch s {
	case tcpCTNone:
		return "none"
	case tcpCTSynSent:
		return "syn_sent"
	case tcpCTSynRecv:
		return "syn_recv"
	case tcpCTEstablished:
		return "established"
	case tcpCTFinWait:
		return "fin_wait"
	case tcpCTCloseWait:
		return "close_wait"
	case tcpCTLastAck:
		return "last_ack"
	case tcpCTTimeWait:
		return "time_wait"
	case tcpCTClose:
		return "close"
	case tcpCTSynSent2:
		return "syn_sent2"
	case tcpCTInvalid:
		return "invalid"
	case tcpCTIgnored:
		return "ignored"
	}
	return strconv.Itoa(int(s))
}

func (s udpConntrackState) String() string {
	switch s {
	case udpCTUnreplied:
		return "unreplied"
	case udpCTReplied:
		return "replied"
	}
	return strconv.Itoa(int(s))
}

func (s icmpConntrackState) String() string {
	switch s {
	case icmpCTUnreplied:
		return "unreplied"
	case icmpCTReplied:
		return "replied"
	}
	return strconv.Itoa(int(s))
}

var (
	once     sync.Once
	vsEvents = make(chan Event, 1024)
)

var tcpTimeouts = map[tcpConntrackState]time.Duration{
	tcpCTNone:        time.Second * 0, // 2 mins in kernel
	tcpCTSynSent:     time.Minute,     // 2 mins in kernel
	tcpCTSynRecv:     time.Minute,     // the same
	tcpCTEstablished: time.Hour * 3,   // 5 days in kernel
	tcpCTFinWait:     time.Minute * 2,
	tcpCTCloseWait:   time.Minute,
	tcpCTLastAck:     time.Second * 30,
	tcpCTTimeWait:    time.Minute * 2,
	tcpCTClose:       time.Second * 10,
	tcpCTSynSent2:    time.Minute, // 2 mins in kernel
	tcpCTInvalid:     0,
	tcpCTIgnored:     0,
}

var udpTimeouts = map[udpConntrackState]time.Duration{
	udpCTUnreplied: time.Second * 30,  // UDP_TIMEOUT_UNREPLIED in kernel
	udpCTReplied:   time.Second * 180, // UDP_TIMEOUT_REPLIED in kernel
}

// icmpTimeout is the fixed timeout for ICMP echo sessions (30 s).
// Matches ICMP_TIMEOUT in icmp.h.
const icmpTimeout = time.Second * 30

var (
	ErrSessionsTooMany         = errors.New("too many sessions")
	ErrSessionExpiredNotClosed = errors.New("sessions expired but not closed")
)

type Event struct {
	Error   error
	Message string
}

type sessionKey struct {
	SourceIP   uint32
	TargetIP   uint32
	SourcePort uint16
	TargetPort uint16
	Version    uint32
	Protocol   uint8
	Reserved   [3]uint8
}

type natSession struct {
	AccessTime  uint64
	NodeIfindex uint32
	NodeIP      uint32
	VMIfindex   uint32
	VMIP        uint32
	NodePort    uint16
	VMPort      uint16
	State       uint8
	ActiveClose uint8
	Reserved    [34]uint8
}

// timeout returns the timeout for the session in nanoseconds.
func (s *natSession) tcpTimeout() uint64 {
	if s.State == uint8(tcpCTTimeWait) && s.ActiveClose == 1 {
		// The guest kernel active close the connection
		return uint64(tcpTimeouts[tcpCTClose].Nanoseconds())
	}

	return uint64(tcpTimeouts[tcpConntrackState(s.State)].Nanoseconds())
}

func (s *natSession) udpTimeout() uint64 {
	return uint64(udpTimeouts[udpConntrackState(s.State)].Nanoseconds())
}

func (s *natSession) icmpTimeout() uint64 {
	return uint64(icmpTimeout.Nanoseconds())
}

func sessionStateString(protocol uint8, state uint8) string {
	switch protocol {
	case unix.IPPROTO_UDP:
		return udpConntrackState(state).String()
	case unix.IPPROTO_ICMP:
		return icmpConntrackState(state).String()
	default:
		return tcpConntrackState(state).String()
	}
}

func egressSession(key *sessionKey, value *natSession, now uint64) string {
	var timeout uint64
	switch key.Protocol {
	case unix.IPPROTO_UDP:
		timeout = value.udpTimeout()
	case unix.IPPROTO_ICMP:
		timeout = value.icmpTimeout()
	default:
		timeout = value.tcpTimeout()
	}
	duration := time.Duration(value.AccessTime+timeout-now) * time.Nanosecond
	return fmt.Sprintf("%s:%d(%s:%d)->%s:%d in state %s, expire in %s",
		uint32ToIP(key.SourceIP), ntohs(key.SourcePort), uint32ToIP(value.NodeIP), ntohs(value.NodePort),
		uint32ToIP(key.TargetIP), ntohs(key.TargetPort), sessionStateString(key.Protocol, value.State),
		duration.String())
}

type ingressSessionValue struct {
	Version  uint32
	VMIP     uint32
	VMPort   uint16
	Reserved [3]uint16
}

//nolint:unused
func ingressSession(key *sessionKey, value *ingressSessionValue) string {
	return fmt.Sprintf("%s:%d->%s:%d(%s:%d)",
		uint32ToIP(key.SourceIP), ntohs(key.SourcePort),
		uint32ToIP(key.TargetIP), ntohs(key.TargetPort),
		uint32ToIP(value.VMIP), ntohs(value.VMPort))
}

// StartSessionReaper starts a goroutine that will periodically
// check for sessions and DNS-learned policies that have expired and remove them.
func StartSessionReaper() <-chan Event {
	once.Do(func() {
		go doReap()
	})
	return vsEvents
}

func doReap() {
	ticker := time.NewTicker(reapSessionsInterval)
	defer ticker.Stop()

	for range ticker.C {
		reapSessions()
		reapDNSState()
	}
}

func currentNS() (uint64, error) {
	t := &unix.Timespec{}
	err := unix.ClockGettime(unix.CLOCK_MONOTONIC, t)
	if err != nil {
		return 0, fmt.Errorf("unix.ClockGettime failed: %w", err)
	}
	return uint64(t.Nano()), nil
}

func enqueueEvent(event Event) {
	select {
	case vsEvents <- event:
	default:
		// If the channel is full, drop the event.
		break
	}
}

func reportCount(count int) {
	if float64(count) > maxSessions*maxSessionPercentage {
		enqueueEvent(Event{
			Error:   ErrSessionsTooMany,
			Message: fmt.Sprintf("too many sessions: %d/%d", count, maxSessions),
		})
	}
}

func sessionExpired(now uint64, key *sessionKey, sess *natSession) bool {
	var timeout uint64
	switch key.Protocol {
	case unix.IPPROTO_UDP:
		timeout = sess.udpTimeout()
	case unix.IPPROTO_ICMP:
		timeout = sess.icmpTimeout()
	default:
		timeout = sess.tcpTimeout()
	}
	return now > sess.AccessTime+timeout
}

// sessionClosedNormally returns true if the session expired in a normal
// terminal state. For TCP this means Close or TimeWait; for UDP and ICMP
// any expired session is considered normal (no explicit close handshake).
func sessionClosedNormally(key *sessionKey, sess *natSession) bool {
	switch key.Protocol {
	case unix.IPPROTO_UDP, unix.IPPROTO_ICMP:
		return true
	default:
		return sess.State == uint8(tcpCTClose) || sess.State == uint8(tcpCTTimeWait)
	}
}

func deleteSessions(egressSessions, ingressSessions *ebpf.Map,
	egressKey *sessionKey, sess *natSession,
) error {
	// delete ingress session first because we need egress session to
	// construct the ingress session key.
	ingressKey := sessionKey{
		SourceIP:   egressKey.TargetIP,
		TargetIP:   sess.NodeIP,
		SourcePort: egressKey.TargetPort,
		TargetPort: sess.NodePort,
		Version:    0,
		Protocol:   egressKey.Protocol,
	}
	err := ingressSessions.Delete(&ingressKey)
	if err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("failed to delete ingress session: %w", err)
	}

	err = egressSessions.Delete(egressKey)
	if err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("failed to delete egress session: %w", err)
	}

	return nil
}

func reapSessions() {
	m, err := loadPinnedMap(MapNameEgressSessions)
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to load egress session map",
		})
		return
	}
	defer m.Close()

	m2, err := loadPinnedMap(MapNameIngressSessions)
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to load ingress session map",
		})
		return
	}
	defer m2.Close()

	now, err := currentNS()
	if err != nil {
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to get current time",
		})
		return
	}

	var (
		key   sessionKey
		value natSession
		count int
	)
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		count++
		if sessionExpired(now, &key, &value) {
			err := deleteSessions(m, m2, &key, &value)
			if err != nil {
				enqueueEvent(Event{
					Error:   err,
					Message: "failed to delete sessions",
				})
			}

			if !sessionClosedNormally(&key, &value) {
				enqueueEvent(Event{
					Error:   ErrSessionExpiredNotClosed,
					Message: egressSession(&key, &value, now),
				})
			}
		}
	}

	err = iter.Err()
	if err != nil {
		// Known error:
		//   - ErrIterationAborted
		//
		// See below:
		//   - https://github.com/cilium/ebpf/issues/9
		//   - https://github.com/cilium/ebpf/pull/11
		enqueueEvent(Event{
			Error:   err,
			Message: "failed to iterate session maps",
		})
		return
	}

	reportCount(count)
}
