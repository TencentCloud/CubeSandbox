// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

func TestCheckReqVolumes(t *testing.T) {
	ctx := context.Background()

	t.Run("no volumes", func(t *testing.T) {
		r := &cubebox.RunCubeSandboxRequest{}
		err := checkReqVolumes(ctx, r)
		assert.NoError(t, err)
	})

	t.Run("valid volumes", func(t *testing.T) {
		r := &cubebox.RunCubeSandboxRequest{
			Volumes: []*cubebox.Volume{
				{
					Name: "vol1",
					VolumeSource: &cubebox.VolumeSource{
						EmptyDir: &cubebox.EmptyDirVolumeSource{
							Medium: cubebox.StorageMedium_StorageMediumDefault,
						},
					},
				},
				{
					Name: "vol2",
					VolumeSource: &cubebox.VolumeSource{
						EmptyDir: &cubebox.EmptyDirVolumeSource{
							Medium: cubebox.StorageMedium_StorageMediumDefault,
						},
					},
				},
			},
			Containers: []*cubebox.ContainerConfig{
				{
					VolumeMounts: []*cubebox.VolumeMounts{
						{
							Name: "vol1",
						},
						{
							Name: "vol2",
						},
					},
				},
			},
		}
		err := checkReqVolumes(ctx, r)
		assert.NoError(t, err)
	})

	t.Run("duplicate volume names", func(t *testing.T) {
		r := &cubebox.RunCubeSandboxRequest{
			Volumes: []*cubebox.Volume{
				{
					Name: "vol1",
					VolumeSource: &cubebox.VolumeSource{
						EmptyDir: &cubebox.EmptyDirVolumeSource{
							Medium: cubebox.StorageMedium_StorageMediumDefault,
						},
					},
				},
				{
					Name: "vol1",
					VolumeSource: &cubebox.VolumeSource{
						EmptyDir: &cubebox.EmptyDirVolumeSource{
							Medium: cubebox.StorageMedium_StorageMediumDefault,
						},
					},
				},
			},
		}
		err := checkReqVolumes(ctx, r)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "vol1 duplicated in Volumes params")
	})

	t.Run("invalid volume mount", func(t *testing.T) {
		r := &cubebox.RunCubeSandboxRequest{
			Volumes: []*cubebox.Volume{
				{
					Name: "vol1",
					VolumeSource: &cubebox.VolumeSource{
						EmptyDir: &cubebox.EmptyDirVolumeSource{
							Medium: cubebox.StorageMedium_StorageMediumDefault,
						},
					},
				},
			},
			Containers: []*cubebox.ContainerConfig{
				{
					VolumeMounts: []*cubebox.VolumeMounts{
						{
							Name: "vol2",
						},
					},
				},
			},
		}
		err := checkReqVolumes(ctx, r)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "volume vol2 not found")
	})
}

func TestCheckReqSandboxPathVolume(t *testing.T) {
	ctx := context.Background()
	t.Run("invalid volume mount", func(t *testing.T) {
		r := &cubebox.RunCubeSandboxRequest{
			Volumes: []*cubebox.Volume{
				{
					Name: "vol1",
					VolumeSource: &cubebox.VolumeSource{
						SandboxPath: &cubebox.SandboxPathVolumeSource{
							Path: "/tmp",
						},
					},
				},
			},
			Containers: []*cubebox.ContainerConfig{
				{
					VolumeMounts: []*cubebox.VolumeMounts{
						{
							Name: "vol1",
						},
					},
				},
			},
		}
		err := checkReqVolumes(ctx, r)
		assert.Error(t, err)
	})

	t.Run("valid volume mount", func(t *testing.T) {
		r := &cubebox.RunCubeSandboxRequest{
			Volumes: []*cubebox.Volume{
				{
					Name: "vol1",
					VolumeSource: &cubebox.VolumeSource{
						SandboxPath: &cubebox.SandboxPathVolumeSource{
							Type: "Cgroup",
							Path: "memory/default",
						},
					},
				},
			},
			Containers: []*cubebox.ContainerConfig{
				{
					VolumeMounts: []*cubebox.VolumeMounts{
						{
							Name: "vol1",
						},
					},
				},
			},
		}
		err := checkReqVolumes(ctx, r)
		assert.NoError(t, err)
	})
}

func TestCheckContainerVolumes(t *testing.T) {
	ctx := context.Background()

	t.Run("no volumes", func(t *testing.T) {
		c := &cubebox.ContainerConfig{}
		err := checkContainerVolumes(ctx, c)
		assert.NoError(t, err)
	})

	t.Run("valid volumes", func(t *testing.T) {
		c := &cubebox.ContainerConfig{
			VolumeMounts: []*cubebox.VolumeMounts{
				{
					Name:          "vol1",
					ContainerPath: "/path/to/vol1",
					Readonly:      true,
					Propagation:   cubebox.MountPropagation_PROPAGATION_PRIVATE,
				},
				{
					Name:          "vol2",
					ContainerPath: "/path/to/vol2",
					Propagation:   cubebox.MountPropagation_PROPAGATION_PRIVATE,
				},
			},
		}
		err := checkContainerVolumes(ctx, c)
		assert.NoError(t, err)
	})

	t.Run("invalid container path", func(t *testing.T) {
		c := &cubebox.ContainerConfig{
			VolumeMounts: []*cubebox.VolumeMounts{
				{
					Name:          "vol1",
					ContainerPath: "",
					Propagation:   cubebox.MountPropagation_PROPAGATION_PRIVATE,
				},
			},
		}
		err := checkContainerVolumes(ctx, c)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "should provide container_path")
	})
}

func TestCheckContainerNames(t *testing.T) {
	t.Run("no name", func(t *testing.T) {
		nameSet := sets.NewString()
		c := &cubebox.ContainerConfig{}
		err := checkContainerName(c.Name, &nameSet)
		assert.Error(t, err)
	})

	t.Run("invalid name", func(t *testing.T) {
		c := &cubebox.ContainerConfig{
			Name: "56..sdf66",
		}
		nameSet := sets.NewString()
		err := checkContainerName(c.Name, &nameSet)
		assert.Error(t, err)
	})

	t.Run("valid", func(t *testing.T) {
		ctrs := []*cubebox.ContainerConfig{
			{
				Name: "container1",
			},
			{
				Name: "container2",
			},
		}

		nameSet := sets.NewString()
		for _, c := range ctrs {
			err := checkContainerName(c.Name, &nameSet)
			assert.NoError(t, err)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		ctrs := []*cubebox.ContainerConfig{
			{
				Name: "container1",
			},
			{
				Name: "container1",
			},
		}

		err := func() error {
			nameSet := sets.NewString()
			for _, c := range ctrs {
				err := checkContainerName(c.Name, &nameSet)
				if err != nil {
					return err
				}
			}
			return nil
		}()
		assert.Error(t, err)
	})
}

func TestCheckParamExposedPorts(t *testing.T) {
	ctx := context.Background()

	t.Run("no exposed ports", func(t *testing.T) {
		err := checkParam(ctx, &cubebox.RunCubeSandboxRequest{})
		assert.NoError(t, err)
	})

	t.Run("valid ports within limit", func(t *testing.T) {
		err := checkParam(ctx, &cubebox.RunCubeSandboxRequest{
			ExposedPorts: []int64{49983, 80, 443, 8080},
		})
		assert.NoError(t, err)
	})

	t.Run("invalid port zero", func(t *testing.T) {
		err := checkParam(ctx, &cubebox.RunCubeSandboxRequest{
			ExposedPorts: []int64{0},
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid exposed port")
	})

	t.Run("invalid port out of range", func(t *testing.T) {
		err := checkParam(ctx, &cubebox.RunCubeSandboxRequest{
			ExposedPorts: []int64{65536},
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid exposed port")
	})

	t.Run("rejects more than 4 ports", func(t *testing.T) {
		err := checkParam(ctx, &cubebox.RunCubeSandboxRequest{
			ExposedPorts: []int64{49983, 80, 443, 8080, 9000},
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "at most 4")
	})
}
