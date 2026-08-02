// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	cubeboxapi "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

func TestTerminalRejectsNonOpenLeadingFrame(t *testing.T) {
	service := newTerminalTestService(t, terminalHandlerConfig())
	stream := newFakeTerminalStream(&cubeboxapi.TerminalClientFrame{
		Frame: &cubeboxapi.TerminalClientFrame_Resize{Resize: &cubeboxapi.TerminalResize{Cols: 80, Rows: 24}},
	})
	require.NoError(t, service.Terminal(stream))
	sent := stream.sentFrames()
	require.Len(t, sent, 1)
	require.Equal(t, terminalcore.CodeProtocolError, sent[0].GetError().GetCode())
}

func TestTerminalOpenedIsFirstAndClientFramesDrivePTY(t *testing.T) {
	config := terminalHandlerConfig()
	service := newTerminalTestService(t, config)
	openedEvents := make([]terminalOpenedLogEvent, 0, 1)
	oldOpenedLogger := terminalOpenedLogger
	terminalOpenedLogger = func(_ context.Context, event terminalOpenedLogEvent) {
		openedEvents = append(openedEvents, event)
	}
	t.Cleanup(func() { terminalOpenedLogger = oldOpenedLogger })
	sessionID := uuid.NewString()
	stream := newFakeTerminalStream(
		terminalOpenFrame(sessionID),
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Stdin{Stdin: []byte("echo ready\n")}},
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Resize{Resize: &cubeboxapi.TerminalResize{Cols: 100, Rows: 40}}},
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Close{Close: &cubeboxapi.TerminalClose{Reason: "untrusted-client-reason"}}},
	)
	stream.recvDelay = 5 * time.Millisecond
	require.NoError(t, service.Terminal(stream))

	sent := stream.sentFrames()
	require.GreaterOrEqual(t, len(sent), 3)
	require.Equal(t, sessionID, sent[0].GetOpened().GetSessionId(), "opened must be the first successful server frame")
	require.Equal(t, int32(137), sent[len(sent)-2].GetExit().GetExitCode())
	require.Equal(t, terminalcore.CloseUserClosed, sent[len(sent)-1].GetClose().GetReason())

	process := service.testAdapter.lastProcess()
	require.Contains(t, process.stdinString(), "echo ready\n")
	cols, rows := process.lastResize()
	require.Equal(t, uint32(100), cols)
	require.Equal(t, uint32(40), rows)
	require.Equal(t, []string{"close-stdin", "kill", "delete"}, process.operations())
	require.Len(t, openedEvents, 1, "stdin, resize, and close frames must not add success logs")
	require.Equal(t, sessionID, openedEvents[0].SessionID)
	require.False(t, openedEvents[0].Resume)
	require.Equal(t, "sandbox-a", openedEvents[0].SandboxID)
	require.Equal(t, "sandbox-a", openedEvents[0].ContainerID)
	require.Equal(t, "cubelet-term-"+sessionID[:12], openedEvents[0].ExecID)
}

func TestTerminalOversizedStdinClosesWithProtocolError(t *testing.T) {
	config := terminalHandlerConfig()
	config.MaxFrameBytes = 4
	service := newTerminalTestService(t, config)
	stream := newFakeTerminalStream(
		terminalOpenFrame(uuid.NewString()),
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Stdin{Stdin: []byte("12345")}},
	)
	require.NoError(t, service.Terminal(stream))
	sent := stream.sentFrames()
	require.Equal(t, terminalcore.CodeProtocolError, sent[1].GetError().GetCode())
	require.Equal(t, terminalcore.CloseProtocolError, sent[len(sent)-1].GetClose().GetReason())
}

func TestTerminalStdinBackpressureClosesWithSlowProducer(t *testing.T) {
	config := terminalHandlerConfig()
	config.StdinQueueFrames = 1
	service := newTerminalTestService(t, config)
	service.testAdapter.blockStdin = true
	stream := newFakeTerminalStream(
		terminalOpenFrame(uuid.NewString()),
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Stdin{Stdin: []byte("one")}},
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Stdin{Stdin: []byte("two")}},
		&cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Stdin{Stdin: []byte("three")}},
	)
	require.NoError(t, service.Terminal(stream))

	sent := stream.sentFrames()
	require.Equal(t, terminalcore.CodeSlowProducer, sent[1].GetError().GetCode())
	require.Equal(t, terminalcore.CloseSlowProducer, sent[len(sent)-1].GetClose().GetReason())
}

func TestTerminalLogsInitialAndResumeWithTrustedTargetAndStableExecID(t *testing.T) {
	service := newTerminalTestService(t, terminalHandlerConfig())
	openedEvents := make([]terminalOpenedLogEvent, 0, 2)
	oldOpenedLogger := terminalOpenedLogger
	terminalOpenedLogger = func(_ context.Context, event terminalOpenedLogEvent) {
		openedEvents = append(openedEvents, event)
	}
	t.Cleanup(func() { terminalOpenedLogger = oldOpenedLogger })

	sessionID := uuid.NewString()
	initial := newFakeTerminalStream(terminalOpenFrame(sessionID))
	require.NoError(t, service.Terminal(initial))
	resume := terminalOpenFrame(sessionID)
	resume.GetOpen().Resume = &cubeboxapi.TerminalResume{SessionId: sessionID, LastOffset: 0}
	require.NoError(t, service.Terminal(newFakeTerminalStream(resume)))

	require.Len(t, openedEvents, 2)
	require.False(t, openedEvents[0].Resume)
	require.True(t, openedEvents[1].Resume)
	require.Equal(t, openedEvents[0].ExecID, openedEvents[1].ExecID)
	for _, event := range openedEvents {
		require.Equal(t, sessionID, event.SessionID)
		require.Equal(t, "sandbox-a", event.SandboxID)
		require.Equal(t, "sandbox-a", event.ContainerID)
	}
}

func TestTerminalOpenFailureDoesNotLogSuccessOrPeerText(t *testing.T) {
	service := newTerminalTestService(t, terminalHandlerConfig())
	service.testAdapter.resolveErr = errors.New("Bearer peer-secret terminal payload")
	openedEvents := make([]terminalOpenedLogEvent, 0)
	rejectedEvents := make([]terminalOpenRejectedLogEvent, 0, 1)
	oldOpenedLogger := terminalOpenedLogger
	oldRejectedLogger := terminalOpenRejectedLogger
	terminalOpenedLogger = func(_ context.Context, event terminalOpenedLogEvent) {
		openedEvents = append(openedEvents, event)
	}
	terminalOpenRejectedLogger = func(_ context.Context, event terminalOpenRejectedLogEvent) {
		rejectedEvents = append(rejectedEvents, event)
	}
	t.Cleanup(func() {
		terminalOpenedLogger = oldOpenedLogger
		terminalOpenRejectedLogger = oldRejectedLogger
	})

	require.NoError(t, service.Terminal(newFakeTerminalStream(terminalOpenFrame(uuid.NewString()))))
	require.Empty(t, openedEvents)
	require.Len(t, rejectedEvents, 1)
	require.Equal(t, terminalcore.CodeInternal, rejectedEvents[0].ErrorKind)
	require.NotContains(t, rejectedEvents[0].ErrorKind, "peer-secret")
}

func terminalOpenFrame(sessionID string) *cubeboxapi.TerminalClientFrame {
	return &cubeboxapi.TerminalClientFrame{Frame: &cubeboxapi.TerminalClientFrame_Open{Open: &cubeboxapi.TerminalOpen{
		RequestId: uuid.NewString(),
		SandboxId: "sandbox-a",
		SessionId: sessionID,
		Cols:      80,
		Rows:      24,
	}}}
}

type terminalTestService struct {
	service
	testAdapter *terminalHandlerAdapter
}

func newTerminalTestService(t *testing.T, config terminalcore.Config) *terminalTestService {
	t.Helper()
	adapter := &terminalHandlerAdapter{}
	journal, err := terminalcore.NewFileJournal(t.TempDir())
	require.NoError(t, err)
	core, err := terminalcore.New(context.Background(), config, adapter, journal)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, core.Close(ctx, terminalcore.CloseServerDraining))
	})
	return &terminalTestService{service: service{terminal: core}, testAdapter: adapter}
}

func terminalHandlerConfig() terminalcore.Config {
	config := terminalcore.DefaultConfig()
	config.OpenTimeout = time.Second
	config.ResizeCoalesce = time.Millisecond
	config.ReconnectGrace = 20 * time.Millisecond
	config.IdleTimeout = time.Second
	config.MaxLifetime = time.Minute
	config.CleanupGrace = 2 * time.Millisecond
	config.CleanupTimeout = time.Second
	config.ReconcileInterval = time.Hour
	return config
}

type fakeTerminalStream struct {
	ctx context.Context

	mu        sync.Mutex
	received  []*cubeboxapi.TerminalClientFrame
	sent      []*cubeboxapi.TerminalServerFrame
	recvDelay time.Duration
}

func newFakeTerminalStream(frames ...*cubeboxapi.TerminalClientFrame) *fakeTerminalStream {
	return &fakeTerminalStream{ctx: context.Background(), received: frames}
}

func (s *fakeTerminalStream) Recv() (*cubeboxapi.TerminalClientFrame, error) {
	if s.recvDelay > 0 {
		time.Sleep(s.recvDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.received) == 0 {
		return nil, io.EOF
	}
	frame := s.received[0]
	s.received = s.received[1:]
	return frame, nil
}

func (s *fakeTerminalStream) Send(frame *cubeboxapi.TerminalServerFrame) error {
	s.mu.Lock()
	s.sent = append(s.sent, frame)
	s.mu.Unlock()
	return nil
}

func (s *fakeTerminalStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeTerminalStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeTerminalStream) SetTrailer(metadata.MD)       {}
func (s *fakeTerminalStream) Context() context.Context     { return s.ctx }
func (s *fakeTerminalStream) SendMsg(interface{}) error    { return nil }
func (s *fakeTerminalStream) RecvMsg(interface{}) error    { return nil }

func (s *fakeTerminalStream) sentFrames() []*cubeboxapi.TerminalServerFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*cubeboxapi.TerminalServerFrame(nil), s.sent...)
}

type terminalHandlerTarget struct{ metadata terminalcore.TargetMetadata }

func (t *terminalHandlerTarget) Metadata() terminalcore.TargetMetadata { return t.metadata }

type terminalHandlerAdapter struct {
	mu         sync.Mutex
	processes  []*terminalHandlerProcess
	resolveErr error
	blockStdin bool
}

func (a *terminalHandlerAdapter) Resolve(_ context.Context, sandboxID, containerID string) (terminalcore.Target, error) {
	if a.resolveErr != nil {
		return nil, a.resolveErr
	}
	if containerID == "" {
		containerID = sandboxID
	}
	return &terminalHandlerTarget{metadata: terminalcore.TargetMetadata{
		SandboxID:          sandboxID,
		ContainerID:        containerID,
		Namespace:          "default",
		RuntimeContainerID: containerID,
	}}, nil
}

func (a *terminalHandlerAdapter) StartPTY(context.Context, terminalcore.Target, terminalcore.PTYSpec) (terminalcore.PTYProcess, error) {
	process := newTerminalHandlerProcess()
	if a.blockStdin {
		process.stdinBlock = make(chan struct{})
	}
	a.mu.Lock()
	a.processes = append(a.processes, process)
	a.mu.Unlock()
	return process, nil
}

func (a *terminalHandlerAdapter) CleanupOrphan(context.Context, terminalcore.JournalRecord) error {
	return nil
}

func (a *terminalHandlerAdapter) lastProcess() *terminalHandlerProcess {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.processes[len(a.processes)-1]
}

type terminalHandlerProcess struct {
	mu         sync.Mutex
	stdin      bytes.Buffer
	stdoutR    *io.PipeReader
	stdoutW    *io.PipeWriter
	exitCh     chan terminalcore.ExitStatus
	exitOnce   sync.Once
	cols       uint32
	rows       uint32
	ops        []string
	stdinBlock chan struct{}
	stdinOnce  sync.Once
}

func newTerminalHandlerProcess() *terminalHandlerProcess {
	reader, writer := io.Pipe()
	return &terminalHandlerProcess{stdoutR: reader, stdoutW: writer, exitCh: make(chan terminalcore.ExitStatus, 1)}
}

func (p *terminalHandlerProcess) Stdin() io.WriteCloser {
	return terminalHandlerStdin{process: p}
}

func (p *terminalHandlerProcess) Stdout() io.Reader { return p.stdoutR }

func (p *terminalHandlerProcess) Resize(_ context.Context, cols, rows uint32) error {
	p.mu.Lock()
	p.cols, p.rows = cols, rows
	p.mu.Unlock()
	return nil
}

func (p *terminalHandlerProcess) CloseStdin(context.Context) error {
	p.stdinOnce.Do(func() {
		if p.stdinBlock != nil {
			close(p.stdinBlock)
		}
	})
	p.record("close-stdin")
	return nil
}

func (p *terminalHandlerProcess) Exited() <-chan terminalcore.ExitStatus { return p.exitCh }

func (p *terminalHandlerProcess) Kill(context.Context) error {
	p.record("kill")
	p.exitOnce.Do(func() {
		p.exitCh <- terminalcore.ExitStatus{Code: 137}
		close(p.exitCh)
	})
	return nil
}

func (p *terminalHandlerProcess) Delete(context.Context) error {
	p.record("delete")
	_ = p.stdoutW.Close()
	return nil
}

func (p *terminalHandlerProcess) stdinString() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdin.String()
}

func (p *terminalHandlerProcess) lastResize() (uint32, uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

func (p *terminalHandlerProcess) operations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ops...)
}

func (p *terminalHandlerProcess) record(operation string) {
	p.mu.Lock()
	p.ops = append(p.ops, operation)
	p.mu.Unlock()
}

type terminalHandlerStdin struct{ process *terminalHandlerProcess }

func (w terminalHandlerStdin) Write(data []byte) (int, error) {
	if w.process.stdinBlock != nil {
		<-w.process.stdinBlock
	}
	w.process.mu.Lock()
	defer w.process.mu.Unlock()
	return w.process.stdin.Write(data)
}

func (w terminalHandlerStdin) Close() error { return nil }
