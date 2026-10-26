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
	if err := setTemplateRequestCache(templateID, req); err != nil {
		t.Fatalf("setTemplateRequestCache failed: %v", err)
	}

	cached, hit, err := getCachedTemplateRequest(templateID)
	if err != nil {
		t.Fatalf("getCachedTemplateRequest failed: %v", err)
	}
	if !hit {
		t.Fatal("expected cache hit")
	}
	cached.Labels["k"] = "changed"

	cachedAgain, hit, err := getCachedTemplateRequest(templateID)
	if err != nil {
		t.Fatalf("getCachedTemplateRequest second read failed: %v", err)
	}
	if !hit {
		t.Fatal("expected cache hit on second read")
	}
	if got := cachedAgain.Labels["k"]; got != "v" {
		t.Fatalf("cache should return an isolated clone, got %q", got)
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
	if err := setTemplateRequestCache(templateID, req); err != nil {
		t.Fatalf("setTemplateRequestCache failed: %v", err)
	}
	setTemplateLocalityCache(templateID, []ReplicaStatus{
		{NodeID: "node-a", Status: ReplicaStatusReady},
	})
	setTemplateKindCache(templateID, TemplateKindSnapshot)

	if _, hit, err := getCachedTemplateRequest(templateID); err != nil {
		t.Fatalf("getCachedTemplateRequest failed before invalidation: %v", err)
	} else if !hit {
		t.Fatal("expected template request cache hit before invalidation")
	}
	if _, hit := getCachedTemplateLocality(templateID); !hit {
		t.Fatal("expected locality cache hit before invalidation")
	}
	if _, hit := getCachedTemplateKind(templateID); !hit {
		t.Fatal("expected kind cache hit before invalidation")
	}

	invalidateTemplateCaches(templateID)

	if _, hit, err := getCachedTemplateRequest(templateID); err != nil {
		t.Fatalf("getCachedTemplateRequest failed after invalidation: %v", err)
	} else if hit {
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
			if err := setTemplateRequestCache(templateID, req); err != nil {
				return err
			}
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

	if _, hit, err := getCachedTemplateRequest(templateID); err != nil {
		t.Fatalf("getCachedTemplateRequest failed after write lock invalidation: %v", err)
	} else if hit {
		t.Fatal("expected template request cache miss after concurrent delete invalidation")
	}
	if _, hit := getCachedTemplateLocality(templateID); hit {
		t.Fatal("expected locality cache miss after concurrent delete invalidation")
	}
}
