// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestGetSchedulableNodesByInstanceTypeFiltersBeforeLimit(t *testing.T) {
	origNodesByClusters := l.sortedNodesByClusters
	origCache := l.cache
	defer func() {
		l.sortedNodesByClusters = origNodesByClusters
		l.cache = origCache
	}()

	now := time.Now()
	mk := func(id, ip string, disabled bool) *node.Node {
		n := &node.Node{InsID: id, IP: ip, ReportedReady: true, Healthy: true, MetaDataUpdateAt: now}
		n.SetSchedulingDisabled(disabled)
		return n
	}
	ok := mk("ok-1", "10.0.0.3", false)
	l.sortedNodesByClusters = map[string]node.NodeList{
		"valid": {mk("iso-1", "10.0.0.1", true), mk("iso-2", "10.0.0.2", true), ok},
	}
	l.cache = cache.New(0, 0)

	got := GetSchedulableNodesByInstanceType(1, "valid")
	if got.Len() != 1 || got[0].ID() != "ok-1" {
		t.Fatalf("schedulable limit: got %+v", got)
	}
	if GetHealthyNodesByInstanceType(-1, "valid").Len() != 3 {
		t.Fatal("healthy should include isolated")
	}
	if GetSchedulableNodesByInstanceType(-1, "valid").Len() != 1 {
		t.Fatal("schedulable should exclude isolated")
	}
}

func TestUpdateNodeFromMetaDataPropagatesSchedulingDisabled(t *testing.T) {
	origCache := l.cache
	origSorted := l.sortedNodesByClusters
	defer func() {
		l.cache = origCache
		l.sortedNodesByClusters = origSorted
	}()
	l.cache = cache.New(0, 0)
	l.sortedNodesByClusters = map[string]node.NodeList{}

	existing := &node.Node{InsID: "n1", IP: "10.0.0.1", Healthy: true, InstanceType: "valid"}
	l.cache.SetDefault("n1", existing)

	incoming := &node.Node{InsID: "n1", IP: "10.0.0.1", Healthy: true, InstanceType: "valid"}
	incoming.SetSchedulingDisabled(true)
	if err := l.updateNodeFromMetaData(incoming); err != nil {
		t.Fatalf("updateNodeFromMetaData: %v", err)
	}
	got, ok := GetNode("n1")
	if !ok || got.SchedulingAllowed() || !got.SchedulingDisabled() {
		t.Fatalf("got allowed=%v disabled=%v", got.SchedulingAllowed(), got.SchedulingDisabled())
	}
}
