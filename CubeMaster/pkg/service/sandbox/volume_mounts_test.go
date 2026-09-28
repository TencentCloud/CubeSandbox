// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestCollectVolumeMountsFromContainersIncludesSandboxOnly(t *testing.T) {
	containers := []*cubeboxv1.Container{
		{
			Type: "sandbox",
			VolumeMounts: []*cubeboxv1.VolumeMounts{{
				Name:          "hostdir-0",
				ContainerPath: "/mnt/data",
				HostPath:      "/tmp/data",
				Readonly:      true,
			}},
		},
	}

	got := collectVolumeMountsFromContainers(containers)
	if len(got) != 1 {
		t.Fatalf("collectVolumeMountsFromContainers() len=%d want 1", len(got))
	}
	if got[0].GetName() != "hostdir-0" || got[0].GetContainerPath() != "/mnt/data" {
		t.Fatalf("unexpected mount: %+v", got[0])
	}
}

func TestCollectVolumeMountsFromContainersDedupsAcrossTypes(t *testing.T) {
	mount := &cubeboxv1.VolumeMounts{
		Name:          "hostdir-0",
		ContainerPath: "/mnt/data",
		HostPath:      "/tmp/data",
		Readonly:      true,
	}
	containers := []*cubeboxv1.Container{
		{Type: "sandbox", VolumeMounts: []*cubeboxv1.VolumeMounts{mount}},
		{Type: "container", VolumeMounts: []*cubeboxv1.VolumeMounts{mount}},
	}

	got := volumeMountsToContainerInfo(collectVolumeMountsFromContainers(containers))
	if len(got) != 1 {
		t.Fatalf("deduped mounts len=%d want 1", len(got))
	}
	if !got[0].Readonly || got[0].ContainerPath != "/mnt/data" || got[0].Name != "hostdir-0" {
		t.Fatalf("unexpected mount info: %+v", got[0])
	}
}

func TestCollectVolumeMountsFromContainersSkipsNilContainer(t *testing.T) {
	containers := []*cubeboxv1.Container{
		nil,
		{
			Type: "container",
			VolumeMounts: []*cubeboxv1.VolumeMounts{{
				Name:          "hostdir-0",
				ContainerPath: "/mnt/data",
			}},
		},
	}

	got := collectVolumeMountsFromContainers(containers)
	if len(got) != 1 || got[0].GetName() != "hostdir-0" {
		t.Fatalf("collectVolumeMountsFromContainers()=%#v", got)
	}
}

func TestVolumeMountsToContainerInfoOmitsEmpty(t *testing.T) {
	if got := volumeMountsToContainerInfo(nil); got != nil {
		t.Fatalf("volumeMountsToContainerInfo(nil)=%v want nil", got)
	}
	if got := volumeMountsToContainerInfo([]*cubeboxv1.VolumeMounts{nil}); got != nil {
		t.Fatalf("volumeMountsToContainerInfo([nil])=%v want nil", got)
	}
}

func TestVolumeMountsToContainerInfoFiltersInternalRootfs(t *testing.T) {
	got := volumeMountsToContainerInfo([]*cubeboxv1.VolumeMounts{
		{
			Name:          "cube_rootfs_rw",
			ContainerPath: "/",
		},
		{
			Name:          "hostdir-0",
			ContainerPath: "/mnt/rw",
		},
		{
			Name:          "hostdir-1",
			ContainerPath: "/mnt/ro",
			Readonly:      true,
		},
	})
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (rootfs filtered): %+v", len(got), got)
	}
	if got[0].Name != "hostdir-0" || got[1].Name != "hostdir-1" {
		t.Fatalf("unexpected mounts: %+v", got)
	}
}

func TestVolumeMountsToContainerInfoFiltersRootPathEvenWithoutRootfsName(t *testing.T) {
	got := volumeMountsToContainerInfo([]*cubeboxv1.VolumeMounts{
		{Name: "other", ContainerPath: "/"},
		{Name: "hostdir-0", ContainerPath: "/mnt/data"},
	})
	if len(got) != 1 || got[0].ContainerPath != "/mnt/data" {
		t.Fatalf("got=%+v want only /mnt/data", got)
	}
}
