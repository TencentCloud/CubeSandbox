// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func trialFailure(stage string) xfss3VerifyOutcome {
	return xfss3VerifyOutcome{state: cow.RemoteStatePending, msg: trialRetryPrefix(stage) + "trial failed", backoff: true, trialStage: stage}
}

func TestARestoreFailureHaltsThisNodeOnly(t *testing.T) {
	ver := &xfss3Verify{}
	ver.noteVerifyOutcome(trialFailure("restore"))
	if !ver.halted || !strings.HasPrefix(ver.message, "retry(restore)") {
		t.Fatalf("halted=%v message=%q", ver.halted, ver.message)
	}
	if ver.state != cow.RemoteStatePending {
		t.Fatalf("one node failing to restore must not fail the checkpoint: %s", ver.state)
	}

	// A halted node keeps answering without starting another trial.
	const snap = "snap-halted00000000000000001"
	xfss3Verifies.Store(snap, ver)
	t.Cleanup(func() { xfss3Verifies.Delete(snap) })
	st := XFSS3CheckpointStatus(context.Background(), snap)
	if st.State != cow.RemoteStatePending || !strings.HasPrefix(st.Message, "retry(restore)") || ver.running {
		t.Fatalf("status %+v running=%v", st, ver.running)
	}
}

func TestTrialRetriesAreCapped(t *testing.T) {
	ver := &xfss3Verify{}
	for i := 1; i < xfss3MaxTrials; i++ {
		ver.noteVerifyOutcome(trialFailure("agent"))
		if ver.halted {
			t.Fatalf("halted after %d trials", i)
		}
	}
	// Failed downloads cost no trial: the store, not this node, is at fault.
	ver.noteVerifyOutcome(xfss3VerifyOutcome{state: cow.RemoteStatePending, msg: "retry(download): unreachable", backoff: true})
	if ver.halted {
		t.Fatal("a failed download must not count as a trial")
	}
	ver.noteVerifyOutcome(trialFailure("infra"))
	if !ver.halted || !strings.HasPrefix(ver.message, "retry(exhausted)") {
		t.Fatalf("halted=%v message=%q", ver.halted, ver.message)
	}
}

func TestPassedOrDefectiveTrialsAreNotCounted(t *testing.T) {
	ver := &xfss3Verify{}
	ver.noteVerifyOutcome(xfss3VerifyOutcome{state: cow.RemoteStateFailed, msg: "corrupt"})
	if ver.halted || ver.trials != 0 {
		t.Fatalf("a terminal outcome is not a retry: %+v", ver)
	}
}
