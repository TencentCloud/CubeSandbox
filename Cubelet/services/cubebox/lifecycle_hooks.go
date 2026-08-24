// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"

	containerd "github.com/containerd/containerd/v2/client"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// In-sandbox lifecycle hooks (issue #1308). Cubelet executes these around the
// pause/resume transition, inside the guest, before the sandbox is reported
// paused / ready. Two handler kinds are supported:
//   - exec:  run argv inside the guest via containerd task.Exec
//     (same path as collectEnvdVersion).
//   - http:  HTTP GET to a user-configured endpoint in the sandbox
//     (same path as doStopHooks).
//
// Hooks are opt-in: a nil/empty config is a no-op and leaves the pause/resume
// path byte-identical to the pre-feature behaviour. The failure policy
// (ABORT/IGNORE) is applied by the call sites in update.go, not here — this
// file only executes a hook and reports its outcome.

const (
	// lifecycleHookMaxTimeoutMs caps the configured per-hook timeout. A Go child
	// context cannot extend the pause/resume RPC deadline, so the effective
	// runtime ceiling is min(timeout_ms, remaining operation budget).
	lifecycleHookMaxTimeoutMs = 30000
	// lifecycleHookOutputLimit bounds captured exec output for error reporting.
	lifecycleHookOutputLimit = 4 << 10 // 4 KiB
	// lifecycleHookExecIDPrefix namespaces hook exec ids so they don't collide
	// with the envd-version probe or user execs in containerd.
	lifecycleHookExecIDPrefix = "cubesandbox-lifecycle-hook-"

	// envdHealthPath is the envd readiness endpoint (returns 204 when ready).
	envdHealthPath        = "/health"
	envdReadyMaxAttempts  = 10
	envdReadyAttemptDelay = 200 * time.Millisecond
	// envdReadyAttemptTimeout bounds a single /health probe so a hung guest
	// cannot consume the whole readiness budget in one attempt.
	envdReadyAttemptTimeout = 300 * time.Millisecond

	// Hook phase identifiers, surfaced to the hook via headers / env vars.
	lifecyclePhasePrePause   = "prePause"
	lifecyclePhasePostResume = "postResume"
)

// Lifecycle context is delivered to the hook so it can correlate the transition
// and act idempotently on the operation ID (= the pause/resume RequestID).
const (
	hookHTTPHeaderPhase     = "X-Cube-Lifecycle-Phase"
	hookHTTPHeaderOpID      = "X-Cube-Lifecycle-Operation-ID"
	hookHTTPHeaderSandboxID = "X-Cube-Lifecycle-Sandbox-ID"

	hookEnvPhase     = "CUBE_LIFECYCLE_PHASE"
	hookEnvOpID      = "CUBE_LIFECYCLE_OPERATION_ID"
	hookEnvSandboxID = "CUBE_LIFECYCLE_SANDBOX_ID"
)

// isAbortPolicy reports whether a hook failure should block the transition.
// ABORT is the proto zero value (the default for an explicitly-configured hook).
func isAbortPolicy(p cubebox.HookFailurePolicy) bool {
	return p != cubebox.HookFailurePolicy_HOOK_FAILURE_POLICY_IGNORE
}

// runPrePauseHook executes the configured prePause hook for the container, if any.
// Returns nil when no hook is configured.
func (l *local) runPrePauseHook(ctx context.Context, ci *cubeboxstore.Container, sandboxID, opID string) error {
	hook := ci.Config.GetLifecycleHooks().GetPrePause()
	if hook == nil || hook.Handler == nil {
		return nil
	}
	return l.runLifecycleHook(ctx, ci, hook, sandboxID, lifecyclePhasePrePause, opID)
}

// runPostResumeHook executes the configured postResume hook for the container, if
// any. For an HTTP hook that targets envd's own port, it first runs a bounded
// best-effort envd-readiness wait so the hook doesn't race a listener still
// warming up after VM resume. The wait shares the hook's own timeout_ms budget
// (allocated once below) and is skipped when the hook targets a different port.
// Exec hooks are NOT gated on guest readiness: task.Resume returning RUNNING
// means the vCPUs are scheduled, but guest userspace may still be booting, so an
// exec postResume hook must tolerate a briefly-unready guest (size timeout_ms
// accordingly). Returns nil when no hook is configured.
func (l *local) runPostResumeHook(ctx context.Context, ci *cubeboxstore.Container, sandboxID, opID string) error {
	hook := ci.Config.GetLifecycleHooks().GetPostResume()
	if hook == nil || hook.Handler == nil {
		return nil
	}
	// Allocate the hook's budget ONCE. The envd-readiness wait (when applicable)
	// and the hook itself share this budget, so total postResume time stays within
	// timeout_ms rather than approaching 2×timeout_ms. runLifecycleHook derives
	// its own child ctx from this one, which is a no-op against the shared deadline.
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(hook.GetTimeoutMs())*time.Millisecond)
	defer cancel()
	if httpGet := hook.Handler.GetHttpGet(); httpGet != nil && httpGet.GetPort() == int32(l.getEnvdInitPort()) {
		if err := l.waitEnvdReady(hookCtx, ci.IP); err != nil {
			log.G(ctx).WithField("sandboxID", sandboxID).
				Warnf("postResume: envd readiness wait ended early: %v", err)
		}
	}
	return l.runLifecycleHook(hookCtx, ci, hook, sandboxID, lifecyclePhasePostResume, opID)
}

// runPostResumeHookResult runs the postResume hook once for the sandbox (the
// pod/first container, which carries the template's lifecycle_hooks) and
// applies its failure policy. Hooks are a sandbox-level transition semantic
// (resume is whole-sandbox), so they run once per sandbox, not once per
// container — running them per-container would repeat the same exec/HTTP N
// times for an N-container sandbox. Returns nil when no hook is configured or
// it succeeds (or fails under IGNORE); returns TaskResumeFailed on ABORT.
// Note: a postResume ABORT failure is NOT recoverable by retrying resume — the
// retry takes the "already running" idempotent path and never re-enters the
// hook; an explicit pause→resume is needed to re-run it.
func (l *local) runPostResumeHookResult(ctx context.Context, sb *cubeboxstore.CubeBox, opID string) *errorcode.Ret {
	ci := sb.FirstContainer()
	if ci == nil {
		return nil
	}
	hook := ci.Config.GetLifecycleHooks().GetPostResume()
	if hook == nil || hook.Handler == nil {
		return nil
	}
	if err := l.runPostResumeHook(ctx, ci, sb.ID, opID); err != nil {
		if isAbortPolicy(hook.GetFailurePolicy()) {
			return &errorcode.Ret{
				RetCode: errorcode.ErrorCode_TaskResumeFailed,
				RetMsg:  fmt.Sprintf("postResume lifecycle hook failed: %v", err),
			}
		}
		log.G(ctx).WithField("sandboxID", sb.ID).
			Warnf("postResume lifecycle hook failed (IGNORE policy): %v", err)
	}
	return nil
}

// runPostResumeHookForCreate runs the postResume hook for a resume-from-pause
// Create. In this architecture resume IS a Create (the VM is restored from the
// pause snapshot), so a failing hook cannot block the transition without
// tearing down a running sandbox: the ABORT failure is logged (WARN) and
// surfaced through the container status message instead of failing the create.
// A retry resumes nothing — re-running the hook requires pause→resume again.
func (l *local) runPostResumeHookForCreate(ctx context.Context, sb *cubeboxstore.CubeBox, opID string) {
	if ret := l.runPostResumeHookResult(ctx, sb, opID); ret != nil {
		logger := log.G(ctx).WithField("sandboxID", sb.ID)
		logger.Warnf("%s", ret.RetMsg)
		if ci := sb.FirstContainer(); ci != nil && ci.Status != nil {
			ci.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.Message = ret.RetMsg
				return status, nil
			})
		}
	}
}

// runPrePauseHookResult runs the prePause hook once for the sandbox (the
// pod/first container) and applies its failure policy. See runPostResumeHookResult
// for why this is per-sandbox, not per-container. Returns nil when no hook is
// configured or it succeeds (or fails under IGNORE); returns TaskPauseFailed on
// the first ABORT failure.
func (l *local) runPrePauseHookResult(ctx context.Context, sb *cubeboxstore.CubeBox, opID string) *errorcode.Ret {
	ci := sb.FirstContainer()
	if ci == nil {
		return nil
	}
	hook := ci.Config.GetLifecycleHooks().GetPrePause()
	if hook == nil || hook.Handler == nil {
		return nil
	}
	if err := l.runPrePauseHook(ctx, ci, sb.ID, opID); err != nil {
		if isAbortPolicy(hook.GetFailurePolicy()) {
			return &errorcode.Ret{
				RetCode: errorcode.ErrorCode_TaskPauseFailed,
				RetMsg:  fmt.Sprintf("prePause lifecycle hook failed: %v", err),
			}
		}
		log.G(ctx).WithField("sandboxID", sb.ID).
			Warnf("prePause lifecycle hook failed (IGNORE policy): %v", err)
	}
	return nil
}

// runLifecycleHook executes one in-sandbox hook and returns an error describing
// any failure. The caller applies the failure policy.
func (l *local) runLifecycleHook(
	ctx context.Context,
	ci *cubeboxstore.Container,
	hook *cubebox.LifecycleHook,
	sandboxID, phase, opID string,
) error {
	if hook.GetTimeoutMs() <= 0 || hook.GetTimeoutMs() > lifecycleHookMaxTimeoutMs {
		return fmt.Errorf("lifecycle hook %s: invalid timeout_ms %d (must be 1..%d)",
			phase, hook.GetTimeoutMs(), lifecycleHookMaxTimeoutMs)
	}
	// Child ctx; the effective ceiling is min(timeout_ms, parent remaining) — a
	// Go child context cannot extend the pause/resume RPC deadline.
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(hook.GetTimeoutMs())*time.Millisecond)
	defer cancel()

	start := time.Now()
	logger := log.G(ctx).WithFields(map[string]interface{}{
		"sandboxID":   sandboxID,
		"phase":       phase,
		"operationID": opID,
		"timeoutMs":   hook.GetTimeoutMs(),
	})

	var err error
	switch {
	case hook.Handler.GetExec() != nil:
		err = l.runHookExec(hookCtx, ci, hook.Handler.GetExec(), hook.GetTimeoutMs(), sandboxID, phase, opID)
	case hook.Handler.GetHttpGet() != nil:
		err = runHookHTTP(hookCtx, ci, hook.Handler.GetHttpGet(), sandboxID, phase, opID)
	default:
		return fmt.Errorf("lifecycle hook %s: handler has neither exec nor http_get", phase)
	}

	if err != nil {
		logger.WithField("durationMs", time.Since(start).Milliseconds()).
			Warnf("lifecycle hook %s failed: %v", phase, err)
		return err
	}
	logger.WithField("durationMs", time.Since(start).Milliseconds()).
		Infof("lifecycle hook %s succeeded", phase)
	return nil
}

// runHookHTTP performs an HTTP GET against the user-configured endpoint inside
// the sandbox. A 2xx response is success; anything else (transport error,
// non-2xx, timeout) is a failure. Reuses NewRequestForHTTPGetAction so the
// host/port/path/headers semantics match the existing probe + preStop hooks.
func runHookHTTP(
	ctx context.Context,
	ci *cubeboxstore.Container,
	httpGet *cubebox.HTTPGetAction,
	sandboxID, phase, opID string,
) error {
	if ci.IP == "" || ci.IP == "<nil>" {
		return fmt.Errorf("lifecycle hook %s: sandbox IP is empty", phase)
	}
	req, err := NewRequestForHTTPGetAction(ctx, httpGet, ci.IP)
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: build http request: %w", phase, err)
	}
	req.Header.Set(hookHTTPHeaderPhase, phase)
	req.Header.Set(hookHTTPHeaderOpID, opID)
	req.Header.Set(hookHTTPHeaderSandboxID, sandboxID)

	// Do not follow redirects: the hook endpoint is fully in-guest (possibly
	// untrusted), and a 3xx must not make the host-side cubelet fetch an
	// arbitrary URL. Matches the envd/probe clients (http.ErrUseLastResponse);
	// a 3xx is then treated as a non-2xx hook failure below.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: http request: %w", phase, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, lifecycleHookOutputLimit+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lifecycle hook %s: http returned status %d", phase, resp.StatusCode)
	}
	return nil
}

// runHookExec runs the hook's argv inside the sandbox guest via containerd
// task.Exec. A zero exit code is success; non-zero exit, timeout, or transport
// failure is an error carrying the bounded captured output. Mirrors the proven
// collectEnvdVersion exec path (envd_version.go).
func (l *local) runHookExec(
	ctx context.Context,
	ci *cubeboxstore.Container,
	exec *cubebox.ExecAction,
	timeoutMs int32,
	sandboxID, phase, opID string,
) (retErr error) {
	if len(exec.GetCommand()) == 0 || strings.TrimSpace(exec.GetCommand()[0]) == "" {
		return fmt.Errorf("lifecycle hook %s: exec command is empty", phase)
	}
	logger := log.G(ctx).WithFields(map[string]interface{}{
		"sandboxID": sandboxID,
		"phase":     phase,
	})

	cb, err := l.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: get cubebox: %w", phase, err)
	}
	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	execCtx := namespaces.WithNamespace(ctx, ns)

	container, err := l.client.LoadContainer(execCtx, sandboxID)
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: load container: %w", phase, err)
	}
	task, err := container.Task(execCtx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: get task: %w", phase, err)
	}
	spec, err := container.Spec(execCtx)
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: get spec: %w", phase, err)
	}
	if spec.Process == nil {
		return fmt.Errorf("lifecycle hook %s: container spec has no process", phase)
	}

	// Fork the container's process spec so the exec inherits the right user /
	// capabilities / env, then overlay the hook command, working dir, and
	// lifecycle context env.
	pspecCopy := *spec.Process
	pspecCopy.Env = append([]string{}, spec.Process.Env...)
	pspec := &pspecCopy
	pspec.Terminal = false
	pspec.Args = exec.GetCommand()
	if wd := exec.GetWorkingDir(); wd != "" {
		pspec.Cwd = wd
	}
	pspec.Env = append(pspec.Env,
		hookEnvPhase+"="+phase,
		hookEnvOpID+"="+opID,
		hookEnvSandboxID+"="+sandboxID,
	)

	// NullIO: the hook's exit code is the signal, so no host-side stdio is
	// captured. Empty FIFO paths also let the shim skip the passfd IO handshake
	// entirely, which avoids the guest-agent passfd protocol version coupling
	// (a stream-backed exec would fail at Start when the in-guest agent predates
	// the shim's passfd protocol). Hook output, if needed, should go to the
	// guest console or a file the operator can read.
	execID := lifecycleHookExecIDPrefix + uuid.New().String()
	process, err := runProbeCall(execCtx, func() (containerd.Process, error) {
		return task.Exec(execCtx, execID, pspec, cio.NullIO)
	})
	if err != nil {
		return fmt.Errorf("lifecycle hook %s: exec: %w", phase, err)
	}
	defer func() {
		deleteCtx := namespaces.WithNamespace(context.Background(), ns)
		if _, derr := process.Delete(deleteCtx); derr != nil {
			logger.Warnf("lifecycle hook %s: delete exec process: %v", phase, derr)
		}
	}()

	statusCh, err := runProbeCall(execCtx, func() (<-chan containerd.ExitStatus, error) {
		return process.Wait(execCtx)
	})
	if err != nil {
		abortProbe(process, nil, logger, "lifecycle hook "+phase, "wait", err)
		return fmt.Errorf("lifecycle hook %s: wait: %w", phase, err)
	}
	// evalExit maps the containerd exit status to the hook result.
	evalExit := func(status containerd.ExitStatus) error {
		code, _, serr := status.Result()
		if serr != nil {
			return fmt.Errorf("lifecycle hook %s: exit status error: %w", phase, serr)
		}
		if code != 0 {
			// No host-side stdio with NullIO; the exit code is the signal.
			return fmt.Errorf("lifecycle hook %s: exited with code %d", phase, code)
		}
		return nil
	}

	if _, err := runProbeCall(execCtx, func() (struct{}, error) {
		return struct{}{}, process.Start(execCtx)
	}); err != nil {
		abortProbe(process, statusCh, logger, "lifecycle hook "+phase, "start", err)
		return fmt.Errorf("lifecycle hook %s: start: %w", phase, err)
	}

	select {
	case <-execCtx.Done():
		killProbeProcess(process, statusCh, logger, "lifecycle hook "+phase)
		return fmt.Errorf("lifecycle hook %s: timed out after %dms", phase, timeoutMs)
	case status := <-statusCh:
		return evalExit(status)
	}
}

// waitEnvdReady performs a bounded, best-effort probe of the in-sandbox envd
// /health endpoint. It returns nil once envd reports ready (2xx) OR when the
// attempt budget is exhausted — it never fails the resume: the postResume hook's
// own timeout/policy is the authority. Empty sandboxIP is a no-op.
func (l *local) waitEnvdReady(ctx context.Context, sandboxIP string) error {
	if sandboxIP == "" || sandboxIP == "<nil>" {
		return nil
	}
	port := l.getEnvdInitPort()
	client := l.getEnvdHTTPClient()
	for attempt := 1; attempt <= envdReadyMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, envdReadyAttemptTimeout)
		reqURL := formatURL("http", sandboxIP, port, envdHealthPath)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, reqURL.String(), nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				cancel()
				return nil
			}
		}
		cancel()
		select {
		case <-time.After(envdReadyAttemptDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
