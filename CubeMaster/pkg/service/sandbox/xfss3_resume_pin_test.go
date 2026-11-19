// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// withPinnedBinding serves rec for every lookup, runs the lifecycle lock
// inline and records released attempts.
func withPinnedBinding(t *testing.T, rec *pausesnap.Record, holds bool, holdsErr error) *[]pausesnap.ResumeAttempt {
	t.Helper()
	var released []pausesnap.ResumeAttempt
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		if rec == nil {
			return nil, pausesnap.ErrNotFound
		}
		return rec, nil
	})
	patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	prevHolds, prevFail := targetHoldsLiveSandboxFn, failResumeAttemptFn
	t.Cleanup(func() { targetHoldsLiveSandboxFn, failResumeAttemptFn = prevHolds, prevFail })
	targetHoldsLiveSandboxFn = func(context.Context, string, string) (bool, error) { return holds, holdsErr }
	failResumeAttemptFn = func(_ context.Context, _ string, a *pausesnap.ResumeAttempt) error {
		released = append(released, *a)
		return nil
	}
	return &released
}

func releasePin() *sandboxtypes.Res {
	return ReleaseResumePin(context.Background(), &sandboxtypes.ReleaseResumePinRequest{RequestID: "r", SandboxID: "sb-x"})
}

func TestReleaseResumePinRefusesWhileTheNodeAnswers(t *testing.T) {
	rec := xfss3Binding("10.0.0.2", pausesnap.ResumeInflight)
	for _, holds := range []bool{true, false} {
		released := withPinnedBinding(t, rec, holds, nil)
		rsp := releasePin()
		require.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
		require.Contains(t, rsp.Ret.RetMsg, "resume the sandbox again")
		require.Empty(t, *released, "a node that answers settles the attempt itself (holds=%v)", holds)
	}
}

func TestReleaseResumePinReleasesTheAttemptOfANodeThatIsGone(t *testing.T) {
	rec := xfss3Binding("10.0.0.2", pausesnap.ResumeInflight)
	rec.ResumeAttempt = 4
	released := withPinnedBinding(t, rec, false, errors.New("connection refused"))
	rsp := releasePin()
	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode, rsp.Ret.RetMsg)
	require.Equal(t, []pausesnap.ResumeAttempt{{Target: "10.0.0.2", N: 4}}, *released,
		"only the attempt that was read is released")
}

func TestReleaseResumePinNeedsAnUnresolvedAttempt(t *testing.T) {
	for name, rec := range map[string]*pausesnap.Record{
		"no binding":    nil,
		"not pinned":    xfss3Binding("10.0.0.2", pausesnap.ResumeFailed),
		"other backend": {SandboxID: "sb-x", SnapshotID: "snap-x", Backend: "xfs", ResumeTarget: "10.0.0.2", ResumeState: pausesnap.ResumeInflight},
		"never resumed": xfss3Binding("", ""),
	} {
		released := withPinnedBinding(t, rec, false, errors.New("unreachable"))
		rsp := releasePin()
		require.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode, name)
		require.Empty(t, *released, name)
	}
}
