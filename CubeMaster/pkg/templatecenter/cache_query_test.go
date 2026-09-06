// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"
	"time"
)

// ---- templateQueryCacheTTL ----

func TestTemplateQueryCacheTTL(t *testing.T) {
	// Must be positive and non-zero so go-cache behaves correctly. Pin to the
	// documented 1 minute so an accidental change to 0/negative is caught.
	if got := templateQueryCacheTTL(); got != time.Minute {
		t.Fatalf("templateQueryCacheTTL() = %v, want 1m", got)
	}
}

// ---- cloneTemplateInfo ----

func TestCloneTemplateInfo(t *testing.T) {
	t.Run("nil returns nil", func(t *testing.T) {
		if got := cloneTemplateInfo(nil); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("replicas are deep-copied", func(t *testing.T) {
		src := &TemplateInfo{
			TemplateID: "tpl-1",
			Status:     StatusReady,
			Replicas: []ReplicaStatus{
				{NodeID: "node-1", Status: StatusReady},
			},
		}
		cloned := cloneTemplateInfo(src)
		if cloned == src {
			t.Fatalf("expected a new pointer")
		}
		// Mutating the clone must not affect the source.
		cloned.Replicas[0].NodeID = "mutated"
		cloned.Status = StatusFailed
		if src.Replicas[0].NodeID != "node-1" {
			t.Fatalf("source replica mutated via clone")
		}
		if src.Status != StatusReady {
			t.Fatalf("source status mutated via clone")
		}
	})

	t.Run("nil replicas stay nil", func(t *testing.T) {
		src := &TemplateInfo{TemplateID: "tpl-2"}
		cloned := cloneTemplateInfo(src)
		if cloned.Replicas != nil {
			t.Fatalf("expected nil replicas, got %v", cloned.Replicas)
		}
	})
}

// ---- list cache ----

func TestTemplateListCacheRoundTrip(t *testing.T) {
	templateListCache.Flush()

	// Miss before set.
	if _, ok := getCachedTemplateList(); ok {
		t.Fatalf("expected miss before set")
	}

	infos := []TemplateInfo{
		{TemplateID: "tpl-a", Status: StatusReady},
		{TemplateID: "tpl-b", Status: StatusFailed},
	}
	setTemplateListCache(infos)

	// Caller mutates the input after set; cache must be unaffected.
	infos[0].Status = "MUTATED"

	got, ok := getCachedTemplateList()
	if !ok {
		t.Fatalf("expected hit after set")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Status != StatusReady {
		t.Fatalf("cache was polluted by caller mutation: %v", got[0].Status)
	}

	// Mutating the returned slice must not affect the cache.
	got[0].Status = "MUTATED2"
	got2, ok := getCachedTemplateList()
	if !ok {
		t.Fatalf("expected hit on second get")
	}
	if got2[0].Status != StatusReady {
		t.Fatalf("cache was polluted by returned-slice mutation: %v", got2[0].Status)
	}
}

func TestTemplateListCacheWrongTypeEvicted(t *testing.T) {
	templateListCache.Flush()
	// Store a wrong-typed value directly.
	templateListCache.Set(templateListCacheKey, "not-a-slice", time.Minute)
	if _, ok := getCachedTemplateList(); ok {
		t.Fatalf("expected miss for wrong-typed cache value")
	}
	// Entry must be evicted so a subsequent set works cleanly.
	setTemplateListCache([]TemplateInfo{{TemplateID: "tpl-x"}})
	if _, ok := getCachedTemplateList(); !ok {
		t.Fatalf("expected hit after re-set")
	}
}

// ---- info cache ----

func TestTemplateInfoCacheRoundTrip(t *testing.T) {
	templateInfoCache.Flush()
	id := "tpl-info-1"

	if _, ok := getCachedTemplateInfo(id); ok {
		t.Fatalf("expected miss before set")
	}

	info := &TemplateInfo{
		TemplateID: id,
		Status:     StatusReady,
		Replicas:   []ReplicaStatus{{NodeID: "n1", Status: StatusReady}},
	}
	setTemplateInfoCache(id, info)

	// Mutate the input after set; cache must hold the clone.
	info.Status = "MUTATED"

	got, ok := getCachedTemplateInfo(id)
	if !ok {
		t.Fatalf("expected hit after set")
	}
	if got.Status != StatusReady {
		t.Fatalf("cache polluted by caller mutation: %v", got.Status)
	}

	// Mutating the returned clone must not affect cache.
	got.Replicas[0].NodeID = "mutated"
	got2, _ := getCachedTemplateInfo(id)
	if got2.Replicas[0].NodeID != "n1" {
		t.Fatalf("cache polluted by returned-clone mutation")
	}
}

func TestSetTemplateInfoCacheNoOps(t *testing.T) {
	templateInfoCache.Flush()
	// Empty templateID -> no-op.
	setTemplateInfoCache("", &TemplateInfo{TemplateID: "x"})
	if _, ok := getCachedTemplateInfo(""); ok {
		t.Fatalf("expected no entry for empty templateID")
	}
	// Nil info -> no-op.
	setTemplateInfoCache("tpl-nil", nil)
	if _, ok := getCachedTemplateInfo("tpl-nil"); ok {
		t.Fatalf("expected no entry for nil info")
	}
}

// ---- invalidateTemplateListCache vs invalidateTemplateCaches ----

func TestInvalidateTemplateListCacheOnlyClearsList(t *testing.T) {
	templateListCache.Flush()
	templateInfoCache.Flush()

	setTemplateListCache([]TemplateInfo{{TemplateID: "tpl-1"}})
	setTemplateInfoCache("tpl-1", &TemplateInfo{TemplateID: "tpl-1"})

	invalidateTemplateListCache()

	if _, ok := getCachedTemplateList(); ok {
		t.Fatalf("expected list cache cleared")
	}
	// Per-template info cache must be PRESERVED (this is the key behavioral
	// difference from invalidateTemplateCaches).
	if _, ok := getCachedTemplateInfo("tpl-1"); !ok {
		t.Fatalf("expected info cache preserved after invalidateTemplateListCache")
	}
}

func TestInvalidateTemplateCachesClearsListAndInfo(t *testing.T) {
	templateListCache.Flush()
	templateInfoCache.Flush()

	setTemplateListCache([]TemplateInfo{{TemplateID: "tpl-2"}})
	setTemplateInfoCache("tpl-2", &TemplateInfo{TemplateID: "tpl-2"})

	invalidateTemplateCaches("tpl-2")

	if _, ok := getCachedTemplateList(); ok {
		t.Fatalf("expected list cache cleared by invalidateTemplateCaches")
	}
	if _, ok := getCachedTemplateInfo("tpl-2"); ok {
		t.Fatalf("expected info cache cleared by invalidateTemplateCaches")
	}
}
