// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminalprotocol

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
)

type socketMessage struct {
	typeID int
	data   []byte
}

type fakeSocket struct {
	reads     chan socketMessage
	closed    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	writes    []socketMessage
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{reads: make(chan socketMessage, 8), closed: make(chan struct{})}
}

func (s *fakeSocket) ReadMessage() (int, []byte, error) {
	select {
	case message := <-s.reads:
		return message.typeID, message.data, nil
	case <-s.closed:
		return 0, nil, io.EOF
	}
}

func (s *fakeSocket) WriteMessage(typeID int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, socketMessage{typeID: typeID, data: append([]byte(nil), data...)})
	return nil
}

func (s *fakeSocket) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

type fakeBackend struct {
	recv      chan *cubebox.TerminalServerMessage
	closed    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	sent      []*cubebox.TerminalClientMessage
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{recv: make(chan *cubebox.TerminalServerMessage, 8), closed: make(chan struct{})}
}

func (b *fakeBackend) Send(frame *cubebox.TerminalClientMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, frame)
	return nil
}

func (b *fakeBackend) Recv() (*cubebox.TerminalServerMessage, error) {
	select {
	case frame := <-b.recv:
		return frame, nil
	case <-b.closed:
		return nil, io.EOF
	}
}

func (b *fakeBackend) CloseSend() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

type fakeBackendFactory struct {
	backend *fakeBackend
	calls   int
	open    ClientControl
}

func (f *fakeBackendFactory) Open(_ context.Context, open ClientControl) (Backend, error) {
	f.calls++
	f.open = open
	return f.backend, nil
}

func validOpenJSON() []byte {
	return []byte(`{"v":1,"type":"open","requestId":"request-1","sessionId":"session-1","sandboxId":"sandbox-1","containerId":"container-1","cols":120,"rows":30}`)
}

func TestRelayClosesSocketWhenOpenFrameIsRejected(t *testing.T) {
	socket := newFakeSocket()
	socket.reads <- socketMessage{typeID: BinaryMessage, data: []byte("not-an-open-control")}

	err := Relay(context.Background(), socket, &fakeBackendFactory{backend: newFakeBackend()})
	if err == nil {
		t.Fatal("Relay() accepted an invalid first frame")
	}
	select {
	case <-socket.closed:
	default:
		t.Fatal("Relay() did not close the upgraded socket after rejecting the first frame")
	}
}

func TestRelayForwardsOpenBinaryResizeAndClose(t *testing.T) {
	socket := newFakeSocket()
	socket.reads <- socketMessage{typeID: TextMessage, data: validOpenJSON()}
	socket.reads <- socketMessage{typeID: BinaryMessage, data: []byte("echo ok\n")}
	socket.reads <- socketMessage{typeID: TextMessage, data: []byte(`{"v":1,"type":"resize","cols":90,"rows":24}`)}
	socket.reads <- socketMessage{typeID: TextMessage, data: []byte(`{"v":1,"type":"close"}`)}
	backend := newFakeBackend()
	factory := &fakeBackendFactory{backend: backend}

	if err := Relay(context.Background(), socket, factory); err != nil {
		t.Fatalf("Relay() error = %v", err)
	}
	if factory.open.SandboxID != "sandbox-1" || factory.open.ContainerID != "container-1" {
		t.Fatalf("factory target = %q/%q", factory.open.SandboxID, factory.open.ContainerID)
	}
	if len(backend.sent) != 4 {
		t.Fatalf("backend received %d frames, want open/stdin/resize/close", len(backend.sent))
	}
	if got := string(backend.sent[1].GetStdin()); got != "echo ok\n" {
		t.Fatalf("stdin = %q", got)
	}
	if got := backend.sent[2].GetResize(); got.GetCols() != 90 || got.GetRows() != 24 {
		t.Fatalf("resize = %#v", got)
	}
	if backend.sent[3].GetClose() == nil {
		t.Fatal("last backend frame is not close")
	}
}

func TestRelayWritesReadyOutputAndExitInOrder(t *testing.T) {
	socket := newFakeSocket()
	socket.reads <- socketMessage{typeID: TextMessage, data: validOpenJSON()}
	backend := newFakeBackend()
	backend.recv <- &cubebox.TerminalServerMessage{Payload: &cubebox.TerminalServerMessage_Ready{Ready: &cubebox.TerminalReady{ExecId: "exec-1"}}}
	backend.recv <- &cubebox.TerminalServerMessage{Payload: &cubebox.TerminalServerMessage_Output{Output: []byte("\x1b[32mok\x1b[0m")}}
	backend.recv <- &cubebox.TerminalServerMessage{Payload: &cubebox.TerminalServerMessage_Exit{Exit: &cubebox.TerminalExit{ExitCode: 0, Reason: "process_exited"}}}

	if err := Relay(context.Background(), socket, &fakeBackendFactory{backend: backend}); err != nil {
		t.Fatalf("Relay() error = %v", err)
	}
	if len(socket.writes) != 3 {
		t.Fatalf("socket wrote %d frames, want ready/output/exit", len(socket.writes))
	}
	if socket.writes[0].typeID != TextMessage || socket.writes[1].typeID != BinaryMessage || socket.writes[2].typeID != TextMessage {
		t.Fatalf("message types = %#v", socket.writes)
	}
	if got := string(socket.writes[1].data); got != "\x1b[32mok\x1b[0m" {
		t.Fatalf("output = %q", got)
	}
}

func TestRelayRejectsBinaryFirstFrameWithoutOpeningBackend(t *testing.T) {
	socket := newFakeSocket()
	socket.reads <- socketMessage{typeID: BinaryMessage, data: []byte("whoami\n")}
	factory := &fakeBackendFactory{backend: newFakeBackend()}

	err := Relay(context.Background(), socket, factory)
	if err == nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("Relay() error = %v, want protocol error", err)
	}
	if factory.calls != 0 {
		t.Fatalf("factory called %d times", factory.calls)
	}
}

func TestRelayDoesNotExposeBackendErrorDetails(t *testing.T) {
	socket := newFakeSocket()
	socket.reads <- socketMessage{typeID: TextMessage, data: validOpenJSON()}
	backend := newFakeBackend()
	backend.recv <- &cubebox.TerminalServerMessage{Payload: &cubebox.TerminalServerMessage_Error{Error: &cubebox.TerminalError{
		Code:    cubebox.TerminalErrorCode_TERMINAL_ERROR_INTERNAL,
		Message: "dial 10.0.0.9:1234: connection refused",
	}}}

	if err := Relay(context.Background(), socket, &fakeBackendFactory{backend: backend}); err != nil {
		t.Fatalf("Relay() error = %v", err)
	}
	if len(socket.writes) != 1 {
		t.Fatalf("socket wrote %d frames, want one error", len(socket.writes))
	}
	if strings.Contains(string(socket.writes[0].data), "10.0.0.9") || !strings.Contains(string(socket.writes[0].data), "terminal session failed") {
		t.Fatalf("public error leaked backend details: %s", socket.writes[0].data)
	}
}
