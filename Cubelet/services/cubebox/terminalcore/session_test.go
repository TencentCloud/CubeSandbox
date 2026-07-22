// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminalcore

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

type fakeStream struct {
	ctx    context.Context
	recv   chan *cubebox.TerminalClientMessage
	mu     sync.Mutex
	sent   []*cubebox.TerminalServerMessage
	sendCh chan *cubebox.TerminalServerMessage
}

func newFakeStream(ctx context.Context) *fakeStream {
	return &fakeStream{
		ctx:    ctx,
		recv:   make(chan *cubebox.TerminalClientMessage, 8),
		sendCh: make(chan *cubebox.TerminalServerMessage, 8),
	}
}

func (s *fakeStream) Context() context.Context { return s.ctx }

func (s *fakeStream) Recv() (*cubebox.TerminalClientMessage, error) {
	select {
	case frame := <-s.recv:
		return frame, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *fakeStream) Send(frame *cubebox.TerminalServerMessage) error {
	s.mu.Lock()
	s.sent = append(s.sent, frame)
	s.mu.Unlock()
	s.sendCh <- frame
	return nil
}

type fakeProcess struct {
	id           string
	output       io.Reader
	exit         chan ExitStatus
	stdin        bytes.Buffer
	started      int
	cleanupCalls int
	resizes      [][2]uint32
}

func newFakeProcess(output io.Reader) *fakeProcess {
	return &fakeProcess{id: "exec-1", output: output, exit: make(chan ExitStatus, 1)}
}

func (p *fakeProcess) ID() string                   { return p.id }
func (p *fakeProcess) Output() io.Reader            { return p.output }
func (p *fakeProcess) Wait() <-chan ExitStatus      { return p.exit }
func (p *fakeProcess) Start(context.Context) error  { p.started++; return nil }
func (p *fakeProcess) Cleanup() error               { p.cleanupCalls++; return nil }
func (p *fakeProcess) WriteStdin(data []byte) error { _, _ = p.stdin.Write(data); return nil }
func (p *fakeProcess) Resize(_ context.Context, cols, rows uint32) error {
	p.resizes = append(p.resizes, [2]uint32{cols, rows})
	return nil
}

type fakeFactory struct {
	process *fakeProcess
	calls   int
}

func (f *fakeFactory) Create(context.Context, *cubebox.TerminalOpenRequest) (Process, error) {
	f.calls++
	return f.process, nil
}

func openFrame() *cubebox.TerminalClientMessage {
	return &cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Open{
		Open: &cubebox.TerminalOpenRequest{
			RequestId:   "request-1",
			SessionId:   "session-1",
			SandboxId:   "sandbox-1",
			ContainerId: "container-1",
			Cols:        120,
			Rows:        30,
		},
	}}
}

func TestRunRequiresOpenAsFirstFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.recv <- &cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Stdin{Stdin: []byte("whoami\n")}}
	factory := &fakeFactory{process: newFakeProcess(bytes.NewReader(nil))}

	if err := Run(stream, factory); err == nil {
		t.Fatal("Run() accepted stdin before open")
	}
	if factory.calls != 0 {
		t.Fatalf("factory called %d times for invalid first frame", factory.calls)
	}
}

func TestRunForwardsInputResizeAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.recv <- openFrame()
	stream.recv <- &cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Stdin{Stdin: []byte("echo ok\n")}}
	stream.recv <- &cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Resize{Resize: &cubebox.TerminalResize{Cols: 90, Rows: 24}}}
	stream.recv <- &cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Close{Close: &cubebox.TerminalClose{}}}
	process := newFakeProcess(bytes.NewReader(nil))

	if err := Run(stream, &fakeFactory{process: process}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := process.stdin.String(); got != "echo ok\n" {
		t.Fatalf("stdin = %q", got)
	}
	wantResizes := [][2]uint32{{120, 30}, {90, 24}}
	if len(process.resizes) != len(wantResizes) || process.resizes[0] != wantResizes[0] || process.resizes[1] != wantResizes[1] {
		t.Fatalf("resizes = %#v, want %#v", process.resizes, wantResizes)
	}
	if process.started != 1 || process.cleanupCalls != 1 {
		t.Fatalf("started=%d cleanup=%d", process.started, process.cleanupCalls)
	}
	if len(stream.sent) == 0 || stream.sent[0].GetReady().GetExecId() != "exec-1" {
		t.Fatalf("first server frame is not ready: %#v", stream.sent)
	}
}

func TestRunDrainsOutputBeforeExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.recv <- openFrame()
	process := newFakeProcess(bytes.NewReader([]byte("\x1b[31mred\x1b[0m")))
	process.exit <- ExitStatus{Code: 7}

	if err := Run(stream, &fakeFactory{process: process}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(stream.sent) != 3 {
		t.Fatalf("sent %d frames, want ready/output/exit", len(stream.sent))
	}
	if got := string(stream.sent[1].GetOutput()); got != "\x1b[31mred\x1b[0m" {
		t.Fatalf("output = %q", got)
	}
	if got := stream.sent[2].GetExit().GetExitCode(); got != 7 {
		t.Fatalf("exit code = %d", got)
	}
}
