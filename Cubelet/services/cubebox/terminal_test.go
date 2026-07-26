// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubTerminalProcess struct {
	mu          sync.Mutex
	waitCalls   int
	killCalls   int
	deleteCalls int
	resizeCalls [][2]uint32
	exitStatus  chan containerd.ExitStatus
}

func newStubTerminalProcess() *stubTerminalProcess {
	return &stubTerminalProcess{exitStatus: make(chan containerd.ExitStatus, 1)}
}

func (p *stubTerminalProcess) ID() string                  { return "terminal-stub" }
func (p *stubTerminalProcess) Pid() uint32                 { return 0 }
func (p *stubTerminalProcess) Start(context.Context) error { return nil }
func (p *stubTerminalProcess) CloseIO(context.Context, ...containerd.IOCloserOpts) error {
	return nil
}
func (p *stubTerminalProcess) IO() cio.IO { return nil }
func (p *stubTerminalProcess) Status(context.Context) (containerd.Status, error) {
	return containerd.Status{}, nil
}

func (p *stubTerminalProcess) Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteCalls++
	return containerd.NewExitStatus(137, time.Now(), nil), nil
}

func (p *stubTerminalProcess) Kill(context.Context, syscall.Signal, ...containerd.KillOpts) error {
	p.mu.Lock()
	p.killCalls++
	p.mu.Unlock()
	p.exitStatus <- *containerd.NewExitStatus(137, time.Now(), nil)
	return nil
}

func (p *stubTerminalProcess) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waitCalls++
	return p.exitStatus, nil
}

func (p *stubTerminalProcess) Resize(_ context.Context, width, height uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizeCalls = append(p.resizeCalls, [2]uint32{width, height})
	return nil
}

func TestCleanupTerminalProcessKillsWaitsAndDeletesActiveProcess(t *testing.T) {
	process := newStubTerminalProcess()

	cleanupTerminalProcess(context.Background(), process, true, false)

	process.mu.Lock()
	defer process.mu.Unlock()
	assert.Equal(t, 1, process.waitCalls)
	assert.Equal(t, 1, process.killCalls)
	assert.Equal(t, 1, process.deleteCalls)
}

func TestCleanupTerminalProcessOnlyDeletesExitedProcess(t *testing.T) {
	process := newStubTerminalProcess()

	cleanupTerminalProcess(context.Background(), process, true, true)

	process.mu.Lock()
	defer process.mu.Unlock()
	assert.Zero(t, process.waitCalls)
	assert.Zero(t, process.killCalls)
	assert.Equal(t, 1, process.deleteCalls)
}

func TestCleanupTerminalProcessOnlyDeletesUnstartedProcess(t *testing.T) {
	process := newStubTerminalProcess()

	cleanupTerminalProcess(context.Background(), process, false, false)

	process.mu.Lock()
	defer process.mu.Unlock()
	assert.Zero(t, process.waitCalls)
	assert.Zero(t, process.killCalls)
	assert.Equal(t, 1, process.deleteCalls)
}

func TestResizeTerminalForwardsNonZeroDimensions(t *testing.T) {
	process := newStubTerminalProcess()

	require.NoError(t, resizeTerminal(context.Background(), process, 120, 40))
	require.NoError(t, resizeTerminal(context.Background(), process, 0, 40))
	require.NoError(t, resizeTerminal(context.Background(), process, 9999999, 9999999))

	process.mu.Lock()
	defer process.mu.Unlock()
	assert.Equal(t, [][2]uint32{{120, 40}, {terminalMaxCols, terminalMaxRows}}, process.resizeCalls)
}

func TestMergeTerminalEnvOverridesByName(t *testing.T) {
	merged := mergeTerminalEnv(
		[]string{"PATH=/usr/bin", "TERM=xterm", "IGNORED"},
		[]string{"TERM=xterm-256color", "LANG=C.UTF-8"},
	)

	assert.Equal(t, []string{"PATH=/usr/bin", "TERM=xterm-256color", "LANG=C.UTF-8"}, merged)
}

func TestTerminalProcessFromSpecDoesNotMutateBase(t *testing.T) {
	base := &specs.Process{
		Args:    []string{"original"},
		Env:     []string{"PATH=/usr/bin", "TERM=xterm"},
		Cwd:     "/original",
		Rlimits: []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024}},
	}
	open := &cubebox.TerminalOpenRequest{
		Args: []string{"/bin/bash"},
		Env:  []string{"TERM=xterm-256color"},
		Cwd:  "/workspace",
	}

	process := terminalProcessFromSpec(base, open)
	assert.Equal(t, []string{"original"}, base.Args)
	assert.Equal(t, []string{"PATH=/usr/bin", "TERM=xterm"}, base.Env)
	assert.Equal(t, "/original", base.Cwd)
	assert.Equal(t, []string{"/bin/bash"}, process.Args)
	assert.Equal(t, []string{"PATH=/usr/bin", "TERM=xterm-256color"}, process.Env)
	assert.Equal(t, "/workspace", process.Cwd)
	assert.True(t, process.Terminal)

	process.Rlimits[0].Soft = 512
	assert.Equal(t, uint64(1024), base.Rlimits[0].Soft)
}

func TestEnqueueTerminalStdinIsBoundedAndCopiesFrames(t *testing.T) {
	queue := make(chan []byte, 1)
	frame := []byte("input")
	require.NoError(t, enqueueTerminalStdin(queue, frame))
	frame[0] = 'X'
	assert.Equal(t, "input", string(<-queue))

	require.NoError(t, enqueueTerminalStdin(queue, []byte("queued")))
	err := enqueueTerminalStdin(queue, []byte("overflow"))
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))

	err = enqueueTerminalStdin(queue, make([]byte, terminalMaxStdinFrame+1))
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}
