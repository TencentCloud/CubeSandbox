// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"sort"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/protobuf/proto"
)

// Restart rebuilds a sandbox in place: destroy keeps disk, network, volume
// metadata and the cubebox row; create reuses the sandbox ID and cold-boots.
// Callers must have already decided that the policy allows a restart.
func (s *service) Restart(ctx context.Context, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
	rsp := &cubebox.RunCubeSandboxResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if req == nil || req.GetAnnotations() == nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "restart requires the original create request"
		return rsp, nil
	}
	sandboxID := req.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID]
	if sandboxID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "restart requires cube.master.desired.sandbox.id"
		return rsp, nil
	}
	rsp.SandboxID = sandboxID

	unlock, lockErr := s.sandboxLifecycleLocks.LockContext(ctx, sandboxID)
	if lockErr != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		rsp.Ret.RetMsg = "sandbox lifecycle operation is in progress"
		return rsp, nil
	}
	defer unlock()

	old, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil || old == nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "sandbox is not found"
		return rsp, nil
	}
	if old.IsPaused() {
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		rsp.Ret.RetMsg = "sandbox is paused"
		return rsp, nil
	}
	if err := checkParam(ctx, req); err != nil {
		rerr, _ := ret.FromError(err)
		rsp.Ret.RetMsg = rerr.Message()
		rsp.Ret.RetCode = rerr.Code()
		s.noteRestartFailure(sandboxID, old.RestartCount)
		return rsp, nil
	}

	ids := savedContainerIDs(old)
	prior := restartPriorFrom(old)
	template := old.LocalRunTemplate
	oldCount := old.RestartCount

	ns := req.GetNamespace()
	if ns == "" {
		ns = old.Namespace
	}
	if ns == "" {
		ns = namespaces.Default
	}
	// Create decides the cube vs OCI path from this flag. Service.Create sets
	// it before engine.Create; Restart calls the engine directly and must too.
	// Without it, cold start takes the OCI snapshot path and panics on a nil image.
	runtime, rtErr := s.cubeboxMgr.getSandboxRuntime(req)
	if rtErr != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = rtErr.Error()
		s.noteRestartFailure(sandboxID, oldCount)
		return rsp, nil
	}
	base := constants.WithRuntimeType(namespaces.WithNamespace(ctx, ns), runtime.Type)
	// Same bound as user delete, applied separately to Destroy and Create.
	// Destroy can spend the whole window in shim Wait; Create still needs its
	// own window to cold-start. The restart loop calls in with a bare context.
	deadline := defaultDestroyDeadline
	if s.config != nil && s.config.destroyDeadline > 0 {
		deadline = s.config.destroyDeadline
	}
	destroyCtx, destroyCancel := context.WithTimeout(base, deadline)
	defer destroyCancel()
	// Destroy and Create both run parallel steps that require a trace.
	rt := restartRequestTrace(req, sandboxID, s.engine.ID(), "Destroy")
	destroyCtx = CubeLog.WithRequestTrace(destroyCtx, rt)

	destroyInfo := &workflow.DestroyContext{
		IsRestartDestroy: true,
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			RequestID: req.GetRequestID(),
			SandboxID: sandboxID,
		},
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
	}
	destroyCtx = context.WithValue(destroyCtx, workflow.KDestroyContext, destroyInfo)
	if err := s.engine.Destroy(destroyCtx, destroyInfo); err != nil {
		// Destroy did not finish. Cold start on a half-released sandbox leaves
		// the guest health port closed, and that failure must not consume
		// maxRestarts. noteRestartFailure stays on parameter errors above and
		// on Create failures after a finished destroy.
		rerr, _ := ret.FromError(err)
		rsp.Ret.RetMsg = rerr.Message()
		rsp.Ret.RetCode = rerr.Code()
		log.G(base).Warnf("restart destroy %s failed: %v", sandboxID, err)
		return rsp, nil
	}

	// Failover stays off. A failed cold start must not start engine.failover:
	// that Destroy runs beside the restart loop, and the loop is the only
	// owner of the next destroy and create. Normal Create still sets Failover.
	createInfo := &workflow.CreateContext{
		ReqInfo:           proto.Clone(req).(*cubebox.RunCubeSandboxRequest),
		Failover:          false,
		IsRestart:         true,
		SavedContainerIDs: ids,
		RestartPrior:      prior,
		LocalRunTemplate:  template,
		BaseWorkflowInfo:  workflow.BaseWorkflowInfo{SandboxID: sandboxID},
	}
	createCtx, createCancel := context.WithTimeout(base, deadline)
	defer createCancel()
	createCtx = workflow.WithCreateContext(createCtx, createInfo)
	createCtx = context.WithValue(createCtx, workflow.KCreateContext, createInfo)
	// Destroy's parallel steps already DeepCopy'd the trace.
	rt.CalleeAction = "Create"
	createCtx = CubeLog.WithRequestTrace(createCtx, rt)
	if err := s.engine.Create(createCtx, createInfo); err != nil {
		rerr, _ := ret.FromError(err)
		rsp.Ret.RetMsg = rerr.Message()
		rsp.Ret.RetCode = rerr.Code()
		// Destroy already finished. A guest that did not become ready used
		// this restart attempt.
		s.noteRestartFailure(sandboxID, oldCount)
		return rsp, nil
	}
	s.noteRestartSuccess(sandboxID)
	log.G(ctx).Infof("restarted sandbox %s", sandboxID)
	return rsp, nil
}

func restartRequestTrace(req *cubebox.RunCubeSandboxRequest, sandboxID, callee, action string) *CubeLog.RequestTrace {
	requestID := ""
	if req != nil {
		requestID = req.GetRequestID()
	}
	return &CubeLog.RequestTrace{
		Action:       "Restart",
		RequestID:    requestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       callee,
		CalleeAction: action,
		InstanceID:   sandboxID,
	}
}

func applyRestartBookkeeping(box *cubeboxstore.CubeBox, p *workflow.RestartPrior, increment bool) {
	if box == nil || p == nil {
		return
	}
	box.StatusSeq = p.StatusSeq
	box.LastExitCode = p.LastExitCode
	box.LastExitReason = p.LastExitReason
	box.LastSuccessfulRestartAt = p.LastSuccessfulRestartAt
	box.LastFailedRestartAt = p.LastFailedRestartAt
	if len(p.OriginalRequest) > 0 {
		box.OriginalRequest = append([]byte(nil), p.OriginalRequest...)
	}
	if increment {
		box.RestartCount = p.RestartCount + 1
		box.LastRestartAt = time.Now().UnixNano()
		box.RestartState = restartpolicy.StateRestarting
		return
	}
	box.RestartCount = p.RestartCount
	box.LastRestartAt = p.LastRestartAt
	box.RestartState = restartpolicy.StateRunning
}

// clearRestartCountForResume applies the pause/resume rule: coming back from
// pause is not a restart, the policy stays, and the restart streak is forgotten.
func clearRestartCountForResume(box *cubeboxstore.CubeBox) {
	if box == nil {
		return
	}
	box.RestartCount = 0
	box.RestartState = restartpolicy.StateRunning
}

// restartTransition is one write of the restart fields on a CubeBox.
// Callers lock and save; this function only assigns fields.
type restartTransition struct {
	state         string
	setState      bool
	nextRestartAt int64
	setNext       bool
	exitCode      int32
	setExit       bool
	reason        string
	setReason     bool
	incrementFrom int32
	increment     bool
	markFailed    bool
	markSuccess   bool
	bumpSeq       bool
	resetCount    bool
	now           int64
}

func applyRestartTransition(box *cubeboxstore.CubeBox, t restartTransition) {
	if box == nil {
		return
	}
	if t.resetCount {
		box.RestartCount = 0
	}
	if t.increment && box.RestartCount <= t.incrementFrom {
		box.RestartCount = t.incrementFrom + 1
		box.LastRestartAt = t.now
	}
	if t.setExit {
		box.LastExitCode = t.exitCode
	}
	if t.setReason {
		box.LastExitReason = t.reason
	}
	if t.markFailed {
		box.LastFailedRestartAt = t.now
	}
	if t.markSuccess {
		box.LastSuccessfulRestartAt = t.now
	}
	if t.setState {
		box.RestartState = t.state
	}
	if t.setNext {
		box.NextRestartAt = t.nextRestartAt
	}
	if t.bumpSeq {
		box.StatusSeq++
	}
}

func restartPriorFrom(old *cubeboxstore.CubeBox) *workflow.RestartPrior {
	if old == nil {
		return nil
	}
	return &workflow.RestartPrior{
		RestartCount:            old.RestartCount,
		LastRestartAt:           old.LastRestartAt,
		LastSuccessfulRestartAt: old.LastSuccessfulRestartAt,
		LastFailedRestartAt:     old.LastFailedRestartAt,
		LastExitCode:            old.LastExitCode,
		LastExitReason:          old.LastExitReason,
		StatusSeq:               old.StatusSeq,
		OriginalRequest:         append([]byte(nil), old.OriginalRequest...),
	}
}

func (s *service) noteRestartFailure(sandboxID string, oldCount int32) {
	cb, err := s.cubeboxMgr.cubeboxManger.Get(context.Background(), sandboxID)
	if err != nil || cb == nil {
		return
	}
	cb.Lock()
	defer cb.Unlock()
	now := time.Now().UnixNano()
	applyRestartTransition(cb, restartTransition{
		increment:     true,
		incrementFrom: oldCount,
		now:           now,
		markFailed:    true,
		setState:      true,
		state:         restartpolicy.StateBackOff,
	})
	if err := s.cubeboxMgr.cubeboxManger.Save(context.Background(), cb); err != nil {
		log.G(context.Background()).Warnf("restart: save failure for %s: %v", sandboxID, err)
	}
}

func (s *service) noteRestartSuccess(sandboxID string) {
	cb, err := s.cubeboxMgr.cubeboxManger.Get(context.Background(), sandboxID)
	if err != nil || cb == nil {
		return
	}
	cb.Lock()
	defer cb.Unlock()
	applyRestartTransition(cb, restartTransition{
		markSuccess:   true,
		now:           time.Now().UnixNano(),
		setState:      true,
		state:         restartpolicy.StateRunning,
		setNext:       true,
		nextRestartAt: 0,
	})
	if err := s.cubeboxMgr.cubeboxManger.Save(context.Background(), cb); err != nil {
		log.G(context.Background()).Warnf("restart: save success for %s: %v", sandboxID, err)
	}
}

// holdProbes stops probes for the duration of an operation that freezes the
// guest (commit, rollback) and starts them again when it returns.
// A restart already in progress is left alone: cancelling it would drop the sandbox.
func (s *service) holdProbes(id string) func() {
	if s == nil || s.restarts == nil || id == "" || !s.restarts.Suspend(id) {
		return func() {}
	}
	return func() { s.restarts.Resume(id) }
}

func savedContainerIDs(cb *cubeboxstore.CubeBox) map[int]string {
	out := map[int]string{}
	if cb == nil {
		return out
	}
	out[0] = cb.ID
	var rest []*cubeboxstore.Container
	for _, c := range cb.AllContainers() {
		if c == nil || c.IsPod || c.ID == cb.ID {
			continue
		}
		rest = append(rest, c)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].CreatedAt < rest[j].CreatedAt })
	for i, c := range rest {
		out[i+1] = c.ID
	}
	return out
}
