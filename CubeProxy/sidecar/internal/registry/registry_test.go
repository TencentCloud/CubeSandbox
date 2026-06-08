// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package registry

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/lifecycle"
)

func TestUpsert_PreservesLastActiveOnReplace(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx", AutoPause: true})
	if !r.MergeLastActive("sbx", 1000) {
		t.Fatal("first MergeLastActive should advance the watermark")
	}
	// Replace meta with a fresh value (e.g. stream replay).
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx", AutoPause: false})
	got := r.Get("sbx")
	if got == nil {
		t.Fatal("entry vanished after re-upsert")
	}
	if got.LastActiveMs != 1000 {
		t.Fatalf("LastActiveMs reset to %d; should have been preserved at 1000", got.LastActiveMs)
	}
	if got.Meta.AutoPause {
		t.Fatal("Meta.AutoPause should reflect latest upsert")
	}
}

func TestMergeLastActive_TakesMaxAndIgnoresUnknown(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})

	if !r.MergeLastActive("sbx", 500) {
		t.Fatal("expected first merge to advance")
	}
	if r.MergeLastActive("sbx", 400) {
		t.Fatal("merge with smaller ts must not advance")
	}
	if !r.MergeLastActive("sbx", 600) {
		t.Fatal("merge with larger ts must advance")
	}
	if got := r.Get("sbx").LastActiveMs; got != 600 {
		t.Fatalf("expected 600, got %d", got)
	}
	// Unknown sandbox: ignored, returns false (not an error).
	if r.MergeLastActive("nope", 9999) {
		t.Fatal("merge for unknown sandbox should return false")
	}
}

func TestSnapshot_StableOrderAndCopy(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "b"})
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "a"})
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "c"})

	snap := r.Snapshot()
	if len(snap) != 3 || snap[0].Meta.SandboxID != "a" || snap[1].Meta.SandboxID != "b" || snap[2].Meta.SandboxID != "c" {
		t.Fatalf("snapshot order wrong: %+v", snap)
	}

	// Snapshot entries are copies — mutating must not bleed into the registry.
	snap[0].LastActiveMs = 7777
	if r.Get("a").LastActiveMs != 0 {
		t.Fatal("snapshot mutation leaked back into registry")
	}
}

func TestFirstSeenAt_StableAcrossUpserts(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	first := r.Get("sbx").FirstSeenAt
	if first.IsZero() {
		t.Fatal("FirstSeenAt should be set on first upsert")
	}
	time.Sleep(2 * time.Millisecond)
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx", AutoPause: true})
	if got := r.Get("sbx").FirstSeenAt; !got.Equal(first) {
		t.Fatalf("FirstSeenAt changed across re-upsert: was %s now %s", first, got)
	}
}

func TestDelete(t *testing.T) {
	r := New()
	r.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	r.Delete("sbx")
	if r.Get("sbx") != nil {
		t.Fatal("entry should be gone after Delete")
	}
	// Delete on absent must not panic.
	r.Delete("sbx")
	r.Delete("never-existed")
}
