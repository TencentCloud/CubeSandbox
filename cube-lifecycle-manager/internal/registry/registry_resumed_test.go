// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Tests for the MarkResumed API used by issue #1683's sweeper grace gate.

package registry

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

func TestMarkResumed_AdvancesForward(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1"})

	if !r.MarkResumed("sb-1", 1000) {
		t.Fatal("first MarkResumed should report advancement")
	}
	if got := r.Get("sb-1"); got == nil || got.ResumedAtMs != 1000 {
		t.Fatalf("ResumedAtMs = %d, want 1000", got.ResumedAtMs)
	}
}

func TestMarkResumed_IgnoresBackwards(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1"})

	r.MarkResumed("sb-1", 5000)
	if r.MarkResumed("sb-1", 4000) {
		t.Fatal("MarkResumed with a smaller tsMs must not report advancement")
	}
	if got := r.Get("sb-1").ResumedAtMs; got != 5000 {
		t.Fatalf("ResumedAtMs = %d, want 5000 (backwards writes ignored)", got)
	}
}

func TestMarkResumed_UnknownSandbox(t *testing.T) {
	r := New()
	if r.MarkResumed("never-seen", 1000) {
		t.Fatal("MarkResumed on unknown sandbox must report no-op")
	}
}

func TestMarkResumed_SurvivesUpsert(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1", AutoPause: true})
	r.MarkResumed("sb-1", 7777)

	// Stream replay replaces the meta; ResumedAtMs must not be wiped.
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1", AutoPause: false})
	got := r.Get("sb-1")
	if got == nil || got.ResumedAtMs != 7777 {
		t.Fatalf("ResumedAtMs after Upsert = %d, want 7777", got.ResumedAtMs)
	}
}
