// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"errors"
	"strings"
	"testing"
)

// TestDistributionFailureMatrix pins the decision that turns a distribution
// outcome into a terminal error.
//
// The interesting property is that there is NO minimum node count: a
// single-node deployment is legal, so 1 ready of 1 expected must succeed. What
// used to be wrong is the opposite end — the guard read
// `expected > 0 && ready == 0`, so "distribution never reached any node"
// (expected == 0) fell straight through it and the job went on to write a
// template_definition for a template that could not have a single replica.
func TestDistributionFailureMatrix(t *testing.T) {
	distErr := errors.New("cubelet refused the artifact")

	tests := []struct {
		name        string
		expected    int32
		ready       int32
		distErr     error
		wantFailure bool
		wantSubstr  string
	}{
		// Success: any ready node at all is enough to keep going.
		{name: "single-node-all-ready", expected: 1, ready: 1},
		{name: "single-node-ready-despite-error", expected: 1, ready: 1, distErr: distErr},
		{name: "multi-node-all-ready", expected: 3, ready: 3},
		// Partial success must NOT be terminal: it becomes PARTIALLY_READY and
		// stays retryable through `redo --failed-only`.
		{name: "multi-node-partially-ready", expected: 3, ready: 1, distErr: distErr},
		{name: "multi-node-one-short", expected: 3, ready: 2, distErr: distErr},

		// Terminal: every node was tried and every node failed.
		{
			name: "single-node-failed", expected: 1, ready: 0, distErr: distErr,
			wantFailure: true, wantSubstr: "failed on all 1 node(s)",
		},
		{
			name: "multi-node-all-failed", expected: 3, ready: 0, distErr: distErr,
			wantFailure: true, wantSubstr: "failed on all 3 node(s)",
		},

		// Terminal: distribution never reached a node. This is the case the old
		// guard let through.
		{
			name: "no-node-reached", expected: 0, ready: 0, distErr: ErrNoTemplateNodes,
			wantFailure: true, wantSubstr: "did not reach any node",
		},
		{
			name: "no-node-reached-scope-mismatch", expected: 0, ready: 0, distErr: distErr,
			wantFailure: true, wantSubstr: "did not reach any node",
		},
		// Defensive: a caller returning zero targets without an error must still
		// produce a usable message rather than "<nil>".
		{
			name: "no-node-reached-without-error", expected: 0, ready: 0, distErr: nil,
			wantFailure: true, wantSubstr: "did not reach any node",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := distributionFailure(tc.expected, tc.ready, tc.distErr)
			if !tc.wantFailure {
				if err != nil {
					t.Fatalf("distributionFailure(expected=%d, ready=%d) = %v, want nil",
						tc.expected, tc.ready, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("distributionFailure(expected=%d, ready=%d) = nil, want an error",
					tc.expected, tc.ready)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("distributionFailure() = %q, want it to contain %q", err, tc.wantSubstr)
			}
			// The node counts must survive into the message: it is written to
			// job.error_message and is the only diagnosis left once cleanup has
			// deleted the per-node replica rows.
			if tc.expected > 0 && !strings.Contains(err.Error(), "node(s)") {
				t.Fatalf("distributionFailure() = %q, want it to report the node count", err)
			}
		})
	}
}

// TestDistributionFailureWrapsNoNodesError checks the expected==0 branch keeps
// the cause matchable. Callers (and the reconciler) distinguish "no capacity"
// from "the nodes rejected it", so the sentinel must not be flattened into a
// string.
func TestDistributionFailureWrapsNoNodesError(t *testing.T) {
	err := distributionFailure(0, 0, ErrNoTemplateNodes)
	if !errors.Is(err, ErrNoTemplateNodes) {
		t.Fatalf("distributionFailure() = %v, want it to wrap ErrNoTemplateNodes", err)
	}

	// The expected>0 branch intentionally uses %v, because distErr there is an
	// aggregate of per-node failures rather than a single matchable cause.
	if errors.Is(distributionFailure(2, 0, ErrNoTemplateNodes), ErrNoTemplateNodes) {
		t.Fatalf("the all-nodes-failed branch should not claim ErrNoTemplateNodes")
	}
}

// TestArtifactStatusReusableForRedo pins which artifact states a redo may
// reuse instead of rebuilding.
//
// Getting this wrong is expensive in both directions: reusing a PENDING or
// BUILDING artifact reads a half-written ext4, while refusing to reuse a READY
// one rebuilds an image needlessly. And a job that failed distribution on every
// node records phase=DISTRIBUTING even though the cleanup already deleted its
// artifact, which is what made such templates permanently unretryable.
func TestArtifactStatusReusableForRedo(t *testing.T) {
	tests := []struct {
		name         string
		status       string
		wantReusable bool
	}{
		// The only reusable state: the ext4 exists and is complete.
		{name: "ready", status: ArtifactStatusReady, wantReusable: true},
		{name: "ready-lowercase", status: "ready", wantReusable: true},
		{name: "ready-mixed-case", status: "Ready", wantReusable: true},
		{name: "ready-padded", status: "  READY  ", wantReusable: true},

		// Someone else's in-flight build: reusing it reads a partial file.
		{name: "pending", status: ArtifactStatusPending},
		{name: "building", status: ArtifactStatusBuilding},

		// Files are gone or going.
		{name: "failed", status: ArtifactStatusFailed},
		{name: "cleanup-pending", status: ArtifactStatusCleanupPending},

		// Absent/unknown states must rebuild rather than assume.
		{name: "empty", status: ""},
		{name: "whitespace", status: "   "},
		{name: "unknown-future-state", status: "ARCHIVED"},
		{name: "not-ready-substring", status: "NOT_READY"},
		{name: "ready-prefixed", status: "READY_TO_DELETE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := artifactStatusReusableForRedo(tc.status); got != tc.wantReusable {
				t.Fatalf("artifactStatusReusableForRedo(%q) = %v, want %v",
					tc.status, got, tc.wantReusable)
			}
		})
	}
}
