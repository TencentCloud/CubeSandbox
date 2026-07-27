// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

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
)

func TestSessionPTYLifecycleAndBoundedIO(t *testing.T) {
	adapter := newFakeAdapter()
	core := newTestCore(t, adapter, testConfig())
	attachment, opened, err := core.Open(context.Background(), openRequest("sandbox-a"))
	require.NoError(t, err)
	require.Equal(t, attachment.Opened(), opened)
	require.Equal(t, opened.SessionID, adapter.lastSpec().SessionID)
	require.Equal(t, execIDForSession(opened.SessionID), adapter.lastSpec().ExecID)

	require.NoError(t, attachment.SendStdin([]byte("whoami\n")))
	process := adapter.lastProcess()
	require.Eventually(t, func() bool {
		return bytes.Contains(process.stdinBytes(), []byte("whoami\n"))
	}, time.Second, time.Millisecond)
	require.Equal(t, CodeProtocolError, CodeOf(attachment.SendStdin(bytes.Repeat([]byte{'x'}, core.MaxFrameBytes()+1))))

	require.NoError(t, attachment.Resize(90, 30))
	require.NoError(t, attachment.Resize(120, 40))
	require.Eventually(t, func() bool {
		cols, rows := process.lastResize()
		return cols == 120 && rows == 40
	}, time.Second, time.Millisecond)

	process.writeStdout(t, "hello")
	event := nextEvent(t, attachment)
	require.Equal(t, EventStdout, event.Kind)
	require.Equal(t, uint64(0), event.Offset)
	require.Equal(t, []byte("hello"), event.Data)

	process.exit(0)
	require.Equal(t, EventExit, nextEvent(t, attachment).Kind)
	closeEvent := nextEvent(t, attachment)
	require.Equal(t, EventClose, closeEvent.Kind)
	require.Equal(t, CloseRuntimeExited, closeEvent.Reason)
	require.Eventually(t, func() bool {
		_, exists := core.SessionState(opened.SessionID)
		return !exists
	}, time.Second, time.Millisecond)
	require.Equal(t, []string{"close-stdin", "delete"}, process.operations())
}

func TestCleanupEscalatesAndIsIdempotent(t *testing.T) {
	adapter := newFakeAdapter()
	config := testConfig()
	config.CleanupGrace = 5 * time.Millisecond
	core := newTestCore(t, adapter, config)
	attachment, _, err := core.Open(context.Background(), openRequest("sandbox-a"))
	require.NoError(t, err)

	require.NoError(t, attachment.Close(CloseUserClosed))
	require.Equal(t, CodeSessionLost, CodeOf(attachment.Close(CloseUserClosed)))
	process := adapter.lastProcess()
	require.Equal(t, EventExit, nextEvent(t, attachment).Kind)
	require.Equal(t, EventClose, nextEvent(t, attachment).Kind)
	require.Eventually(t, func() bool {
		return process.operationsEqual([]string{"close-stdin", "kill", "delete"})
	}, time.Second, time.Millisecond)
}

func TestDetachedResumeReplayAndGenerationFence(t *testing.T) {
	adapter := newFakeAdapter()
	config := testConfig()
	config.ReplayBufferBytes = 16
	config.StdoutPendingBytes = 32
	config.StdoutChunkBytes = 4
	core := newTestCore(t, adapter, config)
	request := openRequest("sandbox-a")
	attachment, _, err := core.Open(context.Background(), request)
	require.NoError(t, err)

	process := adapter.lastProcess()
	process.writeStdout(t, "abcdefghijklmnopqrst")
	var live bytes.Buffer
	for live.Len() < 20 {
		event := nextEvent(t, attachment)
		require.Equal(t, EventStdout, event.Kind)
		live.Write(event.Data)
	}
	require.Equal(t, "abcdefghijklmnopqrst", live.String())
	attachment.Detach()
	require.Equal(t, CodeSessionLost, CodeOf(attachment.SendStdin([]byte("stale"))))

	resumeRequest := request
	resumeRequest.Resume = &ResumeRequest{SessionID: request.SessionID, LastOffset: 0}
	resumed, opened, err := core.Open(context.Background(), resumeRequest)
	require.NoError(t, err)
	require.True(t, opened.ReplayTruncated)
	require.Equal(t, uint64(4), opened.ReplayFrom)
	var replay bytes.Buffer
	for replay.Len() < 16 {
		event := nextEvent(t, resumed)
		require.Equal(t, EventStdout, event.Kind)
		replay.Write(event.Data)
	}
	require.Equal(t, "efghijklmnopqrst", replay.String())

	resumed.Detach()
	results := make(chan *Attachment, 2)
	errorsCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate, _, openErr := core.Open(context.Background(), resumeRequest)
			if openErr != nil {
				errorsCh <- openErr
				return
			}
			results <- candidate
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	require.Len(t, results, 1, "only one resume generation may win")
	require.Len(t, errorsCh, 1)
	for resumeErr := range errorsCh {
		require.Equal(t, CodeSessionLost, CodeOf(resumeErr))
	}
	for winner := range results {
		require.NoError(t, winner.Close(CloseUserClosed))
		for {
			event, nextErr := nextEventWithTimeout(winner, time.Second)
			require.NoError(t, nextErr)
			if event.Kind == EventClose {
				break
			}
		}
	}
}

func TestDetachedReplayOverflowClosesSession(t *testing.T) {
	adapter := newFakeAdapter()
	config := testConfig()
	config.ReplayBufferBytes = 8
	config.StdoutPendingBytes = 8
	config.StdoutChunkBytes = 4
	core := newTestCore(t, adapter, config)
	request := openRequest("sandbox-a")
	attachment, _, err := core.Open(context.Background(), request)
	require.NoError(t, err)
	attachment.Detach()
	adapter.lastProcess().writeStdout(t, "abcdefghijkl")
	require.Eventually(t, func() bool {
		_, exists := core.SessionState(request.SessionID)
		return !exists
	}, time.Second, time.Millisecond)
}

func TestLimitsAndDrainFence(t *testing.T) {
	adapter := newFakeAdapter()
	config := testConfig()
	config.MaxSessionsSandbox = 1
	core := newTestCore(t, adapter, config)
	firstRequest := openRequest("sandbox-a")
	_, _, err := core.Open(context.Background(), firstRequest)
	require.NoError(t, err)
	secondRequest := openRequest("sandbox-a")
	_, _, err = core.Open(context.Background(), secondRequest)
	require.Equal(t, CodeLimitExceeded, CodeOf(err))

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, core.DrainSandbox(drainCtx, "sandbox-a", CloseSandboxTransition))
	cancel()
	_, _, err = core.Open(context.Background(), openRequest("sandbox-a"))
	require.Equal(t, CodeSandboxTransition, CodeOf(err))

	core.AllowSandbox("sandbox-a")
	attachment, _, err := core.Open(context.Background(), openRequest("sandbox-a"))
	require.NoError(t, err)
	require.NoError(t, attachment.Close(CloseUserClosed))
	for {
		if nextEvent(t, attachment).Kind == EventClose {
			break
		}
	}
}

func TestDrainDeliversOutputProducedDuringCleanupGrace(t *testing.T) {
	adapter := newFakeAdapter()
	config := testConfig()
	config.CleanupGrace = 100 * time.Millisecond
	config.CleanupTimeout = time.Second
	core := newTestCore(t, adapter, config)
	request := openRequest("sandbox-a")
	attachment, _, err := core.Open(context.Background(), request)
	require.NoError(t, err)
	process := adapter.lastProcess()

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = process.stdoutW.Write([]byte("drained tail"))
		process.exit(0)
	}()

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, core.DrainSandbox(drainCtx, "sandbox-a", CloseSandboxTransition))
	cancel()

	var output bytes.Buffer
	for {
		event := nextEvent(t, attachment)
		if event.Kind == EventStdout {
			_, _ = output.Write(event.Data)
		}
		if event.Kind == EventClose {
			break
		}
	}
	require.Contains(t, output.String(), "drained tail")
}

func TestResizeDoesNotExtendIdleButStdinDoes(t *testing.T) {
	adapter := newFakeAdapter()
	config := testConfig()
	config.IdleTimeout = 80 * time.Millisecond
	core := newTestCore(t, adapter, config)
	attachment, _, err := core.Open(context.Background(), openRequest("sandbox-a"))
	require.NoError(t, err)
	time.Sleep(45 * time.Millisecond)
	require.NoError(t, attachment.Resize(100, 30))
	closeEvent := waitForKind(t, attachment, EventClose)
	require.Equal(t, CloseIdleTimeout, closeEvent.Reason)

	second, _, err := core.Open(context.Background(), openRequest("sandbox-a"))
	require.NoError(t, err)
	time.Sleep(45 * time.Millisecond)
	require.NoError(t, second.SendStdin([]byte("x")))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	_, err = second.Next(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "stdin must extend the idle deadline")
	require.Equal(t, CloseIdleTimeout, waitForKind(t, second, EventClose).Reason)
}

func TestParentCancellationDrainsSessionsAndRejectsNewOpens(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	adapter := newFakeAdapter()
	core, err := New(parent, testConfig(), adapter, newMemoryJournal())
	require.NoError(t, err)
	require.NoError(t, core.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, core.Close(ctx, CloseServerDraining))
	})

	attachment, opened, err := core.Open(context.Background(), openRequest("sandbox-a"))
	require.NoError(t, err)
	cancelParent()
	closeEvent := waitForKind(t, attachment, EventClose)
	require.Equal(t, CloseServerDraining, closeEvent.Reason)
	require.Eventually(t, func() bool {
		_, exists := core.SessionState(opened.SessionID)
		return !exists
	}, time.Second, time.Millisecond)

	_, _, err = core.Open(context.Background(), openRequest("sandbox-a"))
	require.Equal(t, CodeServerDraining, CodeOf(err))
}

func TestRecoverOrphansRemovesOnlySuccessfulRecords(t *testing.T) {
	adapter := newFakeAdapter()
	journal := newMemoryJournal()
	config := testConfig()
	core, err := New(context.Background(), config, adapter, journal)
	require.NoError(t, err)

	success := journalRecord("sandbox-a")
	failure := journalRecord("sandbox-b")
	journal.records[success.SessionID] = success
	journal.records[failure.SessionID] = failure
	adapter.cleanupErrors[failure.SessionID] = errors.New("shim unavailable")

	err = core.RecoverOrphans(context.Background())
	require.Error(t, err)
	require.NotContains(t, journal.snapshot(), success.SessionID)
	require.Contains(t, journal.snapshot(), failure.SessionID)
	require.ElementsMatch(t, []string{success.SessionID, failure.SessionID}, adapter.cleanedSessions())
}

func TestRecoverOrphansSerializesConcurrentReconciliation(t *testing.T) {
	journal := newMemoryJournal()
	record := journalRecord("sandbox-a")
	journal.records[record.SessionID] = record
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	adapter := &blockingRecoveryAdapter{
		fakeAdapter: newFakeAdapter(),
		entered:     entered,
		release:     release,
	}
	core, err := New(context.Background(), testConfig(), adapter, journal)
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() { firstDone <- core.RecoverOrphans(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first orphan recovery did not reach the runtime adapter")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- core.RecoverOrphans(context.Background())
	}()
	<-secondStarted
	select {
	case <-entered:
		t.Fatal("concurrent orphan recovery reached the runtime adapter")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	require.Equal(t, []string{record.SessionID}, adapter.cleanedSessions())
	require.NotContains(t, journal.snapshot(), record.SessionID)
}

func TestAttachmentFinishDrainsBoundedQueueBeforeEOF(t *testing.T) {
	config := testConfig()
	config.StdoutChunkBytes = 4
	config.StdoutPendingBytes = 8
	attachment := newAttachment(nil, 1, Opened{}, config)

	require.True(t, attachment.enqueue(Event{Kind: EventStdout, Data: []byte("abcd")}))
	require.True(t, attachment.enqueue(Event{Kind: EventStdout, Data: []byte("efgh")}))
	require.True(t, attachment.enqueue(Event{Kind: EventError, Code: "first"}))
	require.True(t, attachment.enqueue(Event{Kind: EventError, Code: "second"}))
	attachment.finish(
		Event{Kind: EventError, Code: CodeInternal},
		Event{Kind: EventExit, ExitCode: 137},
		Event{Kind: EventClose, Reason: CloseInternal},
	)

	var events []Event
	for {
		event, nextErr := nextEventWithTimeout(attachment, time.Second)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		require.NoError(t, nextErr)
		events = append(events, event)
	}
	require.Len(t, events, 7)
	require.Equal(t, EventClose, events[len(events)-1].Kind)
}

func TestAttachmentFinishDoesNotBlockWhenEventChannelIsSaturated(t *testing.T) {
	config := testConfig()
	config.StdoutChunkBytes = 4
	config.StdoutPendingBytes = 8
	attachment := newAttachment(nil, 1, Opened{}, config)

	// Saturate the internal transport queue independently of its accounting.
	// finish must remain bounded even if a future producer violates the normal
	// maxFrames + control-frame reservation invariant.
	for i := 0; i < cap(attachment.events); i++ {
		attachment.events <- Event{Kind: EventError, Code: "queued"}
	}
	finished := make(chan struct{})
	go func() {
		attachment.finish(Event{Kind: EventClose, Reason: CloseInternal})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("attachment finish blocked on a saturated event channel")
	}

	var events []Event
	for {
		event, nextErr := nextEventWithTimeout(attachment, time.Second)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		require.NoError(t, nextErr)
		events = append(events, event)
	}
	require.Len(t, events, cap(attachment.events)+1)
	require.Equal(t, EventClose, events[len(events)-1].Kind)
}

func TestResumeReplayFailureDoesNotCommitGeneration(t *testing.T) {
	config := testConfig()
	core := &Core{config: config, ctx: context.Background()}
	request := openRequest("sandbox-a")
	target := &fakeTarget{metadata: TargetMetadata{
		SandboxID:          request.SandboxID,
		ContainerID:        request.SandboxID + "-primary",
		Namespace:          "default",
		RuntimeContainerID: request.SandboxID + "-primary",
	}}
	s := newSession(core, request, target)
	t.Cleanup(s.cancel)
	s.state = StateDetachedGrace
	s.generation = 3
	s.graceEpoch = 9
	s.ring.Write([]byte("abcdefgh"))

	// Valid production configs guarantee replay fits. Tighten the attachment
	// bound after constructing the ring to exercise the defensive failure path.
	core.config.StdoutChunkBytes = 4
	core.config.StdoutPendingBytes = 4
	resume := request
	resume.Resume = &ResumeRequest{SessionID: request.SessionID, LastOffset: 0}
	attachment, _, err := s.resume(resume)
	require.Nil(t, attachment)
	require.Equal(t, CodeSlowConsumer, CodeOf(err))
	require.Equal(t, StateDetachedGrace, s.state)
	require.Equal(t, uint64(3), s.generation)
	require.Equal(t, uint64(9), s.graceEpoch)
	require.Nil(t, s.attachment)
}

func newTestCore(t *testing.T, adapter *fakeAdapter, config Config) *Core {
	t.Helper()
	core, err := New(context.Background(), config, adapter, newMemoryJournal())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, core.Close(ctx, CloseServerDraining))
	})
	return core
}

func testConfig() Config {
	config := DefaultConfig()
	config.MaxFrameBytes = 32
	config.StdoutChunkBytes = 8
	config.ReplayBufferBytes = 64
	config.StdoutPendingBytes = 64
	config.OpenTimeout = time.Second
	config.ResizeCoalesce = time.Millisecond
	config.ReconnectGrace = 100 * time.Millisecond
	config.IdleTimeout = time.Second
	config.MaxLifetime = 5 * time.Second
	config.CleanupGrace = 10 * time.Millisecond
	config.CleanupTimeout = 500 * time.Millisecond
	config.ReconcileInterval = time.Hour
	return config
}

func openRequest(sandboxID string) OpenRequest {
	return OpenRequest{
		RequestID: uuid.NewString(),
		SandboxID: sandboxID,
		SessionID: uuid.NewString(),
		Cols:      80,
		Rows:      24,
	}
}

func nextEvent(t *testing.T, attachment *Attachment) Event {
	t.Helper()
	event, err := nextEventWithTimeout(attachment, time.Second)
	require.NoError(t, err)
	return event
}

func nextEventWithTimeout(attachment *Attachment, timeout time.Duration) (Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return attachment.Next(ctx)
}

func waitForKind(t *testing.T, attachment *Attachment, kind EventKind) Event {
	t.Helper()
	for {
		event := nextEvent(t, attachment)
		if event.Kind == kind {
			return event
		}
	}
}

type fakeTarget struct{ metadata TargetMetadata }

func (t *fakeTarget) Metadata() TargetMetadata { return t.metadata }

type fakeAdapter struct {
	mu            sync.Mutex
	processes     []*fakeProcess
	specs         []PTYSpec
	cleaned       []string
	cleanupErrors map[string]error
}

type blockingRecoveryAdapter struct {
	*fakeAdapter
	entered chan<- struct{}
	release <-chan struct{}
}

func (a *blockingRecoveryAdapter) CleanupOrphan(ctx context.Context, record JournalRecord) error {
	select {
	case a.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-a.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.fakeAdapter.CleanupOrphan(ctx, record)
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{cleanupErrors: make(map[string]error)}
}

func (a *fakeAdapter) Resolve(_ context.Context, sandboxID, containerID string) (Target, error) {
	if containerID == "" {
		containerID = sandboxID + "-primary"
	}
	return &fakeTarget{metadata: TargetMetadata{
		SandboxID:          sandboxID,
		ContainerID:        containerID,
		Namespace:          "default",
		RuntimeContainerID: containerID,
	}}, nil
}

func (a *fakeAdapter) StartPTY(_ context.Context, _ Target, spec PTYSpec) (PTYProcess, error) {
	process := newFakeProcess()
	a.mu.Lock()
	a.processes = append(a.processes, process)
	a.specs = append(a.specs, spec)
	a.mu.Unlock()
	return process, nil
}

func (a *fakeAdapter) CleanupOrphan(_ context.Context, record JournalRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleaned = append(a.cleaned, record.SessionID)
	return a.cleanupErrors[record.SessionID]
}

func (a *fakeAdapter) lastProcess() *fakeProcess {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.processes[len(a.processes)-1]
}

func (a *fakeAdapter) lastSpec() PTYSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.specs[len(a.specs)-1]
}

func (a *fakeAdapter) cleanedSessions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.cleaned...)
}

type fakeProcess struct {
	mu       sync.Mutex
	stdin    bytes.Buffer
	stdoutR  *io.PipeReader
	stdoutW  *io.PipeWriter
	exitCh   chan ExitStatus
	exitOnce sync.Once
	ops      []string
	cols     uint32
	rows     uint32
}

func newFakeProcess() *fakeProcess {
	reader, writer := io.Pipe()
	return &fakeProcess{stdoutR: reader, stdoutW: writer, exitCh: make(chan ExitStatus, 1)}
}

func (p *fakeProcess) Stdin() io.WriteCloser { return fakeStdin{process: p} }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdoutR }

func (p *fakeProcess) Resize(_ context.Context, cols, rows uint32) error {
	p.mu.Lock()
	p.cols, p.rows = cols, rows
	p.mu.Unlock()
	return nil
}

func (p *fakeProcess) CloseStdin(context.Context) error {
	p.record("close-stdin")
	return nil
}

func (p *fakeProcess) Exited() <-chan ExitStatus { return p.exitCh }

func (p *fakeProcess) Kill(context.Context) error {
	p.record("kill")
	p.exit(137)
	return nil
}

func (p *fakeProcess) Delete(context.Context) error {
	p.record("delete")
	_ = p.stdoutW.Close()
	return nil
}

func (p *fakeProcess) writeStdout(t *testing.T, data string) {
	t.Helper()
	_, err := p.stdoutW.Write([]byte(data))
	require.NoError(t, err)
}

func (p *fakeProcess) exit(code int32) {
	p.exitOnce.Do(func() {
		p.exitCh <- ExitStatus{Code: code}
		close(p.exitCh)
	})
}

func (p *fakeProcess) record(operation string) {
	p.mu.Lock()
	p.ops = append(p.ops, operation)
	p.mu.Unlock()
}

func (p *fakeProcess) stdinBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.stdin.Bytes()...)
}

func (p *fakeProcess) lastResize() (uint32, uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

func (p *fakeProcess) operations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ops...)
}

func (p *fakeProcess) operationsEqual(expected []string) bool {
	return bytes.Equal([]byte(stringsJoin(p.operations())), []byte(stringsJoin(expected)))
}

type fakeStdin struct{ process *fakeProcess }

func (w fakeStdin) Write(data []byte) (int, error) {
	w.process.mu.Lock()
	defer w.process.mu.Unlock()
	return w.process.stdin.Write(data)
}

func (w fakeStdin) Close() error { return nil }

func stringsJoin(items []string) string {
	var result bytes.Buffer
	for _, item := range items {
		result.WriteString(item)
		result.WriteByte(0)
	}
	return result.String()
}

type memoryJournal struct {
	mu      sync.Mutex
	records map[string]JournalRecord
}

func newMemoryJournal() *memoryJournal {
	return &memoryJournal{records: make(map[string]JournalRecord)}
}

func (j *memoryJournal) Put(record JournalRecord) error {
	j.mu.Lock()
	j.records[record.SessionID] = record
	j.mu.Unlock()
	return nil
}

func (j *memoryJournal) Remove(sessionID string) error {
	j.mu.Lock()
	delete(j.records, sessionID)
	j.mu.Unlock()
	return nil
}

func (j *memoryJournal) List() ([]JournalRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	records := make([]JournalRecord, 0, len(j.records))
	for _, record := range j.records {
		records = append(records, record)
	}
	return records, nil
}

func (j *memoryJournal) snapshot() map[string]JournalRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	records := make(map[string]JournalRecord, len(j.records))
	for key, record := range j.records {
		records[key] = record
	}
	return records
}

func journalRecord(sandboxID string) JournalRecord {
	sessionID := uuid.NewString()
	return JournalRecord{
		SessionID: sessionID,
		ExecID:    execIDForSession(sessionID),
		Target: TargetMetadata{
			SandboxID:          sandboxID,
			ContainerID:        sandboxID + "-primary",
			Namespace:          "default",
			RuntimeContainerID: sandboxID + "-primary",
		},
		OpenedAt: time.Now().UTC(),
	}
}
