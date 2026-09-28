// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCubeBoxDeepCopy(t *testing.T) {
	now := time.Now()
	original := &CubeBox{
		Metadata: Metadata{
			ID:        "test-id",
			Name:      "test-name",
			SandboxID: "test-sandbox",
			Annotations: map[string]string{
				"key1": "value1",
			},
			Labels: map[string]string{
				"label1": "labelvalue1",
			},
			CreatedAt:   now.Unix(),
			DeletedTime: &now,
			ResourceWithOverHead: &ResourceWithOverHead{
				MemReq:   resource.MustParse("1Gi"),
				HostCpuQ: resource.MustParse("1000m"),
			},
		},
		Namespace:          "default",
		AppID:              "app123",
		IP:                 "192.168.1.1",
		FirstContainerName: "main-container",
		Version:            "v2",
		PortMappings: []*cubebox.PortMapping{
			{
				ContainerPort: 8080,
				HostPort:      80,
			},
		},
		NetworkType:    "tap",
		RuntimeHandler: "cube",
		ExposedPorts:   []int64{8080, 443},
		CubeNetworkConfig: &cubebox.CubeNetworkConfig{
			AllowOut: []string{"10.0.0.0/8"},
		},
		Volumes: []*cubebox.Volume{{Name: "tmp"}},
		ImageReferences: map[string]ImageReference{
			"img1": {
				ID:         "img1",
				References: []string{"ref1", "ref2"},
			},
		},
	}

	original.ContainersMap = &ContainersMap{
		ContainerMap: map[string]*Container{
			"container1": {
				Metadata: Metadata{
					ID:   "container1",
					Name: "test-container",
				},
				IP:            "192.168.1.2",
				IsDebugStdout: true,
				Status:        StoreStatus(Status{Pid: 12345}),
			},
		},
	}

	copied := original.DeepCopy()

	if copied.ID != original.ID {
		t.Errorf("ID not copied correctly: got %s, want %s", copied.ID, original.ID)
	}

	if copied.Namespace != original.Namespace {
		t.Errorf("Namespace not copied correctly")
	}

	if copied.NetworkType != original.NetworkType || copied.RuntimeHandler != original.RuntimeHandler {
		t.Errorf("network recreate fields not copied")
	}
	if len(copied.ExposedPorts) != 2 || copied.ExposedPorts[0] != 8080 {
		t.Errorf("ExposedPorts not copied: %v", copied.ExposedPorts)
	}
	if copied.CubeNetworkConfig == nil || len(copied.CubeNetworkConfig.AllowOut) != 1 {
		t.Errorf("CubeNetworkConfig not copied")
	}
	copied.CubeNetworkConfig.AllowOut[0] = "mutated"
	if original.CubeNetworkConfig.AllowOut[0] == "mutated" {
		t.Errorf("Modifying copied CubeNetworkConfig affected original")
	}
	if len(copied.Volumes) != 1 || copied.Volumes[0].GetName() != "tmp" {
		t.Errorf("Volumes not copied")
	}
	copied.Volumes[0].Name = "mutated"
	if original.Volumes[0].GetName() == "mutated" {
		t.Errorf("Modifying copied Volumes affected original")
	}

	copied.Annotations["key1"] = "modified"
	if original.Annotations["key1"] == "modified" {
		t.Errorf("Modifying copied Annotations affected original")
	}

	copied.Labels["label1"] = "modified"
	if original.Labels["label1"] == "modified" {
		t.Errorf("Modifying copied Labels affected original")
	}

	copied.ImageReferences["img1"].References[0] = "modified"
	if original.ImageReferences["img1"].References[0] == "modified" {
		t.Errorf("Modifying copied ImageReferences affected original")
	}

	copied.PortMappings[0].ContainerPort = 9999
	if original.PortMappings[0].ContainerPort == 9999 {
		t.Errorf("Modifying copied PortMappings affected original")
	}

	copiedContainer, _ := copied.ContainersMap.Get("container1")
	copiedContainer.IP = "192.168.1.100"
	originalContainer, _ := original.ContainersMap.Get("container1")
	if originalContainer.IP == "192.168.1.100" {
		t.Errorf("Modifying copied Container affected original")
	}

	if copied.DeletedTime == original.DeletedTime {
		t.Errorf("DeletedTime pointer should be different")
	}
	if !copied.DeletedTime.Equal(*original.DeletedTime) {
		t.Errorf("DeletedTime value should be equal")
	}
}

func TestCubeBoxDeepCopyCompontsIndependent(t *testing.T) {
	original := &CubeBox{
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{Version: "v1"}},
			},
			Volumes: map[string]templatetypes.LocalBaseVolume{
				"root": {VolumeID: "vol-1"},
			},
		},
	}
	copied := original.DeepCopy()
	if copied.LocalRunTemplate == original.LocalRunTemplate {
		t.Fatal("DeepCopy shared LocalRunTemplate pointer")
	}
	copied.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim] = templatetypes.LocalComponent{
		Component: templatetypes.MachineComponent{Version: "mutated"},
	}
	copied.LocalRunTemplate.Volumes["root"] = templatetypes.LocalBaseVolume{VolumeID: "mutated"}
	if original.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim].Component.Version != "v1" {
		t.Fatal("DeepCopy shared Componts map")
	}
	if original.LocalRunTemplate.Volumes["root"].VolumeID != "vol-1" {
		t.Fatal("DeepCopy shared Volumes map")
	}
}

func TestContainerDeepCopy(t *testing.T) {
	original := &Container{
		Metadata: Metadata{
			ID:   "test-container",
			Name: "container-name",
			Annotations: map[string]string{
				"anno1": "val1",
			},
		},
		IP:            "10.0.0.1",
		IsDebugStdout: true,
		IsPod:         false,
		Status:        StoreStatus(Status{Pid: 999, CreatedAt: time.Now().Unix()}),
	}

	copied := original.DeepCopy()

	if copied.ID != original.ID {
		t.Errorf("Container ID not copied correctly")
	}

	copied.Annotations["anno1"] = "modified"
	if original.Annotations["anno1"] == "modified" {
		t.Errorf("Modifying copied annotations affected original")
	}

	copiedStatus := copied.Status.Get()
	if copiedStatus.Pid != 999 {
		t.Errorf("Status not copied correctly")
	}
}

func TestStatusStorageDeepCopy(t *testing.T) {
	original := StoreStatus(Status{
		Pid:        12345,
		CreatedAt:  time.Now().Unix(),
		StartedAt:  time.Now().Unix(),
		FinishedAt: 0,
		ExitCode:   0,
		Reason:     "Running",
	})

	copied := original.DeepCopy()

	if copied.Get().Pid != original.Get().Pid {
		t.Errorf("StatusStorage Pid not copied correctly")
	}

	copied.Update(func(s Status) (Status, error) {
		s.Pid = 99999
		return s, nil
	})

	if original.Get().Pid == 99999 {
		t.Errorf("Modifying copied StatusStorage affected original")
	}
}

func TestNilDeepCopy(t *testing.T) {
	var nilBox *CubeBox
	copied := nilBox.DeepCopy()
	if copied != nil {
		t.Errorf("DeepCopy of nil should return nil")
	}

	var nilContainer *Container
	copiedContainer := nilContainer.DeepCopy()
	if copiedContainer != nil {
		t.Errorf("DeepCopy of nil Container should return nil")
	}

	var nilStatus *StatusStorage
	copiedStatus := nilStatus.DeepCopy()
	if copiedStatus != nil {
		t.Errorf("DeepCopy of nil StatusStorage should return nil")
	}
}
