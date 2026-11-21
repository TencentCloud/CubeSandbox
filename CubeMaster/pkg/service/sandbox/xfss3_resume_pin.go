// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// ReleaseResumePin lets an xfs-s3 pause binding leave the node its last
// Resume was sent to when that node no longer answers.
//
// A Resume whose outcome is unknown pins the binding to its node, because
// the sandbox may be running there. While the node answers, resuming again
// settles the attempt on that node: Cubelet answers with the sandbox it
// already started, or starts it. Only a node that never comes back keeps a
// binding pinned for good, and only an operator can know that it is down
// and cannot run the sandbox any more. This is that operator's release; it
// refuses while the node answers.
func ReleaseResumePin(ctx context.Context, req *types.ReleaseResumePinRequest) *types.Res {
	rsp := &types.Res{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "sandbox_id is required"
		return rsp
	}
	err := sandboxlock.WithLock(ctx, sandboxID, sandboxlock.Options{
		Value: "release-resume-pin",
		TTL:   sandboxlock.ResumeTTL,
	}, func(ctx context.Context) error {
		ctx = context.WithoutCancel(ctx)
		rec, err := pausesnap.GetBySandbox(ctx, sandboxID)
		if err != nil {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			if errors.Is(err, pausesnap.ErrNotFound) {
				rsp.Ret.RetMsg = fmt.Sprintf("sandbox %s has no pause binding", sandboxID)
			} else {
				rsp.Ret.RetMsg = fmt.Sprintf("load pause binding: %v", err)
			}
			return nil
		}
		target := pausesnap.PinnedResumeTarget(rec)
		if !constants.IsXFSS3Backend(rec.Backend) || target == "" {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = fmt.Sprintf("sandbox %s has no unresolved resume attempt", sandboxID)
			return nil
		}
		holds, herr := targetHoldsLiveSandboxFn(ctx, target, sandboxID)
		if herr == nil {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = fmt.Sprintf("node %s answers (holds a live instance of sandbox %s: %v); resume the sandbox again to settle the attempt there, or delete it",
				target, sandboxID, holds)
			return nil
		}
		attempt := &pausesnap.ResumeAttempt{Target: target, N: rec.ResumeAttempt}
		if err := failResumeAttemptFn(ctx, rec.SnapshotID, attempt); err != nil {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_DBError)
			rsp.Ret.RetMsg = fmt.Sprintf("release resume attempt %d of %s: %v", attempt.N, rec.SnapshotID, err)
			return nil
		}
		log.G(ctx).Warnf("resume: operator released attempt %d of %s on %s, which does not answer: %v",
			attempt.N, rec.SnapshotID, target, herr)
		rsp.Ret.RetMsg = fmt.Sprintf("released resume attempt %d on %s, which does not answer (%v); the next Resume may choose another node",
			attempt.N, target, herr)
		return nil
	})
	if err != nil {
		applyLifecycleLockError(rsp, err)
	}
	return rsp
}
