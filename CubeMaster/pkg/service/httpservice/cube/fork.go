// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// Max concurrent derivations per fork request, capping the template-resolve
// DB fan-out that each derivation performs before entering the scheduler queue.
const forkCreateConcurrencyLimit = 8

// forkOverallTimeout bounds a whole fork request: snapshot budget plus a
// derive fan-out budget.
var forkOverallTimeout = templatecenter.SnapshotOperationTimeout() + 10*time.Minute

// Post-fork bookkeeping budgets, independent of an exhausted forkOverallTimeout.
const (
	forkSnapshotReclaimTimeout = 240 * time.Second
	forkRefRegisterTimeout     = 30 * time.Second
)

var (
	forkDealCubeboxCreateReqWithTemplateFn = dealCubeboxCreateReqWithTemplate
	forkCreateSandboxRunFn                 = sandbox.CreateSandbox
)

func extendForkWriteDeadline(w http.ResponseWriter) {
	extendWriteDeadline(w, forkOverallTimeout+snapshotResponseWriteDeadlineBuffer)
}

// forkSandboxResult is the outcome of one requested fork.
type forkSandboxResult struct {
	Sandbox *types.CreateCubeSandboxRes `json:"sandbox,omitempty"`
	Ret     *types.Ret                  `json:"ret,omitempty"`
}

// forkSandboxResponse wraps the per-fork results.
type forkSandboxResponse struct {
	*types.Res
	Results []*forkSandboxResult `json:"results"`
}

// forkSandboxFromSnapshot derives `count` sandboxes from one snapshot with
// bounded concurrency. `envVars` is inherited from the source, read-only across goroutines.
func forkSandboxFromSnapshot(
	ctx context.Context,
	requestID string,
	snapshotID string,
	count int,
	timeout *int,
	envVars map[string]string,
) []*forkSandboxResult {
	results := make([]*forkSandboxResult, count)
	var wg sync.WaitGroup
	// Each derivation uses its own sub-ctx (avoids cross-goroutine replica
	// overwrite); each writes its own results index, so no lock is needed.
	sem := make(chan struct{}, forkCreateConcurrencyLimit)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			subCtx := withTemplateResolveResult(ctx, &templateResolveResult{})
			sb, ret := singleForkDerive(subCtx, requestID, snapshotID, timeout, envVars)
			results[idx] = &forkSandboxResult{Sandbox: sb, Ret: ret}
		}(i)
	}
	wg.Wait()
	return results
}

// singleForkDerive derives one sandbox from `snapshotID`, returning its
// response or a business Ret on failure. `subCtx` carries its own templateResolveResult.
func singleForkDerive(
	subCtx context.Context,
	requestID string,
	snapshotID string,
	timeout *int,
	envVars map[string]string,
) (*types.CreateCubeSandboxRes, *types.Ret) {
	req, err := buildForkCreateReq(requestID, snapshotID, timeout, envVars)
	if err != nil {
		return nil, forkRet(errorcode.ErrorCode_MasterParamsError, err.Error())
	}

	if err := forkDealCubeboxCreateReqWithTemplateFn(subCtx, req); err != nil {
		return nil, forkRet(errorcode.ErrorCode_MasterParamsError, err.Error())
	}
	// Mirror create: seed node affinity into the per-derivation ctx.
	subCtx, err = runInsReq2Affinity(subCtx, req)
	if err != nil {
		return nil, forkRet(errorcode.ErrorCode_MasterParamsError, err.Error())
	}
	ret := forkCreateSandboxRunFn(subCtx, req)
	if ret == nil {
		return nil, forkRet(errorcode.ErrorCode_MasterInternalError, "create returned nil")
	}
	if ret.Ret != nil && ret.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		return nil, ret.Ret
	}
	// Register the runtime ref (warn-and-keep on failure, as in createSandbox).
	// The budget is independent of the fork deadline so an edge-finishing
	// derivation still registers its ref.
	refCtx, refCancel := context.WithTimeout(context.WithoutCancel(subCtx), forkRefRegisterTimeout)
	defer refCancel()
	if err := registerForkRuntimeRef(refCtx, snapshotID, ret); err != nil {
		CubeLog.WithContext(refCtx).Warnf("register fork runtime ref failed: %v", err)
	}
	return ret, nil
}

// buildForkCreateReq builds a from-snapshot create request via
// constructCreateReq, replaying source env vars since the template merge
// path does not carry CreateTimeEnvVars.
func buildForkCreateReq(
	requestID string,
	snapshotID string,
	timeout *int,
	envVars map[string]string,
) (*types.CreateCubeSandboxReq, error) {
	// Mark as v2 snapshot create to route via template-center, matching
	// snapshot_ops.go's from-snapshot spawn (avoids the local-config path).
	payload := map[string]any{
		"requestID": requestID,
		"annotations": map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID: snapshotID,
			constants.CubeAnnotationAppSnapshotVersion:    templatecenter.DefaultTemplateVersion,
		},
	}
	if len(envVars) > 0 {
		payload["create_time_env_vars"] = envVars
	}
	if timeout != nil {
		payload["timeout"] = *timeout
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "http://fork.internal/fork", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(constants.Caller, "fork")
	return constructCreateReq(httpReq)
}

// registerForkRuntimeRef registers a derived sandbox's runtime ref against the
// snapshot, preferring the replica the template-resolve phase already chose.
func registerForkRuntimeRef(ctx context.Context, snapshotID string, ret *types.CreateCubeSandboxRes) error {
	resolved := templateResolveResultFromContext(ctx)
	if resolved != nil && resolved.HasChosenReplica {
		return templatecenter.RegisterSnapshotRuntimeRefForCreatedSandboxWithReplica(
			ctx, snapshotID, ret.SandboxID, ret.HostID, ret.HostIP, resolved.ChosenReplica)
	}
	return templatecenter.RegisterSnapshotRuntimeRefForCreatedSandbox(
		ctx, snapshotID, ret.SandboxID, ret.HostID, ret.HostIP)
}

func forkRet(code errorcode.ErrorCode, msg string) *types.Ret {
	return &types.Ret{RetCode: int(code), RetMsg: msg}
}

// forkSandboxFromSourceRequest is the request body to fork a running sandbox
// into N copies: CubeMaster snapshots the source once, then derives N sandboxes
// from that snapshot.
type forkSandboxFromSourceRequest struct {
	RequestID       string `json:"requestID,omitempty"`
	LegacyRequestID string `json:"request_id,omitempty"`
	Count           int    `json:"count,omitempty"`
	Timeout         *int   `json:"timeout,omitempty"`
}

func handleForkSandboxFromSandboxAction(c *gin.Context) {
	extendForkWriteDeadline(c.Writer)
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &forkSandboxFromSourceRequest{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, &forkSandboxResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			}},
		})
		return
	}
	requestID := firstNonEmptyTrimmed(req.RequestID, req.LegacyRequestID)
	sandboxID := strings.TrimSpace(c.Param("sandbox_id"))
	if requestID == "" || sandboxID == "" {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, &forkSandboxResponse{
			Res: &types.Res{RequestID: requestID, Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "request_id and sandbox_id are required",
			}},
		})
		return
	}
	count := req.Count
	if count < 1 || count > 100 {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, &forkSandboxResponse{
			Res: &types.Res{RequestID: requestID, Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "count must be between 1 and 100",
			}},
		})
		return
	}

	// The middleware always injects a trace; carry it into the detached ctx.
	ctx := CubeLog.WithRequestTrace(context.Background(), rt.DeepCopy())
	// Bound the whole fork; stalled derivations degrade to per-fork errors.
	ctx, cancel := context.WithTimeout(ctx, forkOverallTimeout)
	defer cancel()
	resp := &forkSandboxResponse{
		Res: &types.Res{
			RequestID: requestID,
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
	}
	if err := forkSandboxFromSource(ctx, requestID, sandboxID, count, req.Timeout, resp); err != nil {
		code := snapshotErrorCode(err)
		resp.Res.Ret = &types.Ret{RetCode: int(code), RetMsg: err.Error()}
		resp.Results = nil
		rt.RetCode = int64(code)
		common.WriteAPI(c, resp)
		return
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, resp)
}

// forkSandboxFromSource snapshots the source once, derives `count` sandboxes,
// and writes results into `resp`. The temp snapshot is reclaimed async.
func forkSandboxFromSource(
	ctx context.Context,
	requestID string,
	sandboxID string,
	count int,
	timeout *int,
	resp *forkSandboxResponse,
) error {
	hostID, hostIP, err := resolveSnapshotHostFn(ctx, requestID, sandboxID)
	if err != nil {
		return err
	}
	jobInfo, err := createSnapshotFn(ctx, requestID, sandboxID, hostID, hostIP, "", "")
	if err != nil {
		return err
	}
	snapshotID := strings.TrimSpace(jobInfo.TemplateID)
	if snapshotID == "" {
		return errors.New("snapshot create returned empty snapshot id")
	}

	// Replay source env vars from the snapshot spec (template merge omits them).
	envVars := inheritedForkEnvVars(ctx, snapshotID)
	resp.Results = forkSandboxFromSnapshot(ctx, requestID, snapshotID, count, timeout, envVars)

	// Reclaim off the response path: a synchronous delete could push the
	// response past its write deadline.
	go func() {
		// Fresh ctx/requestID: the create requestID trips DeleteSnapshot's guard.
		reclaimCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), forkSnapshotReclaimTimeout)
		defer cancel()
		if _, err := deleteSnapshotFn(reclaimCtx, requestID+"-reclaim", snapshotID, ""); err != nil {
			CubeLog.WithContext(reclaimCtx).Warnf("delete temp fork snapshot %s failed: %v", snapshotID, err)
		}
	}()
	return nil
}

// inheritedForkEnvVars returns the source sandbox env vars stored in the
// snapshot spec; nil on read failure so a fork never fails on a spec lookup.
func inheritedForkEnvVars(ctx context.Context, snapshotID string) map[string]string {
	tpl, err := templatecenter.GetTemplateRequest(ctx, snapshotID)
	if err != nil {
		CubeLog.WithContext(ctx).Warnf("read fork snapshot spec %s env vars failed: %v", snapshotID, err)
		return nil
	}
	if tpl == nil || len(tpl.CreateTimeEnvVars) == 0 {
		return nil
	}
	return maps.Clone(tpl.CreateTimeEnvVars)
}
