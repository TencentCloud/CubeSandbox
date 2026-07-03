// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestProbeEnvdReadinessSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, envdReadinessPath, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := testServerHostPort(t, srv)
	err := probeEnvdReadiness(context.Background(), host, port)
	require.NoError(t, err)
}

func TestProbeEnvdReadinessRetriesUntilReady(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := testServerHostPort(t, srv)
	err := probeEnvdReadiness(context.Background(), host, port)
	require.NoError(t, err)
	assert.EqualValues(t, 3, attempts.Load())
}

func TestProbeEnvdReadinessRejectsNonOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	host, port := testServerHostPort(t, srv)
	err := probeEnvdReadiness(context.Background(), host, port)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestFinalizeReadyEnvdVersionSkipsReadinessWhenVersionMissing(t *testing.T) {
	t.Parallel()

	lookupCalled := false
	probeCalled := false
	version := finalizeReadyEnvdVersion(
		context.Background(),
		"sandbox-1",
		"",
		testProbeLogger(),
		func(context.Context, string) (string, error) {
			lookupCalled = true
			return "172.31.0.2", nil
		},
		func(context.Context, string, int) error {
			probeCalled = true
			return nil
		},
	)

	assert.Empty(t, version)
	assert.False(t, lookupCalled)
	assert.False(t, probeCalled)
}

func TestFinalizeReadyEnvdVersionReturnsEmptyWhenSandboxLookupFails(t *testing.T) {
	t.Parallel()

	probeCalled := false
	version := finalizeReadyEnvdVersion(
		context.Background(),
		"sandbox-lookup-fail",
		"0.5.11",
		testProbeLogger(),
		func(context.Context, string) (string, error) {
			return "", errors.New("lookup failed")
		},
		func(context.Context, string, int) error {
			probeCalled = true
			return nil
		},
	)

	assert.Empty(t, version)
	assert.False(t, probeCalled)
}

func TestFinalizeReadyEnvdVersionReturnsEmptyWhenReadinessFails(t *testing.T) {
	t.Parallel()

	version := finalizeReadyEnvdVersion(
		context.Background(),
		"sandbox-not-ready",
		"0.5.11",
		testProbeLogger(),
		func(context.Context, string) (string, error) {
			return "172.31.0.3", nil
		},
		func(context.Context, string, int) error {
			return errors.New("connection refused")
		},
	)

	assert.Empty(t, version)
}

func TestFinalizeReadyEnvdVersionReturnsVersionWhenReadinessSucceeds(t *testing.T) {
	t.Parallel()

	version := finalizeReadyEnvdVersion(
		context.Background(),
		"sandbox-ready",
		"0.5.11",
		testProbeLogger(),
		func(context.Context, string) (string, error) {
			return "172.31.0.4", nil
		},
		func(context.Context, string, int) error {
			return nil
		},
	)

	assert.Equal(t, "0.5.11", version)
}

func TestProbeEnvdReadinessReturnsWhenContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	host, port := testServerHostPort(t, srv)
	cfg, err := buildEnvdReadinessProbe(context.Background(), host, port)
	require.NoError(t, err)
	cfg.FailureThreshold = 10
	cfg.Period = 5 * time.Millisecond
	cfg.ProbeTimeout = 100 * time.Millisecond
	cfg.Timeout = time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = probeEnvdReadinessWithClient(ctx, cfg, newEnvdReadinessHTTPClient())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 300*time.Millisecond)
}

func TestProbeEnvdReadinessReturnsConnectionRefused(t *testing.T) {
	t.Parallel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host, port := testListenerHostPort(t, l)
	require.NoError(t, l.Close())

	cfg, err := buildEnvdReadinessProbe(context.Background(), host, port)
	require.NoError(t, err)
	cfg.FailureThreshold = 1
	cfg.Period = 0
	cfg.ProbeTimeout = 100 * time.Millisecond
	cfg.Timeout = 200 * time.Millisecond

	err = probeEnvdReadinessWithClient(context.Background(), cfg, newEnvdReadinessHTTPClient())
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "refused")
}

func TestProbeEnvdReadinessRejectsRedirect(t *testing.T) {
	t.Parallel()

	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	host, port := testServerHostPort(t, redirect)
	cfg, err := buildEnvdReadinessProbe(context.Background(), host, port)
	require.NoError(t, err)
	cfg.FailureThreshold = 1
	cfg.Period = 0
	cfg.ProbeTimeout = 100 * time.Millisecond
	cfg.Timeout = 200 * time.Millisecond

	err = probeEnvdReadinessWithClient(context.Background(), cfg, newEnvdReadinessHTTPClient())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "302")
	assert.Zero(t, redirected.Load())
}

func testServerHostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()

	host, portText, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}

func testListenerHostPort(t *testing.T, l net.Listener) (string, int) {
	t.Helper()

	host, portText, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}
