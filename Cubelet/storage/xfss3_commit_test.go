// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

func TestXFSS3CommitDecisionExportsOnlyOnADefiniteMiss(t *testing.T) {
	own := &checkpoint.Candidate{Session: "s1"}
	other := &checkpoint.Candidate{Session: "s0"}

	if done, err := xfss3CommitDecision(own, nil, "s1"); !done || err != nil {
		t.Fatalf("own commit: done=%v err=%v, want done", done, err)
	}
	if done, err := xfss3CommitDecision(nil, checkpoint.ErrNoCandidate, "s1"); done || err != nil {
		t.Fatalf("no candidate: done=%v err=%v, want export", done, err)
	}
	// An unreadable store may hide a verified commit: retry, never export.
	done, err := xfss3CommitDecision(nil, errors.New("http 503"), "s1")
	if done || err == nil || isXFSS3Permanent(err) {
		t.Fatalf("transient lookup failure: done=%v err=%v, want a retryable error", done, err)
	}
	// Another session's commit is never replaced.
	if _, err := xfss3CommitDecision(other, nil, "s1"); !isXFSS3Permanent(err) || !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("foreign commit: want a permanent conflict, got %v", err)
	}
	if _, err := xfss3CommitDecision(nil, checkpoint.ErrCorrupt, "s1"); !isXFSS3Permanent(err) {
		t.Fatalf("corrupt candidate: want permanent, got %v", err)
	}
}
