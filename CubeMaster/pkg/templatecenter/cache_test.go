// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestTemplateRequestCacheReturnsClone(t *testing.T) {
	templateDefinitionCache.Flush()
	templateID := "tpl-cache-test"
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      templateID,
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
		Labels: map[string]string{"k": "v"},
	}
	setTemplateRequestCache(templateID, req)

	cached, hit := getCachedTemplateRequest(templateID)
	if !hit {
		t.Fatal("expected cache hit")
	}
	cached.Labels["k"] = "changed"

	cachedAgain, hit := getCachedTemplateRequest(templateID)
	if !hit {
		t.Fatal("expected cache hit on second read")
	}
	if got := cachedAgain.Labels["k"]; got != "v" {
		t.Fatalf("cache should return an isolated clone, got %q", got)
	}
}

// TestTemplateRequestCacheMaterializesDroppedRequest pins the boundary contract
// the JSON clone used to hold by accident: entries are stored normalized, i.e.
// with the embedded *Request dropped, and UnmarshalJSON always materialized it
// again, so a hit never handed out a request that panics on the promoted
// RequestID field. GetTemplateRequest hydrates its result too, but that must not
// be the only thing standing between a caller and a nil-pointer panic.
func TestTemplateRequestCacheMaterializesDroppedRequest(t *testing.T) {
	templateDefinitionCache.Flush()
	templateID := "tpl-cache-dropped-request"
	stored := &types.CreateCubeSandboxReq{Labels: map[string]string{"k": "v"}}
	if stored.Request != nil {
		t.Fatal("fixture must look like a normalized stored request")
	}
	setTemplateRequestCache(templateID, stored)

	cached, hit := getCachedTemplateRequest(templateID)
	if !hit {
		t.Fatal("expected cache hit")
	}
	if cached.Request == nil {
		t.Fatal("cache hit returned a request with a dropped embedded request")
	}
	if got := cached.RequestID; got != "" {
		t.Fatalf("expected an empty request id before hydration, got %q", got)
	}

	// The stored entry stays normalized: hydrating is per hit, not a mutation.
	cached.RequestID = "leaked"
	again, hit := getCachedTemplateRequest(templateID)
	if !hit {
		t.Fatal("expected cache hit on second read")
	}
	if again.Request == nil {
		t.Fatal("second hit lost the embedded request")
	}
	if got := again.RequestID; got != "" {
		t.Fatalf("a hit mutated the cached entry, got request id %q", got)
	}
}

func TestTemplateLocalityCacheReturnsCopy(t *testing.T) {
	templateLocalityReadyCache.Flush()
	templateID := "tpl-locality-test"
	setTemplateLocalityCache(templateID, []ReplicaStatus{
		{NodeID: "node-a", Status: ReplicaStatusReady},
		{NodeID: "node-b", Status: ReplicaStatusFailed},
	})

	replicas, hit := getCachedTemplateLocality(templateID)
	if !hit {
		t.Fatal("expected locality cache hit")
	}
	if len(replicas) != 1 {
		t.Fatalf("expected only ready replicas to be cached, got %d", len(replicas))
	}
	replicas[0].NodeID = "mutated"

	replicasAgain, hit := getCachedTemplateLocality(templateID)
	if !hit {
		t.Fatal("expected locality cache hit on second read")
	}
	if got := replicasAgain[0].NodeID; got != "node-a" {
		t.Fatalf("expected cached locality copy to remain unchanged, got %q", got)
	}
}

func TestInvalidateTemplateCachesClearsTemplateCaches(t *testing.T) {
	templateDefinitionCache.Flush()
	templateLocalityReadyCache.Flush()
	templateKindCache.Flush()

	templateID := "tpl-invalidate-test"
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      templateID,
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	}
	setTemplateRequestCache(templateID, req)
	setTemplateLocalityCache(templateID, []ReplicaStatus{
		{NodeID: "node-a", Status: ReplicaStatusReady},
	})
	setTemplateKindCache(templateID, TemplateKindSnapshot)

	if _, hit := getCachedTemplateRequest(templateID); !hit {
		t.Fatal("expected template request cache hit before invalidation")
	}
	if _, hit := getCachedTemplateLocality(templateID); !hit {
		t.Fatal("expected locality cache hit before invalidation")
	}
	if _, hit := getCachedTemplateKind(templateID); !hit {
		t.Fatal("expected kind cache hit before invalidation")
	}

	invalidateTemplateCaches(templateID)

	if _, hit := getCachedTemplateRequest(templateID); hit {
		t.Fatal("expected template request cache miss after invalidation")
	}
	if _, hit := getCachedTemplateLocality(templateID); hit {
		t.Fatal("expected locality cache miss after invalidation")
	}
	if _, hit := getCachedTemplateKind(templateID); hit {
		t.Fatal("expected kind cache miss after invalidation")
	}
}

func TestTemplateKindCacheRoundTrip(t *testing.T) {
	templateKindCache.Flush()
	templateID := "tpl-kind-cache-test"

	if _, hit := getCachedTemplateKind(templateID); hit {
		t.Fatal("expected kind cache miss before set")
	}

	setTemplateKindCache(templateID, TemplateKindSnapshot)
	kind, hit := getCachedTemplateKind(templateID)
	if !hit {
		t.Fatal("expected kind cache hit after set")
	}
	if kind != TemplateKindSnapshot {
		t.Fatalf("expected kind %q, got %q", TemplateKindSnapshot, kind)
	}

	setTemplateKindCache("", "ignored")
	if _, hit := getCachedTemplateKind(""); hit {
		t.Fatal("expected miss for empty key")
	}
}

func TestTemplateWriteLockClearsConcurrentReadRefill(t *testing.T) {
	templateDefinitionCache.Flush()
	templateLocalityReadyCache.Flush()

	templateID := "tpl-lock-race-test"
	readerReady := make(chan struct{})
	releaseReader := make(chan struct{})
	readerErr := make(chan error, 1)
	writerErr := make(chan error, 1)

	go func() {
		readerErr <- withTemplateReadLock(templateID, func() error {
			close(readerReady)
			<-releaseReader

			req := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{
					constants.CubeAnnotationAppSnapshotTemplateID:      templateID,
					constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
				},
			}
			setTemplateRequestCache(templateID, req)
			setTemplateLocalityCache(templateID, []ReplicaStatus{
				{NodeID: "node-a", Status: ReplicaStatusReady},
			})
			return nil
		})
	}()

	<-readerReady

	go func() {
		writerErr <- withTemplateWriteLock(templateID, func() error {
			invalidateTemplateCaches(templateID)
			return nil
		})
	}()

	close(releaseReader)

	if err := <-readerErr; err != nil {
		t.Fatalf("reader refill failed: %v", err)
	}
	if err := <-writerErr; err != nil {
		t.Fatalf("writer invalidation failed: %v", err)
	}

	if _, hit := getCachedTemplateRequest(templateID); hit {
		t.Fatal("expected template request cache miss after concurrent delete invalidation")
	}
	if _, hit := getCachedTemplateLocality(templateID); hit {
		t.Fatal("expected locality cache miss after concurrent delete invalidation")
	}
}
