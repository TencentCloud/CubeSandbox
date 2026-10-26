// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

func TestVersionsHash(t *testing.T) {
	v1 := []model.ComponentVersion{{Component: "a", Version: "1"}}
	v2 := []model.ComponentVersion{{Component: "a", Version: "1"}}
	if service.VersionsHash(v1) != service.VersionsHash(v2) {
		t.Error("same versions should have same hash")
	}
	v3 := []model.ComponentVersion{{Component: "a", Version: "2"}}
	if service.VersionsHash(v1) == service.VersionsHash(v3) {
		t.Error("different versions should have different hash")
	}
}

func TestMergeComponentVersions(t *testing.T) {
	prev := []model.ComponentVersion{{Component: "a", Version: "1"}, {Component: "b", Version: "1"}}
	next := []model.ComponentVersion{{Component: "b", Version: "2"}}
	merged := service.MergeComponentVersions(prev, next)
	if len(merged) != 2 {
		t.Fatalf("len = %d", len(merged))
	}
	m := map[string]string{}
	for _, v := range merged {
		m[v.Component] = v.Version
	}
	if m["a"] != "1" || m["b"] != "2" {
		t.Errorf("merge result = %v", m)
	}
}

func TestCompatVersionsChanged(t *testing.T) {
	prev := map[string]string{"guest-image": "v1", "cube-agent": "v1"}
	next := map[string]string{"guest-image": "v1", "cube-agent": "v2"}
	if !service.CompatVersionsChanged(prev, next) {
		t.Error("expected changed")
	}
	if service.CompatVersionsChanged(prev, prev) {
		t.Error("expected unchanged")
	}
}

func TestBuildVersionMatrix(t *testing.T) {
	declared := map[string]string{"cubelet": "v1.0"}
	nodes := []*model.NodeSnapshot{
		{NodeID: "n-1", Healthy: true, Versions: []model.ComponentVersion{{Component: "cubelet", Version: "v1.0"}}},
	}
	m := service.BuildVersionMatrix(declared, nil, nodes)
	if len(m.Components) != 1 {
		t.Fatalf("components = %d", len(m.Components))
	}
	if !m.Components[0].Consistent {
		t.Error("expected consistent")
	}
}
