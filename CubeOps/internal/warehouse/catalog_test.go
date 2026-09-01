// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"reflect"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

func TestCatalog(t *testing.T) {
	got := Catalog()
	if len(got) != 4 {
		t.Fatalf("len=%d want 4", len(got))
	}
	want := []string{ComponentShim, ComponentImage, ComponentAgent, ComponentKernel}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("Catalog[%d]=%s want %s", i, got[i].Name, name)
		}
		if !KnownComponent(name) {
			t.Errorf("KnownComponent(%s)=false", name)
		}
	}
	if KnownComponent("cubelet") {
		t.Fatal("cubelet should not be in warehouse catalog")
	}
}

func TestSummarizeComponents_EmptyWarehouse(t *testing.T) {
	out := SummarizeComponents(nil, nil, []string{"n1"}, true)
	if len(out) != 4 {
		t.Fatalf("len=%d want 4", len(out))
	}
	for _, row := range out {
		if row.VersionCount != 0 || row.SizeBytes != 0 || len(row.Arches) != 0 {
			t.Errorf("%s: %+v", row.Name, row)
		}
		if row.NodesMissing == nil || *row.NodesMissing != 0 {
			t.Errorf("%s nodesMissing=%v want 0", row.Name, row.NodesMissing)
		}
	}
}

func TestSummarizeComponents_OmitsCoverageWhenUnavailable(t *testing.T) {
	items := []store.WarehouseItem{{Arch: ArchAMD64, Component: ComponentShim, Version: "v0.6.0", SizeBytes: 10}}
	out := SummarizeComponents(items, nil, []string{"n1"}, false)
	shim := out[0]
	if shim.Name != ComponentShim || shim.VersionCount != 1 || shim.SizeBytes != 10 {
		t.Fatalf("shim=%+v", shim)
	}
	if shim.NodesMissing != nil {
		t.Fatalf("nodesMissing should be omitted, got %v", *shim.NodesMissing)
	}
}

func TestSummarizeComponents_CountsArchVersionAndMissing(t *testing.T) {
	items := []store.WarehouseItem{
		{Arch: ArchAMD64, Component: ComponentShim, Version: "v0.6.0", SizeBytes: 100},
		{Arch: ArchARM64, Component: ComponentShim, Version: "v0.6.0", SizeBytes: 80},
		{Arch: ArchAMD64, Component: ComponentShim, Version: "v0.5.0", SizeBytes: 90},
	}
	installs := []store.NodeInstall{
		{NodeID: "n1", Arch: ArchAMD64, Component: ComponentShim, Version: "v0.6.0"},
		{NodeID: "n1", Arch: ArchARM64, Component: ComponentShim, Version: "v0.6.0"},
		{NodeID: "n1", Arch: ArchAMD64, Component: ComponentShim, Version: "v0.5.0"},
	}
	out := SummarizeComponents(items, installs, []string{"n1", "n2"}, true)
	shim := out[0]
	if shim.VersionCount != 2 {
		t.Errorf("versionCount=%d want 2", shim.VersionCount)
	}
	if shim.SizeBytes != 270 {
		t.Errorf("sizeBytes=%d want 270", shim.SizeBytes)
	}
	if !reflect.DeepEqual(shim.Arches, []string{ArchAMD64, ArchARM64}) {
		t.Errorf("arches=%v", shim.Arches)
	}
	if shim.NodesMissing == nil || *shim.NodesMissing != 1 {
		t.Errorf("nodesMissing=%v want 1", shim.NodesMissing)
	}
}

func TestGroupComponent(t *testing.T) {
	ts := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	items := []store.WarehouseItem{
		{Arch: ArchARM64, Component: ComponentShim, Version: "v0.6.0", SizeBytes: 80, Source: "github", CreatedAt: ts},
		{Arch: ArchAMD64, Component: ComponentShim, Version: "v0.6.0", SizeBytes: 100, Source: "github", CreatedAt: ts},
		{Arch: ArchAMD64, Component: ComponentShim, Version: "v0.5.0", SizeBytes: 90, Source: "github", CreatedAt: ts},
		{Arch: ArchAMD64, Component: ComponentImage, Version: "v0.6.0", SizeBytes: 1, Source: "github"},
	}
	installs := []store.NodeInstall{
		{NodeID: "n1", Arch: ArchAMD64, Component: ComponentShim, Version: "v0.6.0"},
	}
	got := GroupComponent(ComponentShim, items, installs, []string{"n1", "n2"}, true)
	if got.Name != ComponentShim {
		t.Fatalf("name=%s", got.Name)
	}
	if len(got.Versions) != 2 {
		t.Fatalf("versions=%d want 2: %#v", len(got.Versions), got.Versions)
	}
	if got.Versions[0].Version != "v0.6.0" || len(got.Versions[0].Artifacts) != 2 {
		t.Fatalf("first group=%+v", got.Versions[0])
	}
	if got.Versions[0].Artifacts[0].Arch != ArchAMD64 {
		t.Fatalf("arch order=%s want amd64 first", got.Versions[0].Artifacts[0].Arch)
	}
	amd := got.Versions[0].Artifacts[0]
	if !reflect.DeepEqual(amd.NodesInstalled, []string{"n1"}) {
		t.Errorf("installed=%v", amd.NodesInstalled)
	}
	if !reflect.DeepEqual(amd.NodesMissing, []string{"n2"}) {
		t.Errorf("missing=%v", amd.NodesMissing)
	}
	empty := GroupComponent(ComponentAgent, items, nil, []string{"n1"}, true)
	if empty.Name != ComponentAgent || len(empty.Versions) != 0 {
		t.Fatalf("empty agent=%+v", empty)
	}
}
