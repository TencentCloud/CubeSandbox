// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telnet"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/protobuf/proto"
)

// restartMgr owns liveness probes and the backoff loop for every sandbox on
// this node. Exit events and probe failures both enter through OnExit; the
// decision lives in restartpolicy, not here.
type restartMgr struct {
	store     cubes.CubeboxAPI
	restarter interface {
		Restart(ctx context.Context, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error)
	}

	mu       sync.Mutex
	runs     map[string]*restartRun
	httpC    *http.Client
	reporter *statusReporter
}

type restartRun struct {
	cancel     context.CancelFunc
	req        *cubebox.RunCubeSandboxRequest
	backoff    restartpolicy.Backoff
	restarting bool
	suspended  bool
}

func newRestartMgr(store cubes.CubeboxAPI) *restartMgr {
	return &restartMgr{
		store: store,
		runs:  map[string]*restartRun{},
		httpC: newEnvdHTTPClient(),
	}
}

func (m *restartMgr) bind(r interface {
	Restart(ctx context.Context, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error)
}) {
	m.restarter = r
}

func restartDisabled() bool {
	if config.GetConfig() == nil || config.GetCommon() == nil {
		return false
	}
	return config.GetCommon().DisableRestartPolicy
}

// Start begins probes for a sandbox whose policy keeps a request.
func (m *restartMgr) Start(sandboxID string, req *cubebox.RunCubeSandboxRequest) {
	if m == nil || restartDisabled() || sandboxID == "" || req == nil {
		return
	}
	if !restartpolicy.KeepsRequest(req.GetRestartPolicy()) {
		return
	}
	stored := proto.Clone(req).(*cubebox.RunCubeSandboxRequest)
	if stored.Annotations == nil {
		stored.Annotations = map[string]string{}
	}
	stored.Annotations[constants.MasterAnnotationDesiredSandboxID] = sandboxID
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startLocked(sandboxID, stored)
}

func (m *restartMgr) startLocked(sandboxID string, req *cubebox.RunCubeSandboxRequest) {
	if prev := m.runs[sandboxID]; prev != nil && prev.cancel != nil {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &restartRun{
		cancel:  cancel,
		req:     req,
		backoff: restartpolicy.FromProto(req.GetRestartBackoff()),
	}
	m.runs[sandboxID] = run
	specs := probeSpecs(req)
	if len(specs) == 0 {
		return
	}
	agg := newProbeAgg(len(specs))
	for i, spec := range specs {
		spec := spec
		go m.probeLoop(ctx, sandboxID, i, spec, agg)
	}
}

// Stop forgets the sandbox. Destroy and a user delete call it.
func (m *restartMgr) Stop(sandboxID string) {
	if m == nil || sandboxID == "" {
		return
	}
	m.mu.Lock()
	run := m.runs[sandboxID]
	delete(m.runs, sandboxID)
	m.mu.Unlock()
	if run != nil && run.cancel != nil {
		run.cancel()
	}
}

// Suspend pauses probes but remembers the request, so a failed pause can resume them.
// It returns false when there is nothing to pause, including a restart already
// in progress: that loop must not be cancelled by commit, rollback or pause.
func (m *restartMgr) Suspend(sandboxID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.runs[sandboxID]
	if run == nil || run.restarting {
		return false
	}
	run.suspended = true
	if run.cancel != nil {
		run.cancel()
	}
	return true
}

// Resume restarts probes from the remembered request.
func (m *restartMgr) Resume(sandboxID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.runs[sandboxID]
	if run == nil || run.req == nil {
		return
	}
	run.suspended = false
	m.startLocked(sandboxID, run.req)
}

// OnExit is the only entry for "the sandbox is unhealthy".
func (m *restartMgr) OnExit(sandboxID string, exitCode int32, reason restartpolicy.Reason) {
	if m == nil || restartDisabled() || sandboxID == "" {
		return
	}
	m.mu.Lock()
	run := m.runs[sandboxID]
	if run == nil || run.restarting || run.suspended {
		m.mu.Unlock()
		// No run means this sandbox is not inside a restart. Record the exit
		// so a Never sandbox (and any other sandbox without a loop) becomes
		// Succeeded or Failed instead of looking like it is still Running.
		// A loop that is already restarting or suspended owns the sandbox.
		if run == nil {
			m.noteTerminal(sandboxID, exitCode, reason)
		}
		return
	}
	policy := ""
	if run.req != nil {
		policy = run.req.GetRestartPolicy()
	}
	if !restartpolicy.ShouldRestart(policy, reason) {
		delete(m.runs, sandboxID)
		cancel := run.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		m.noteTerminal(sandboxID, exitCode, reason)
		return
	}
	run.restarting = true
	if run.cancel != nil {
		run.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	req := run.req
	backoff := run.backoff
	m.mu.Unlock()

	go m.restartLoop(ctx, sandboxID, req, backoff, exitCode, reason)
}

// Recover reattaches probes after this process restarts. Sandboxes already
// terminated go back into the backoff loop when the policy says so.
func (m *restartMgr) Recover() {
	if m == nil || restartDisabled() || m.store == nil {
		return
	}
	for _, cb := range m.store.List() {
		if cb == nil || cb.IsPaused() {
			continue
		}
		req, err := cb.CopyOriginalRequest()
		if err != nil || req == nil || !restartpolicy.KeepsRequest(req.GetRestartPolicy()) {
			continue
		}
		if cb.RestartState == restartpolicy.StateGaveUp {
			continue
		}
		// A crash between restart-destroy and create leaves the cubebox row
		// with no containers. Treat that the same as an exited sandbox.
		dead := (cb.MainStatus() != nil && cb.MainStatus().IsTerminated()) ||
			(len(cb.AllContainers()) == 0 && cb.LastExitReason != "")
		if dead {
			if !restartpolicy.FromProto(req.GetRestartBackoff()).Allow(cb.RestartCount) {
				m.setState(cb.ID, restartpolicy.StateGaveUp, 0)
				continue
			}
			reason := restartpolicy.Reason(cb.LastExitReason)
			if reason == "" {
				reason = restartpolicy.ReasonForExit(cb.LastExitCode)
			}
			m.Start(cb.ID, req)
			m.OnExit(cb.ID, cb.LastExitCode, reason)
			continue
		}
		if config.RecoverLivenessProbesDisabled() {
			continue
		}
		m.Start(cb.ID, req)
	}
}

func (m *restartMgr) restartLoop(ctx context.Context, sandboxID string, req *cubebox.RunCubeSandboxRequest, backoff restartpolicy.Backoff, exitCode int32, reason restartpolicy.Reason) {
	defer func() {
		m.mu.Lock()
		if run := m.runs[sandboxID]; run != nil {
			run.restarting = false
		}
		m.mu.Unlock()
	}()

	m.noteExit(sandboxID, exitCode, reason)
	for {
		if ctx.Err() != nil {
			return
		}
		cb, err := m.store.Get(ctx, sandboxID)
		if err != nil || cb == nil {
			log.G(ctx).Errorf("restart: sandbox %s gone: %v", sandboxID, err)
			return
		}
		if cb.IsPaused() {
			return
		}
		if backoff.ResetCount(cb.LastRestartAt, time.Now()) && cb.RestartCount != 0 {
			cb.Lock()
			applyRestartTransition(cb, restartTransition{resetCount: true})
			if err := m.store.Save(ctx, cb); err != nil {
				log.G(ctx).Warnf("restart: save reset count for %s: %v", sandboxID, err)
			}
			cb.Unlock()
		}
		if !backoff.Allow(cb.RestartCount) {
			m.setState(sandboxID, restartpolicy.StateGaveUp, 0)
			log.G(ctx).Warnf("restart: sandbox %s gave up after %d restarts", sandboxID, cb.RestartCount)
			return
		}
		delay := backoff.Delay(cb.RestartCount)
		m.setState(sandboxID, restartpolicy.StateBackOff, time.Now().Add(delay).UnixNano())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		if cb2, err := m.store.Get(ctx, sandboxID); err == nil && cb2 != nil && cb2.IsPaused() {
			return
		}
		m.setState(sandboxID, restartpolicy.StateRestarting, 0)
		rsp, err := m.restarter.Restart(ctx, req)
		if err != nil || rsp == nil || rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Success {
			msg := ""
			if err != nil {
				msg = err.Error()
			} else if rsp != nil && rsp.GetRet() != nil {
				msg = rsp.GetRet().GetRetMsg()
			}
			log.G(ctx).Errorf("restart: sandbox %s failed: %s", sandboxID, msg)
			continue
		}
		m.setState(sandboxID, restartpolicy.StateRunning, 0)
		m.Start(sandboxID, req)
		return
	}
}

func (m *restartMgr) noteTerminal(sandboxID string, exitCode int32, reason restartpolicy.Reason) {
	if m == nil || m.store == nil || sandboxID == "" {
		return
	}
	cb, err := m.store.Get(context.Background(), sandboxID)
	if err != nil || cb == nil || cb.IsPaused() || cb.UserMarkDeletedTime != nil {
		return
	}
	switch cb.RestartState {
	case restartpolicy.StateRestarting, restartpolicy.StateBackOff, restartpolicy.StateGaveUp:
		return
	}
	state := restartpolicy.StateFailed
	if reason == restartpolicy.ReasonCompleted {
		state = restartpolicy.StateSucceeded
	}
	cb.Lock()
	applyRestartTransition(cb, restartTransition{
		setExit:   true,
		exitCode:  exitCode,
		setReason: true,
		reason:    string(reason),
		setState:  true,
		state:     state,
		bumpSeq:   true,
	})
	if err := m.store.Save(context.Background(), cb); err != nil {
		log.G(context.Background()).Warnf("restart: save terminal %s for %s: %v", state, sandboxID, err)
	}
	cb.Unlock()
	m.observe(sandboxID)
}

func (m *restartMgr) noteExit(sandboxID string, exitCode int32, reason restartpolicy.Reason) {
	cb, err := m.store.Get(context.Background(), sandboxID)
	if err != nil || cb == nil {
		return
	}
	cb.Lock()
	defer cb.Unlock()
	applyRestartTransition(cb, restartTransition{
		setExit:   true,
		exitCode:  exitCode,
		setReason: true,
		reason:    string(reason),
		setState:  true,
		state:     restartpolicy.StateBackOff,
	})
	if err := m.store.Save(context.Background(), cb); err != nil {
		log.G(context.Background()).Warnf("restart: save exit for %s: %v", sandboxID, err)
	}
}

func (m *restartMgr) setState(sandboxID, state string, next int64) {
	cb, err := m.store.Get(context.Background(), sandboxID)
	if err != nil || cb == nil {
		return
	}
	cb.Lock()
	applyRestartTransition(cb, restartTransition{
		setState:      true,
		state:         state,
		setNext:       true,
		nextRestartAt: next,
		bumpSeq:       true,
	})
	if err := m.store.Save(context.Background(), cb); err != nil {
		log.G(context.Background()).Warnf("restart: save state %s for %s: %v", state, sandboxID, err)
	}
	cb.Unlock()
	m.observe(sandboxID)
}

const envdProbePort int64 = 49983

func probeSpecs(req *cubebox.RunCubeSandboxRequest) []*cubebox.LivenessProbe {
	var specs []*cubebox.LivenessProbe
	if req == nil {
		return specs
	}
	for _, c := range req.GetContainers() {
		if c.GetLivenessProbe() != nil && c.GetLivenessProbe().GetProbeHandler() != nil {
			specs = append(specs, c.GetLivenessProbe())
		}
	}
	if len(specs) > 0 || !config.DefaultLivenessProbeEnabled() || !requestHasEnvd(req) {
		return specs
	}
	path := "/health"
	specs = append(specs, &cubebox.LivenessProbe{
		InitialDelaySecond: 30,
		PeriodSecond:       10,
		ProbeTimeoutSecond: 1,
		FailureThreshold:   3,
		ProbeHandler: &cubebox.ProbeHandler{
			HttpGet: &cubebox.HTTPGetAction{Port: int32(envdProbePort), Path: &path},
		},
	})
	return specs
}

func requestHasEnvd(req *cubebox.RunCubeSandboxRequest) bool {
	if strings.TrimSpace(req.GetAnnotations()[constants.MasterAnnotationComponentEnvdVersion]) != "" {
		return true
	}
	for _, port := range req.GetExposedPorts() {
		if port == envdProbePort {
			return true
		}
	}
	return false
}

type probeAgg struct {
	mu       sync.Mutex
	failures []int32
	fired    bool
}

func newProbeAgg(n int) *probeAgg {
	return &probeAgg{failures: make([]int32, n)}
}

func (a *probeAgg) hit(i int, alive bool, threshold int32) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fired {
		return false
	}
	if threshold <= 0 {
		threshold = 3
	}
	if alive {
		a.failures[i] = 0
		return false
	}
	a.failures[i]++
	if a.failures[i] >= threshold {
		a.fired = true
		return true
	}
	return false
}

func (m *restartMgr) probeLoop(ctx context.Context, sandboxID string, index int, p *cubebox.LivenessProbe, agg *probeAgg) {
	delay := time.Duration(p.GetInitialDelaySecond()) * time.Second
	if delay <= 0 {
		delay = 30 * time.Second
	}
	period := time.Duration(p.GetPeriodSecond()) * time.Second
	if period <= 0 {
		period = 10 * time.Second
	}
	timeout := time.Duration(p.GetProbeTimeoutSecond()) * time.Second
	if timeout <= 0 {
		timeout = time.Second
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	if m.probeTick(ctx, sandboxID, index, p, timeout, agg) {
		return
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.probeTick(ctx, sandboxID, index, p, timeout, agg) {
				return
			}
		}
	}
}

func (m *restartMgr) probeTick(ctx context.Context, sandboxID string, index int, p *cubebox.LivenessProbe, timeout time.Duration, agg *probeAgg) bool {
	if cb, err := m.store.Get(ctx, sandboxID); err == nil && cb != nil && cb.IsPaused() {
		return false
	}
	alive, counted := m.probeOnce(ctx, m.sandboxIP(ctx, sandboxID), p, timeout)
	if !counted {
		return false
	}
	if !agg.hit(index, alive, p.GetFailureThreshold()) {
		return false
	}
	m.OnExit(sandboxID, 0, restartpolicy.ReasonLivenessFailed)
	return true
}

func (m *restartMgr) sandboxIP(ctx context.Context, sandboxID string) string {
	cb, err := m.store.Get(ctx, sandboxID)
	if err != nil || cb == nil {
		return ""
	}
	return cb.IP
}

func (m *restartMgr) probeOnce(ctx context.Context, ip string, p *cubebox.LivenessProbe, timeout time.Duration) (alive bool, counted bool) {
	if ip == "" || net.ParseIP(ip) == nil || p.GetProbeHandler() == nil {
		return false, false
	}
	h := p.GetProbeHandler()
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch {
	case h.GetTcpSocket() != nil:
		return telnet.DoTCPDetect(net.JoinHostPort(ip, fmt.Sprint(h.GetTcpSocket().GetPort())), timeout) == nil, true
	case h.GetPing() != nil:
		return telnet.DoPing(pctx, ip, timeout, h.GetPing().GetUdp()) == nil, true
	case h.GetHttpGet() != nil:
		httpGet := h.GetHttpGet()
		path := httpGet.GetPath()
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "@") || strings.Contains(path, "://") {
			return false, true
		}
		u := formatURL("http", ip, int(httpGet.GetPort()), path)
		u.User = nil
		req, err := http.NewRequestWithContext(pctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return false, true
		}
		res, err := m.httpC.Do(req)
		if err != nil {
			log.G(ctx).Warnf("liveness http %s: %v", u.String(), err)
			return false, true
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 400 {
			log.G(ctx).Warnf("liveness http %s: status %d", u.String(), res.StatusCode)
			return false, true
		}
		return true, true
	default:
		return false, false
	}
}
