package cubevs

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH tcpstate ../src/tcp_state_test.bpf.c -- -I../vmlinux/$GOARCH

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const (
	tcpDirOriginal = 0
	tcpDirReply    = 1
	tcpTestCaseLen = 40
)

type tcpUpdateStep struct {
	dir uint8
	syn uint8
	ack uint8
	fin uint8
	rst uint8
}

type tcpUpdateCase struct {
	accessTime  uint64
	nowNS       uint64
	state       uint8
	activeClose uint8
	steps       []tcpUpdateStep
}

func encodeTCPUpdateCase(tc tcpUpdateCase) []byte {
	data := make([]byte, tcpTestCaseLen)
	binary.LittleEndian.PutUint64(data[0:8], tc.accessTime)
	binary.LittleEndian.PutUint64(data[8:16], tc.nowNS)
	data[16] = tc.state
	data[17] = tc.activeClose
	data[18] = uint8(len(tc.steps))
	for i, step := range tc.steps {
		if i >= 2 {
			break
		}
		off := 24 + i*8
		data[off] = step.dir
		data[off+1] = step.syn
		data[off+2] = step.ack
		data[off+3] = step.fin
		data[off+4] = step.rst
	}
	return data
}

func decodeTCPUpdateCase(data []byte) tcpUpdateCase {
	return tcpUpdateCase{
		accessTime:  binary.LittleEndian.Uint64(data[0:8]),
		nowNS:       binary.LittleEndian.Uint64(data[8:16]),
		state:       data[16],
		activeClose: data[17],
	}
}

func bpfTestUnavailable(err error) bool {
	var verifierErr *ebpf.VerifierError
	if errors.As(err, &verifierErr) {
		return false
	}
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) ||
		errors.Is(err, ebpf.ErrNotSupported)
}

func loadTCPStateTestProgram(t *testing.T) *ebpf.Program {
	t.Helper()

	spec, err := loadTcpstate()
	if err != nil {
		t.Fatalf("load tcp state test spec: %v", err)
	}
	for name := range spec.Maps {
		if name != ".rodata" {
			delete(spec.Maps, name)
		}
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF test unavailable: %v", err)
		}
		t.Fatalf("load tcp state test collection: %v", err)
	}
	t.Cleanup(coll.Close)

	prog := coll.Programs["test_update_session"]
	if prog == nil {
		t.Fatal("test_update_session program missing")
	}
	return prog
}

func runTCPUpdateCase(t *testing.T, prog *ebpf.Program, tc tcpUpdateCase) tcpUpdateCase {
	t.Helper()

	ret, out, err := prog.Test(encodeTCPUpdateCase(tc))
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF test-run unavailable: %v", err)
		}
		t.Fatalf("run tcp state test: %v", err)
	}
	if ret != 0 {
		t.Fatalf("test_update_session returned %d, want TC_ACT_OK", ret)
	}
	if len(out) < tcpTestCaseLen {
		t.Fatalf("test output length=%d, want >=%d", len(out), tcpTestCaseLen)
	}
	return decodeTCPUpdateCase(out)
}

// TestTCPUpdateSessionBidirectionalFINOrderings deterministically covers both
// serializations of near-simultaneous FINs. It does not prove atomicity when
// two CPUs update the same map value at exactly the same time.
func TestTCPUpdateSessionBidirectionalFINOrderings(t *testing.T) {
	prog := loadTCPStateTestProgram(t)
	fin := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1, fin: 1} }

	tests := []struct {
		name            string
		steps           []tcpUpdateStep
		wantState       tcpConntrackState
		wantActiveClose uint8
	}{
		{
			name:            "original FIN then reply FIN",
			steps:           []tcpUpdateStep{fin(tcpDirOriginal), fin(tcpDirReply)},
			wantState:       tcpCTLastAck,
			wantActiveClose: 1,
		},
		{
			name:            "reply FIN then original FIN",
			steps:           []tcpUpdateStep{fin(tcpDirReply), fin(tcpDirOriginal)},
			wantState:       tcpCTLastAck,
			wantActiveClose: 0,
		},
		{
			name:            "original FIN retransmission",
			steps:           []tcpUpdateStep{fin(tcpDirOriginal), fin(tcpDirOriginal)},
			wantState:       tcpCTFinWait,
			wantActiveClose: 1,
		},
		{
			name:            "reply FIN retransmission",
			steps:           []tcpUpdateStep{fin(tcpDirReply), fin(tcpDirReply)},
			wantState:       tcpCTFinWait,
			wantActiveClose: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runTCPUpdateCase(t, prog, tcpUpdateCase{
				accessTime: 1,
				nowNS:      uint64(2 * 1e9),
				state:      uint8(tcpCTEstablished),
				steps:      tt.steps,
			})
			if got.state != uint8(tt.wantState) {
				t.Fatalf("state=%s, want %s", tcpConntrackState(got.state), tt.wantState)
			}
			if got.activeClose != tt.wantActiveClose {
				t.Fatalf("active_close=%d, want %d", got.activeClose, tt.wantActiveClose)
			}
		})
	}
}

func TestTCPUpdateSessionFINRetransmissionAfterACK(t *testing.T) {
	prog := loadTCPStateTestProgram(t)
	fin := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1, fin: 1} }
	ack := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1} }

	tests := []struct {
		name            string
		first           []tcpUpdateStep
		retransmit      tcpUpdateStep
		wantActiveClose uint8
	}{
		{
			name:            "original FIN retransmission in close-wait",
			first:           []tcpUpdateStep{fin(tcpDirOriginal), ack(tcpDirReply)},
			retransmit:      fin(tcpDirOriginal),
			wantActiveClose: 1,
		},
		{
			name:            "reply FIN retransmission in close-wait",
			first:           []tcpUpdateStep{fin(tcpDirReply), ack(tcpDirOriginal)},
			retransmit:      fin(tcpDirReply),
			wantActiveClose: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closeWait := runTCPUpdateCase(t, prog, tcpUpdateCase{
				accessTime: 1,
				nowNS:      uint64(2 * 1e9),
				state:      uint8(tcpCTEstablished),
				steps:      tt.first,
			})
			if closeWait.state != uint8(tcpCTCloseWait) {
				t.Fatalf("precondition state=%s, want %s", tcpConntrackState(closeWait.state), tcpCTCloseWait)
			}
			got := runTCPUpdateCase(t, prog, tcpUpdateCase{
				accessTime:  closeWait.accessTime,
				nowNS:       uint64(3 * 1e9),
				state:       closeWait.state,
				activeClose: closeWait.activeClose,
				steps:       []tcpUpdateStep{tt.retransmit},
			})
			if got.state != uint8(tcpCTCloseWait) {
				t.Fatalf("state=%s, want %s", tcpConntrackState(got.state), tcpCTCloseWait)
			}
			if got.activeClose != tt.wantActiveClose {
				t.Fatalf("active_close=%d, want %d", got.activeClose, tt.wantActiveClose)
			}
		})
	}
}

func TestTCPUpdateSessionBidirectionalFINCompletionTimeout(t *testing.T) {
	prog := loadTCPStateTestProgram(t)
	fin := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1, fin: 1} }
	ack := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1} }

	tests := []struct {
		name            string
		first           []tcpUpdateStep
		lastACK         tcpUpdateStep
		wantActiveClose uint8
		wantTimeout     uint64
	}{
		{
			name:            "original initiated close",
			first:           []tcpUpdateStep{fin(tcpDirOriginal), fin(tcpDirReply)},
			lastACK:         ack(tcpDirOriginal),
			wantActiveClose: 1,
			wantTimeout:     uint64(tcpTimeouts[tcpCTClose].Nanoseconds()),
		},
		{
			name:            "reply initiated close",
			first:           []tcpUpdateStep{fin(tcpDirReply), fin(tcpDirOriginal)},
			lastACK:         ack(tcpDirReply),
			wantActiveClose: 0,
			wantTimeout:     uint64(tcpTimeouts[tcpCTTimeWait].Nanoseconds()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closing := runTCPUpdateCase(t, prog, tcpUpdateCase{
				accessTime: 1,
				nowNS:      uint64(2 * 1e9),
				state:      uint8(tcpCTEstablished),
				steps:      tt.first,
			})
			closed := runTCPUpdateCase(t, prog, tcpUpdateCase{
				accessTime:  closing.accessTime,
				nowNS:       uint64(3 * 1e9),
				state:       closing.state,
				activeClose: closing.activeClose,
				steps:       []tcpUpdateStep{tt.lastACK},
			})
			if closed.state != uint8(tcpCTTimeWait) {
				t.Fatalf("state=%s, want %s", tcpConntrackState(closed.state), tcpCTTimeWait)
			}
			if closed.activeClose != tt.wantActiveClose {
				t.Fatalf("active_close=%d, want %d", closed.activeClose, tt.wantActiveClose)
			}
			sess := natSession{State: closed.state, ActiveClose: closed.activeClose}
			if got := sess.tcpTimeout(); got != tt.wantTimeout {
				t.Fatalf("timeout=%d, want %d", got, tt.wantTimeout)
			}
		})
	}
}

func TestTCPUpdateSessionIgnoredTransitions(t *testing.T) {
	prog := loadTCPStateTestProgram(t)
	synack := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, syn: 1, ack: 1} }
	syn := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, syn: 1} }

	// A: Retransmitted SYN-ACK on ESTABLISHED or SYN_RECV should not poison state to IGNORED(11)
	curr := tcpUpdateCase{
		accessTime: 1,
		nowNS:      1e9,
		state:      uint8(tcpCTEstablished),
		steps:      []tcpUpdateStep{synack(tcpDirReply)},
	}
	curr = runTCPUpdateCase(t, prog, curr)
	if curr.state != uint8(tcpCTEstablished) {
		t.Fatalf("after retransmitted SYN-ACK on ESTABLISHED, state=%s, want established (must not become IGNORED)",
			tcpConntrackState(curr.state))
	}

	curr = tcpUpdateCase{
		accessTime: 1,
		nowNS:      1e9,
		state:      uint8(tcpCTSynRecv),
		steps:      []tcpUpdateStep{synack(tcpDirReply)},
	}
	curr = runTCPUpdateCase(t, prog, curr)
	if curr.state != uint8(tcpCTSynRecv) {
		t.Fatalf("after retransmitted SYN-ACK on SYN_RECV, state=%s, want syn_recv (must not become IGNORED)",
			tcpConntrackState(curr.state))
	}

	// B: Retransmitted SYN on ESTABLISHED from ORIGINAL should not poison state to IGNORED(11)
	curr = tcpUpdateCase{
		accessTime: 1,
		nowNS:      1e9,
		state:      uint8(tcpCTEstablished),
		steps:      []tcpUpdateStep{syn(tcpDirOriginal)},
	}
	curr = runTCPUpdateCase(t, prog, curr)
	if curr.state != uint8(tcpCTEstablished) {
		t.Fatalf("after retransmitted SYN on ESTABLISHED, state=%s, want established (must not become IGNORED)",
			tcpConntrackState(curr.state))
	}
	// C: Reopening a connection in TIME_WAIT resets active_close
	curr = tcpUpdateCase{
		accessTime:  1,
		nowNS:       1e9,
		state:       uint8(tcpCTTimeWait),
		activeClose: 1,
		steps:       []tcpUpdateStep{syn(tcpDirOriginal)},
	}
	curr = runTCPUpdateCase(t, prog, curr)
	if curr.state != uint8(tcpCTSynSent) {
		t.Fatalf("after reopen SYN on TIME_WAIT, state=%s, want syn_sent", tcpConntrackState(curr.state))
	}
	if curr.activeClose != 0 {
		t.Fatalf("after reopen SYN on TIME_WAIT, active_close=%d, want 0", curr.activeClose)
	}
}

func TestPortMappingTCPTimeout(t *testing.T) {
	// Independent Go unit tests exercising tcpTimeout() rules without requiring BPF environment.
	t.Run("port mapping client active close gets TIME_WAIT timeout", func(t *testing.T) {
		sess := natSession{
			State:       uint8(tcpCTTimeWait),
			ActiveClose: 1, // External client closed first
			PacketClass: packetClassPortMapping,
		}
		if got, want := sess.tcpTimeout(), uint64(tcpTimeouts[tcpCTTimeWait].Nanoseconds()); got != want {
			t.Fatalf("timeout=%v, want %v", time.Duration(got), time.Duration(want))
		}
	})

	t.Run("port mapping guest active close gets CLOSE timeout", func(t *testing.T) {
		sess := natSession{
			State:       uint8(tcpCTTimeWait),
			ActiveClose: 0, // Sandbox guest closed first
			PacketClass: packetClassPortMapping,
		}
		if got, want := sess.tcpTimeout(), uint64(tcpTimeouts[tcpCTClose].Nanoseconds()); got != want {
			t.Fatalf("timeout=%v, want %v", time.Duration(got), time.Duration(want))
		}
	})

	t.Run("snat guest active close gets CLOSE timeout", func(t *testing.T) {
		sess := natSession{
			State:       uint8(tcpCTTimeWait),
			ActiveClose: 1, // For SNAT, guest is ORIGINAL, so ActiveClose == 1 means guest closed
			PacketClass: packetClassSNAT,
		}
		if got, want := sess.tcpTimeout(), uint64(tcpTimeouts[tcpCTClose].Nanoseconds()); got != want {
			t.Fatalf("timeout=%v, want %v", time.Duration(got), time.Duration(want))
		}
	})

	t.Run("port mapping established gets 3 hours", func(t *testing.T) {
		sess := natSession{
			State:       uint8(tcpCTEstablished),
			PacketClass: packetClassPortMapping,
		}
		if got, want := sess.tcpTimeout(), uint64(tcpTimeouts[tcpCTEstablished].Nanoseconds()); got != want {
			t.Fatalf("timeout=%v, want %v", time.Duration(got), time.Duration(want))
		}
	})
}

func TestPortMappingTCPStateProgression(t *testing.T) {
	prog := loadTCPStateTestProgram(t)
	synack := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, syn: 1, ack: 1} }
	ack := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1} }
	fin := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, ack: 1, fin: 1} }
	rst := func(dir uint8) tcpUpdateStep { return tcpUpdateStep{dir: dir, rst: 1} }

	t.Run("port mapping handshake to established then client close", func(t *testing.T) {
		// 1. Simulate inbound SYN with session initialized to SYN_SENT state
		curr := tcpUpdateCase{
			accessTime: 1,
			nowNS:      1e9,
			state:      uint8(tcpCTSynSent),
		}

		// 2. VM replies with SYN-ACK (REPLY direction) -> SYN_RECV
		curr.steps = []tcpUpdateStep{synack(tcpDirReply)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTSynRecv) {
			t.Fatalf("after SYN-ACK state=%s, want %s", tcpConntrackState(curr.state), tcpCTSynRecv)
		}

		// 3. Client sends ACK (ORIGINAL direction) -> ESTABLISHED
		curr.steps = []tcpUpdateStep{ack(tcpDirOriginal)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTEstablished) {
			t.Fatalf("after ACK state=%s, want %s", tcpConntrackState(curr.state), tcpCTEstablished)
		}

		// Verify that ESTABLISHED session enjoys 3-hour timeout
		sessEst := natSession{State: curr.state}
		if sessEst.tcpTimeout() != uint64(tcpTimeouts[tcpCTEstablished].Nanoseconds()) {
			t.Fatalf("established timeout=%v, want %v",
				time.Duration(sessEst.tcpTimeout()), tcpTimeouts[tcpCTEstablished])
		}

		// 4. Data transfer in both directions keeps connection ESTABLISHED
		curr.steps = []tcpUpdateStep{ack(tcpDirOriginal), ack(tcpDirReply)}
		curr.nowNS += 1e9
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTEstablished) {
			t.Fatalf("after data state=%s, want %s", tcpConntrackState(curr.state), tcpCTEstablished)
		}

		// 5. Client active close: FIN -> ACK -> FIN -> ACK
		// Client sends FIN -> FIN_WAIT
		curr.steps = []tcpUpdateStep{fin(tcpDirOriginal)}
		curr.nowNS += 1e9
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTFinWait) {
			t.Fatalf("after client FIN state=%s, want %s", tcpConntrackState(curr.state), tcpCTFinWait)
		}
		if curr.activeClose != 1 {
			t.Fatalf("active_close=%d, want 1", curr.activeClose)
		}

		// VM sends ACK -> CLOSE_WAIT
		curr.steps = []tcpUpdateStep{ack(tcpDirReply)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTCloseWait) {
			t.Fatalf("after server ACK state=%s, want %s", tcpConntrackState(curr.state), tcpCTCloseWait)
		}

		// VM sends FIN -> LAST_ACK
		curr.steps = []tcpUpdateStep{fin(tcpDirReply)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTLastAck) {
			t.Fatalf("after server FIN state=%s, want %s", tcpConntrackState(curr.state), tcpCTLastAck)
		}

		// Client sends ACK -> TIME_WAIT
		curr.steps = []tcpUpdateStep{ack(tcpDirOriginal)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTTimeWait) {
			t.Fatalf("after client ACK state=%s, want %s", tcpConntrackState(curr.state), tcpCTTimeWait)
		}

		// 6. Verify reaper recognizes session closed normally
		dummyKey := sessionKey{Protocol: unix.IPPROTO_TCP}
		finalSess := natSession{State: curr.state, ActiveClose: curr.activeClose}
		finalSess.PacketClass = packetClassPortMapping
		if !sessionClosedNormally(&dummyKey, &finalSess) {
			t.Fatal("sessionClosedNormally returned false for TIME_WAIT session, expected true (no reaper false alarm)")
		}
		if got, want := finalSess.tcpTimeout(), uint64(tcpTimeouts[tcpCTTimeWait].Nanoseconds()); got != want {
			t.Fatalf("client-close timeout=%v, want %v", time.Duration(got), time.Duration(want))
		}
	})

	t.Run("port mapping handshake to established then server close", func(t *testing.T) {
		curr := tcpUpdateCase{
			accessTime: 1,
			nowNS:      1e9,
			state:      uint8(tcpCTSynSent),
		}
		// Handshake
		curr.steps = []tcpUpdateStep{synack(tcpDirReply), ack(tcpDirOriginal)}
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTEstablished) {
			t.Fatalf("handshake failed, state=%s, want %s", tcpConntrackState(curr.state), tcpCTEstablished)
		}

		// Server active close: VM sends FIN -> FIN_WAIT
		curr.steps = []tcpUpdateStep{fin(tcpDirReply)}
		curr.nowNS += 1e9
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTFinWait) {
			t.Fatalf("after server FIN state=%s, want %s", tcpConntrackState(curr.state), tcpCTFinWait)
		}
		if curr.activeClose != 0 {
			t.Fatalf("active_close=%d, want 0", curr.activeClose)
		}

		// Client sends ACK -> CLOSE_WAIT
		curr.steps = []tcpUpdateStep{ack(tcpDirOriginal)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTCloseWait) {
			t.Fatalf("after client ACK state=%s, want %s", tcpConntrackState(curr.state), tcpCTCloseWait)
		}

		// Client sends FIN -> LAST_ACK
		curr.steps = []tcpUpdateStep{fin(tcpDirOriginal)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTLastAck) {
			t.Fatalf("after client FIN state=%s, want %s", tcpConntrackState(curr.state), tcpCTLastAck)
		}

		// VM sends ACK -> TIME_WAIT
		curr.steps = []tcpUpdateStep{ack(tcpDirReply)}
		curr.nowNS += 1e7
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTTimeWait) {
			t.Fatalf("after server ACK state=%s, want %s", tcpConntrackState(curr.state), tcpCTTimeWait)
		}

		dummyKey := sessionKey{Protocol: unix.IPPROTO_TCP}
		finalSess := natSession{State: curr.state, ActiveClose: curr.activeClose}
		finalSess.PacketClass = packetClassPortMapping
		if !sessionClosedNormally(&dummyKey, &finalSess) {
			t.Fatal("sessionClosedNormally returned false for TIME_WAIT session, expected true")
		}
		if got, want := finalSess.tcpTimeout(), uint64(tcpTimeouts[tcpCTClose].Nanoseconds()); got != want {
			t.Fatalf("server-close timeout=%v, want %v", time.Duration(got), time.Duration(want))
		}
	})

	t.Run("port mapping reset transitions to CLOSE", func(t *testing.T) {
		// Test RST from client
		curr := tcpUpdateCase{
			accessTime: 1,
			nowNS:      1e9,
			state:      uint8(tcpCTEstablished),
			steps:      []tcpUpdateStep{rst(tcpDirOriginal)},
		}
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTClose) {
			t.Fatalf("after client RST state=%s, want %s", tcpConntrackState(curr.state), tcpCTClose)
		}
		dummyKey := sessionKey{Protocol: unix.IPPROTO_TCP}
		finalSess := natSession{State: curr.state}
		if !sessionClosedNormally(&dummyKey, &finalSess) {
			t.Fatal("sessionClosedNormally returned false for CLOSE session, expected true")
		}

		// Test RST from VM
		curr = tcpUpdateCase{
			accessTime: 1,
			nowNS:      1e9,
			state:      uint8(tcpCTEstablished),
			steps:      []tcpUpdateStep{rst(tcpDirReply)},
		}
		curr = runTCPUpdateCase(t, prog, curr)
		if curr.state != uint8(tcpCTClose) {
			t.Fatalf("after VM RST state=%s, want %s", tcpConntrackState(curr.state), tcpCTClose)
		}
		finalSess = natSession{State: curr.state}
		if !sessionClosedNormally(&dummyKey, &finalSess) {
			t.Fatal("sessionClosedNormally returned false for CLOSE session, expected true")
		}
	})
}
