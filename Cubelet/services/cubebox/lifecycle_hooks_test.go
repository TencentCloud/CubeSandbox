// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func strPtr(s string) *string { return &s }

// newLifecycleHookTestBox builds a CubeBox whose first container carries the
// given lifecycle hooks config and sandbox IP.
func newLifecycleHookTestBox(id, ip string, hooks *cubeboxpb.LifecycleHooks) *cubeboxstore.CubeBox {
	container := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: id, Config: &cubeboxpb.ContainerConfig{LifecycleHooks: hooks}},
		IP:       ip,
		// A real sandbox's first (and usually only) container is the pod
		// container (IsPod=true), which sb.All() EXCLUDES — so this must be
		// true to prove the hooks run via AllContainers(), not All().
		IsPod: true,
	}
	return &cubeboxstore.CubeBox{
		Metadata:           cubeboxstore.Metadata{ID: id, CreatedAt: time.Now().UnixNano()},
		FirstContainerName: id,
		ContainersMap: &cubeboxstore.ContainersMap{
			ContainerMap: map[string]*cubeboxstore.Container{id: container},
		},
	}
}

func httpHook(port int32, path string, timeoutMs int32, policy cubeboxpb.HookFailurePolicy) *cubeboxpb.LifecycleHook {
	return &cubeboxpb.LifecycleHook{
		Handler: &cubeboxpb.LifecycleHookHandler{
			HttpGet: &cubeboxpb.HTTPGetAction{Port: port, Path: strPtr(path)},
		},
		TimeoutMs:     timeoutMs,
		FailurePolicy: policy,
	}
}

// --- runHookHTTP (free function) --------------------------------------------

func TestRunHookHTTP_SuccessOn2xx(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, "prePause", r.Header.Get(hookHTTPHeaderPhase))
		assert.Equal(t, "op-1", r.Header.Get(hookHTTPHeaderOpID))
		assert.Equal(t, "sb-1", r.Header.Get(hookHTTPHeaderSandboxID))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	host, port := hostPortFromURL(t, srv.URL)
	ci := &cubeboxstore.Container{IP: host}
	err := runHookHTTP(context.Background(), ci,
		&cubeboxpb.HTTPGetAction{Port: port, Path: strPtr("/")}, "sb-1", "prePause", "op-1")
	require.NoError(t, err)
	assert.True(t, called, "hook endpoint must be called")
}

func TestRunHookHTTP_FailsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)
	ci := &cubeboxstore.Container{IP: host}
	err := runHookHTTP(context.Background(), ci,
		&cubeboxpb.HTTPGetAction{Port: port, Path: strPtr("/")}, "sb", "prePause", "op")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestRunHookHTTP_FailsOnConnectionRefused(t *testing.T) {
	ci := &cubeboxstore.Container{IP: "127.0.0.1"}
	// Port 1 is reserved and not listening → connection refused.
	err := runHookHTTP(context.Background(), ci,
		&cubeboxpb.HTTPGetAction{Port: 1, Path: strPtr("/")}, "sb", "prePause", "op")
	assert.Error(t, err)
}

func TestRunHookHTTP_EmptyIP(t *testing.T) {
	ci := &cubeboxstore.Container{IP: ""}
	err := runHookHTTP(context.Background(), ci,
		&cubeboxpb.HTTPGetAction{Port: 80, Path: strPtr("/")}, "sb", "prePause", "op")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IP is empty")
}

// --- timeout / validation ----------------------------------------------------

func TestRunLifecycleHook_InvalidTimeout(t *testing.T) {
	l := &local{}
	ci := &cubeboxstore.Container{IP: "127.0.0.1"}
	for _, timeoutMs := range []int32{0, -1, lifecycleHookMaxTimeoutMs + 1} {
		err := l.runLifecycleHook(context.Background(), ci,
			&cubeboxpb.LifecycleHook{
				Handler:   &cubeboxpb.LifecycleHookHandler{HttpGet: &cubeboxpb.HTTPGetAction{Port: 1}},
				TimeoutMs: timeoutMs,
			}, "sb", "prePause", "op")
		require.Error(t, err, "timeout %d must be rejected", timeoutMs)
		assert.Contains(t, err.Error(), "invalid timeout_ms")
	}
}

// --- policy wrappers ---------------------------------------------------------

func TestRunPrePauseHookResult_NoHookNil(t *testing.T) {
	l := &local{}
	sb := newLifecycleHookTestBox("sb", "127.0.0.1", nil)
	assert.Nil(t, l.runPrePauseHookResult(context.Background(), sb, "op"))
}

func TestRunPrePauseHookResult_AbortReturnsPauseFailed(t *testing.T) {
	srv := failingHookServer(t)
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)
	l := &local{}
	sb := newLifecycleHookTestBox("sb", host, &cubeboxpb.LifecycleHooks{
		PrePause: httpHook(port, "/", 1000, cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT),
	})
	ret := l.runPrePauseHookResult(context.Background(), sb, "op")
	require.NotNil(t, ret)
	assert.Equal(t, errorcode.ErrorCode_TaskPauseFailed, ret.RetCode)
	assert.Contains(t, ret.RetMsg, "prePause lifecycle hook failed")
}

func TestRunPrePauseHookResult_IgnoreReturnsNil(t *testing.T) {
	srv := failingHookServer(t)
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)
	l := &local{}
	sb := newLifecycleHookTestBox("sb", host, &cubeboxpb.LifecycleHooks{
		PrePause: httpHook(port, "/", 1000, cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_IGNORE),
	})
	assert.Nil(t, l.runPrePauseHookResult(context.Background(), sb, "op"))
}

func TestRunPrePauseHookResult_SuccessReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)
	l := &local{}
	sb := newLifecycleHookTestBox("sb", host, &cubeboxpb.LifecycleHooks{
		PrePause: httpHook(port, "/", 1000, cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT),
	})
	assert.Nil(t, l.runPrePauseHookResult(context.Background(), sb, "op"))
}

// postResume exercises the envd-readiness wait too: serve /health → 204 so the
// wait returns immediately, then the hook endpoint decides pass/fail.
func TestRunPostResumeHookResult_AbortReturnsResumeFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == envdHealthPath {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // hook fails
	}))
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)
	l := &local{envdHTTPClient: srv.Client(), envdInitPort: int(port)}
	sb := newLifecycleHookTestBox("sb", host, &cubeboxpb.LifecycleHooks{
		PostResume: httpHook(port, "/hook", 1000, cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT),
	})
	ret := l.runPostResumeHookResult(context.Background(), sb, "op")
	require.NotNil(t, ret)
	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, ret.RetCode)
}

func TestRunPostResumeHookResult_NoHookNil(t *testing.T) {
	l := &local{}
	sb := newLifecycleHookTestBox("sb", "127.0.0.1", nil)
	assert.Nil(t, l.runPostResumeHookResult(context.Background(), sb, "op"))
}

// --- isAbortPolicy -----------------------------------------------------------

func TestIsAbortPolicy(t *testing.T) {
	assert.True(t, isAbortPolicy(cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT))
	assert.True(t, isAbortPolicy(cubeboxpb.HookFailurePolicy(0)), "zero value must be ABORT")
	assert.False(t, isAbortPolicy(cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_IGNORE))
}

// --- doPreStop gating --------------------------------------------------------

func TestDoPreStop_SkipsOnPauseWhenPrePauseConfigured(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)

	ctx := constants.WithPreStopType(context.Background(), constants.PreStopTypePause)
	ci := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "c1",
			Config: &cubeboxpb.ContainerConfig{
				Prestop: &cubeboxpb.PreStop{
					TerminationGracePeriodMs: 1000,
					LifecyleHandler: &cubeboxpb.LifecycleHandler{
						HttpGet: &cubeboxpb.HTTPGetAction{Port: port, Path: strPtr("/")},
					},
				},
				// pre_pause configured → legacy preStop must be skipped on pause.
				LifecycleHooks: &cubeboxpb.LifecycleHooks{
					PrePause: httpHook(port, "/", 1000, cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT),
				},
			},
		},
		IP: host,
	}
	doPreStop(ctx, ci)
	assert.False(t, called, "legacy preStop must NOT fire on pause when pre_pause is configured")
}

func TestDoPreStop_FiresOnPauseWhenNoPrePause(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)

	ctx := constants.WithPreStopType(context.Background(), constants.PreStopTypePause)
	ci := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "c1",
			Config: &cubeboxpb.ContainerConfig{
				Prestop: &cubeboxpb.PreStop{
					TerminationGracePeriodMs: 1000,
					LifecyleHandler: &cubeboxpb.LifecycleHandler{
						HttpGet: &cubeboxpb.HTTPGetAction{Port: port, Path: strPtr("/")},
					},
				},
			},
		},
		IP: host,
	}
	doPreStop(ctx, ci)
	assert.True(t, called, "legacy preStop must fire on pause when no pre_pause is configured")
}

func TestDoPreStop_FiresOnDestroyEvenWithPrePause(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := hostPortFromURL(t, srv.URL)

	ctx := constants.WithPreStopType(context.Background(), constants.PreStopTypeDestroy)
	ci := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "c1",
			Config: &cubeboxpb.ContainerConfig{
				Prestop: &cubeboxpb.PreStop{
					TerminationGracePeriodMs: 1000,
					LifecyleHandler: &cubeboxpb.LifecycleHandler{
						HttpGet: &cubeboxpb.HTTPGetAction{Port: port, Path: strPtr("/")},
					},
				},
				LifecycleHooks: &cubeboxpb.LifecycleHooks{
					PrePause: httpHook(port, "/", 1000, cubeboxpb.HookFailurePolicy_HOOK_FAILURE_POLICY_ABORT),
				},
			},
		},
		IP: host,
	}
	doPreStop(ctx, ci)
	assert.True(t, called, "legacy preStop must fire on destroy even when pre_pause is configured")
}

// --- helpers -----------------------------------------------------------------

func failingHookServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
}

func hostPortFromURL(t *testing.T, raw string) (string, int32) {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	p, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return u.Hostname(), int32(p)
}
