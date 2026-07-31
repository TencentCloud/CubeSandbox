// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

type fakeTerminalProcess struct {
	pid        int
	output     chan []byte
	disconnect chan struct{}
	kill       chan struct{}
	once       sync.Once
}

func newFakeTerminalProcess(pid int) *fakeTerminalProcess {
	return &fakeTerminalProcess{
		pid:        pid,
		output:     make(chan []byte),
		disconnect: make(chan struct{}),
		kill:       make(chan struct{}),
	}
}

func (p *fakeTerminalProcess) PID() int                                { return p.pid }
func (p *fakeTerminalProcess) Output() <-chan []byte                   { return p.output }
func (p *fakeTerminalProcess) ExitCode() (int, bool)                   { return 0, false }
func (p *fakeTerminalProcess) ErrorMessage() string                    { return "" }
func (p *fakeTerminalProcess) SendStdin(context.Context, []byte) error { return nil }
func (p *fakeTerminalProcess) Resize(context.Context, cubesandbox.PtySize) error {
	return nil
}
func (p *fakeTerminalProcess) Disconnect() error {
	p.once.Do(func() { close(p.disconnect) })
	return nil
}
func (p *fakeTerminalProcess) Kill(context.Context) (bool, error) {
	select {
	case <-p.kill:
	default:
		close(p.kill)
	}
	return true, nil
}

type fakeTerminalBackend struct {
	mu         sync.Mutex
	nextPID    int
	started    []*fakeTerminalProcess
	reconnects int
}

func (b *fakeTerminalBackend) Start(context.Context, string, int, int, int) (terminalProcess, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextPID++
	process := newFakeTerminalProcess(b.nextPID)
	b.started = append(b.started, process)
	return process, nil
}

func (b *fakeTerminalBackend) Connect(context.Context, string, int, int) (terminalProcess, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconnects++
	process := newFakeTerminalProcess(b.started[0].pid)
	b.started = append(b.started, process)
	return process, nil
}

func (b *fakeTerminalBackend) Close() error { return nil }

func testTicket() TerminalTicket {
	return TerminalTicket{
		Username:    "alice",
		SandboxID:   "sandbox-1",
		ContainerID: "container-1",
		EnvdPort:    49983,
		Rows:        24,
		Cols:        80,
	}
}

func TestTerminalTicketIsSingleUseAndBoundToSandbox(t *testing.T) {
	service := newTerminalService(&fakeTerminalBackend{}, time.Minute, time.Second, 8, 4)
	ticket, err := service.IssueTicket(testTicket())
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if _, err := service.ConsumeTicket(ticket.Token, "other-sandbox"); err != ErrTerminalTicketInvalid {
		t.Fatalf("ConsumeTicket wrong sandbox error = %v", err)
	}
	if _, err := service.ConsumeTicket(ticket.Token, "sandbox-1"); err != ErrTerminalTicketInvalid {
		t.Fatalf("ticket was not consumed after failed binding check: %v", err)
	}
}

func TestTerminalTicketLimitAndProxySchemeValidation(t *testing.T) {
	service := newTerminalService(&fakeTerminalBackend{}, time.Minute, time.Second, 1, 1)
	for i := 0; i < 2; i++ {
		if _, err := service.IssueTicket(testTicket()); err != nil {
			t.Fatalf("IssueTicket %d: %v", i, err)
		}
	}
	if _, err := service.IssueTicket(testTicket()); err != ErrTerminalSessionLimit {
		t.Fatalf("ticket limit error = %v", err)
	}
	if _, err := NewTerminalService("ftp://proxy.example", "cube.test", time.Minute, time.Second, 1, 1); err == nil {
		t.Fatal("unsupported sandbox proxy scheme was accepted")
	}
}

func TestTerminalSessionEstablishDetachReconnectAndTerminate(t *testing.T) {
	backend := &fakeTerminalBackend{}
	service := newTerminalService(backend, time.Minute, time.Hour, 8, 4)
	ticket := testTicket()

	session, reconnected, err := service.Open(context.Background(), ticket)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reconnected || session.PID == 0 {
		t.Fatalf("unexpected initial session: reconnected=%v pid=%d", reconnected, session.PID)
	}

	service.Detach(session)
	reconnectTicket := ticket
	reconnectTicket.SessionID = session.ID
	issued, err := service.IssueTicket(reconnectTicket)
	if err != nil {
		t.Fatalf("IssueTicket reconnect: %v", err)
	}
	consumed, err := service.ConsumeTicket(issued.Token, ticket.SandboxID)
	if err != nil {
		t.Fatalf("ConsumeTicket reconnect: %v", err)
	}
	reconnectedSession, reconnected, err := service.Open(context.Background(), consumed)
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if !reconnected || reconnectedSession.ID != session.ID || backend.reconnects != 1 {
		t.Fatalf("reconnect did not preserve session: %#v", reconnectedSession)
	}

	service.Terminate(session.ID)
	select {
	case <-reconnectedSession.process.(*fakeTerminalProcess).kill:
	case <-time.After(time.Second):
		t.Fatal("active termination did not kill the PTY")
	}
}

func TestStaleAttachmentCannotDetachOrFinishReconnectedSession(t *testing.T) {
	backend := &fakeTerminalBackend{}
	service := newTerminalService(backend, time.Minute, time.Hour, 8, 4)
	ticket := testTicket()

	first, _, err := service.Open(context.Background(), ticket)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	service.Detach(first)

	reconnectTicket := ticket
	reconnectTicket.SessionID = first.ID
	second, reconnected, err := service.Open(context.Background(), reconnectTicket)
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if !reconnected {
		t.Fatal("expected a reconnected session")
	}
	secondProcess := second.process.(*fakeTerminalProcess)

	// A delayed close/output event from the first WebSocket must not affect the
	// process now owned by the reconnected attachment.
	service.Detach(first)
	service.Finish(first)
	select {
	case <-secondProcess.disconnect:
		t.Fatal("stale attachment disconnected the reconnected process")
	default:
	}

	service.Terminate(second.ID)
	select {
	case <-secondProcess.kill:
	case <-time.After(time.Second):
		t.Fatal("stale attachment removed the reconnected session")
	}
}

func TestTerminalSessionAuthorizationAndLimits(t *testing.T) {
	service := newTerminalService(&fakeTerminalBackend{}, time.Minute, time.Hour, 1, 1)
	session, _, err := service.Open(context.Background(), testTicket())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, _, err := service.Open(context.Background(), TerminalTicket{
		Username: "bob", SandboxID: "sandbox-2", ContainerID: "container-2", EnvdPort: 49983, Rows: 24, Cols: 80,
	}); err != ErrTerminalSessionLimit {
		t.Fatalf("limit error = %v", err)
	}

	service.Detach(session)
	unauthorized := testTicket()
	unauthorized.Username = "mallory"
	unauthorized.SessionID = session.ID
	if _, err := service.IssueTicket(unauthorized); err != ErrTerminalSessionGone {
		t.Fatalf("unauthorized reconnect error = %v", err)
	}
	service.Terminate(session.ID)
}

func TestTerminalAllowsIndependentSessionsInOneSandbox(t *testing.T) {
	backend := &fakeTerminalBackend{}
	service := newTerminalService(backend, time.Minute, time.Hour, 8, 4)
	first, _, err := service.Open(context.Background(), testTicket())
	if err != nil {
		t.Fatalf("open first session: %v", err)
	}
	secondTicket := testTicket()
	secondTicket.ContainerID = "container-2"
	secondTicket.EnvdPort = 49984
	second, _, err := service.Open(context.Background(), secondTicket)
	if err != nil {
		t.Fatalf("open second session: %v", err)
	}
	if first.ID == second.ID || first.PID == second.PID {
		t.Fatalf("sessions are not independent: first=%#v second=%#v", first, second)
	}
	service.Terminate(first.ID)
	service.Terminate(second.ID)
}

func TestTerminalReconnectGraceCleansDetachedSession(t *testing.T) {
	backend := &fakeTerminalBackend{}
	service := newTerminalService(backend, time.Minute, 10*time.Millisecond, 8, 4)
	session, _, err := service.Open(context.Background(), testTicket())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	service.Detach(session)

	select {
	case <-session.process.(*fakeTerminalProcess).kill:
	case <-time.After(time.Second):
		t.Fatal("detached session was not reaped after reconnect grace")
	}
	reconnect := testTicket()
	reconnect.SessionID = session.ID
	if _, err := service.IssueTicket(reconnect); err != ErrTerminalSessionGone {
		t.Fatalf("reaped session reconnect error = %v", err)
	}
}
