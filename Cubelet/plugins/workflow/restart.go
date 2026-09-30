// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

// RestartPrior is the bookkeeping copied onto the CubeBox built by a restart.
type RestartPrior struct {
	RestartCount            int32
	LastRestartAt           int64
	LastSuccessfulRestartAt int64
	LastFailedRestartAt     int64
	LastExitCode            int32
	LastExitReason          string
	// StatusSeq is the last value reported to CubeMaster. A rebuilt CubeBox
	// continues from it so a newer report is not dropped as stale.
	StatusSeq       int64
	OriginalRequest []byte
}

// Retain is what a destroy must keep. The zero value keeps nothing.
// Restart is the only destroy that keeps anything; every plugin asks Retain
// instead of growing its own restart condition.
type Retain struct {
	Storage     bool
	Network     bool
	Volume      bool
	CubeboxMeta bool
}

// RetainFor is the one decision of what a restart-destroy keeps.
func RetainFor(opts *DestroyContext) Retain {
	if opts == nil || !opts.IsRestartDestroy {
		return Retain{}
	}
	return restartRetain()
}

// ReuseFor is the one decision of what a restart-create reuses.
// It is the create-side pair of RetainFor.
func ReuseFor(opts *CreateContext) Retain {
	if opts == nil || !opts.IsRestart {
		return Retain{}
	}
	return restartRetain()
}

func restartRetain() Retain {
	return Retain{Storage: true, Network: true, Volume: true, CubeboxMeta: true}
}
