// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

type stubProbeProcess struct {
	mu        sync.Mutex
	killCalls int
}

func (p *stubProbeProcess) ID() string { return "stub" }
func (p *stubProbeProcess) Pid() uint32 {
	return 0
}
func (p *stubProbeProcess) Start(context.Context) error { return nil }
func (p *stubProbeProcess) Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	return nil, nil
}
func (p *stubProbeProcess) Kill(context.Context, syscall.Signal, ...containerd.KillOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killCalls++
	return nil
}
func (p *stubProbeProcess) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	return nil, nil
}
func (p *stubProbeProcess) CloseIO(context.Context, ...containerd.IOCloserOpts) error { return nil }
func (p *stubProbeProcess) Resize(context.Context, uint32, uint32) error              { return nil }
func (p *stubProbeProcess) IO() cio.IO                                                { return nil }
func (p *stubProbeProcess) Status(context.Context) (containerd.Status, error) {
	return containerd.Status{}, nil
}

func (p *stubProbeProcess) killCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killCalls
}

func testProbeLogger() *log.CubeWrapperLogEntry {
	return log.NewWrapperLogEntry(CubeLog.WithContext(context.Background()))
}

func TestRunProbeCallReturnsResult(t *testing.T) {
	ctx := context.Background()
	got, err := runProbeCall(ctx, func() (string, error) {
		return "ok", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
}

func TestRunProbeCallPropagatesError(t *testing.T) {
	ctx := context.Background()
	want := errors.New("boom")
	_, err := runProbeCall(ctx, func() (int, error) {
		return 0, want
	})
	assert.ErrorIs(t, err, want)
}

func TestRunProbeCallTimesOutOnBlockingCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := runProbeCall(ctx, func() (struct{}, error) {
		time.Sleep(2 * time.Second)
		return struct{}{}, nil
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, probeCallTimedOut(err))
	assert.Less(t, elapsed, time.Second)
}

func TestRunProbeCallRecoversPanic(t *testing.T) {
	ctx := context.Background()
	_, err := runProbeCall(ctx, func() (string, error) {
		panic("probe exploded")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe panic")
	assert.Contains(t, err.Error(), "probe exploded")
}

func TestParseEnvdVersionFromOutputPrefersStdout(t *testing.T) {
	t.Parallel()

	stdout := "envd version 1.2.3\n"
	stderr := "WARN: noise 9.9.9\n"
	assert.Equal(t, "1.2.3", parseEnvdVersionFromOutput(stdout, stderr))
}

func TestParseEnvdVersionFromOutputFallsBackToStderr(t *testing.T) {
	t.Parallel()

	stdout := ""
	stderr := "WARN: starting envd 4.5.6\n"
	assert.Equal(t, "4.5.6", parseEnvdVersionFromOutput(stdout, stderr))
}

func TestBoundedBufferEnforcesPerStreamLimit(t *testing.T) {
	t.Parallel()

	buf := &boundedBuffer{limit: 8}
	_, err := buf.Write([]byte("12345678"))
	require.NoError(t, err)
	_, err = buf.Write([]byte("extra"))
	require.NoError(t, err)
	assert.Equal(t, "12345678", buf.String())
}

func TestAbortProbeKillsOnProbeError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "non-timeout start failure",
			err:  errors.New("start rpc failed"),
		},
		{
			name: "timeout start failure",
			err:  context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			process := &stubProbeProcess{}
			statusCh := make(chan containerd.ExitStatus, 1)
			statusCh <- *containerd.NewExitStatus(137, time.Now(), nil)

			abortProbe(process, statusCh, testProbeLogger(), "start", tt.err)

			assert.Equal(t, 1, process.killCount())
		})
	}
}

func TestKillProbeProcessNilStatusCh(t *testing.T) {
	t.Parallel()

	process := &stubProbeProcess{}
	killProbeProcess(process, nil, testProbeLogger())
	assert.Equal(t, 1, process.killCount())
}
