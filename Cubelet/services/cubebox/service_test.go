// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	imagesv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	volpluginv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/volumeplugin/v1"
)

func TestToGRPCContainerIncludesVolumeMounts(t *testing.T) {
	container := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID:     "ctr-1",
			Labels: map[string]string{},
			Config: &cubeboxv1.ContainerConfig{
				Image: &imagesv1.ImageSpec{Image: "img"},
				Resources: &cubeboxv1.Resource{
					Cpu: "100m",
					Mem: "128Mi",
				},
				VolumeMounts: []*cubeboxv1.VolumeMounts{{
					Name:          "hostdir-0",
					ContainerPath: "/mnt/data",
					HostPath:      "/tmp/data",
					Readonly:      true,
				}},
			},
		},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: time.Now().UnixNano()}),
	}

	got := toGRPCContainer(container)
	require.Len(t, got.GetVolumeMounts(), 1)
	assert.Equal(t, "hostdir-0", got.GetVolumeMounts()[0].GetName())
	assert.Equal(t, "/mnt/data", got.GetVolumeMounts()[0].GetContainerPath())
	assert.Equal(t, "/tmp/data", got.GetVolumeMounts()[0].GetHostPath())
	assert.True(t, got.GetVolumeMounts()[0].GetReadonly())
}

func TestDeadline(t *testing.T) {
	expected := time.Duration(10) * time.Second

	t1 := time.Duration(5) * time.Second
	t2 := time.Duration(5000) * time.Millisecond
	tSum := time.Duration(0)
	tSum += t1
	tSum += t2
	assert.True(t, expected == tSum)

	t3, _ := time.ParseDuration("5s")
	tSum = t1
	tSum += t3
	assert.True(t, expected == tSum)
}

func TestDeadlineCtx(t *testing.T) {
	t1, _ := time.ParseDuration("1ms")
	t2 := time.Duration(2) * time.Millisecond
	t3 := time.Duration(3) * time.Microsecond
	tSum := t1 + t2 + t3
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	timeout := false
	ch := make(chan int)
	go func() {
		time.Sleep(tSum + 10*time.Millisecond)
		ch <- 0
	}()

	select {
	case <-ctx.Done():
		timeout = true
	case <-ch:
	}
	assert.True(t, timeout)
}

func TestDeadlineCtxDelay(t *testing.T) {
	t1, _ := time.ParseDuration("1ms")
	t2 := time.Duration(2) * time.Millisecond
	t3 := time.Duration(3) * time.Microsecond

	delaySeconds := 10 * time.Millisecond
	delta := 100 * time.Millisecond
	tSum := t1 + t2 + t3

	ctx, cancel := context.WithTimeout(context.Background(), tSum+delaySeconds+delta)
	defer cancel()
	timeout := false
	ch := make(chan int)
	go func() {
		select {
		case <-time.After(delaySeconds):
			t.Logf("after delay:%v", delaySeconds)
		case <-ctx.Done():
			return
		}

		time.Sleep(tSum + time.Millisecond)
		ch <- 0
	}()

	select {
	case <-ctx.Done():
		timeout = true
	case <-ch:
	}
	assert.False(t, timeout)
}

func TestDeadlineCtxZeroDelay(t *testing.T) {
	t1, _ := time.ParseDuration("1ms")
	t2 := time.Duration(2) * time.Millisecond
	t3 := time.Duration(3) * time.Microsecond

	delaySeconds := time.Duration(0)
	delta := 100 * time.Millisecond
	tSum := t1 + t2 + t3

	ctx, cancel := context.WithTimeout(context.Background(), tSum+delaySeconds+delta)
	defer cancel()
	timeout := false
	ch := make(chan int)
	go func() {
		select {
		case <-time.After(delaySeconds):
			t.Logf("after delay:%v", delaySeconds)
		case <-ctx.Done():
			return
		}

		time.Sleep(tSum + time.Millisecond)
		t.Logf("after job:%v", tSum+time.Millisecond)
		ch <- 0
	}()

	select {
	case <-ctx.Done():
		timeout = true
	case <-ch:
	}
	assert.False(t, timeout)
}

func TestGetNextNumaNode(t *testing.T) {

	s := &service{
		numaNodeIndex: 0,
	}

	nodeCount := int32(numa.GetNumaNodeCount())
	require.Greater(t, nodeCount, int32(0), "numa node count should be positive")

	// getNextNumaNode increments the index then takes modulo the node count,
	// so the sequence cycles through 1, 2, ..., nodeCount-1, 0, 1, ...
	for i := int32(1); i <= 3*nodeCount; i++ {
		want := i % nodeCount
		assert.Equalf(t, want, s.getNextNumaNode(), "call %d should return %d", i, want)
	}
}

func TestCubeboxSorting(t *testing.T) {

	now := time.Now().Unix()

	cubeboxes := []*cubeboxstore.CubeBox{
		{
			Metadata: cubeboxstore.Metadata{
				ID:        "box1",
				CreatedAt: now - 100,
			},
			Status: cubeboxstore.StoreStatus(cubeboxstore.Status{
				StartedAt: now - 50,
			}),
		},
		{
			Metadata: cubeboxstore.Metadata{
				ID:        "box2",
				CreatedAt: now - 50,
			},
			Status: cubeboxstore.StoreStatus(cubeboxstore.Status{
				StartedAt: now - 20,
			}),
		},
		{
			Metadata: cubeboxstore.Metadata{
				ID:        "box3",
				CreatedAt: now - 10,
			},
			Status: cubeboxstore.StoreStatus(cubeboxstore.Status{
				StartedAt: now - 5,
			}),
		},
		{
			Metadata: cubeboxstore.Metadata{
				ID:        "box4",
				CreatedAt: now - 200,
			},
			Status: cubeboxstore.StoreStatus(cubeboxstore.Status{
				FinishedAt: now - 150,
			}),
		},
		{
			Metadata: cubeboxstore.Metadata{
				ID:        "box5",
				CreatedAt: now - 30,
			},
			Status: cubeboxstore.StoreStatus(cubeboxstore.Status{
				FinishedAt: now - 25,
			}),
		},
	}

	sort.Slice(cubeboxes, func(i, j int) bool {
		if cubeboxes[i].Status.IsTerminated() {
			return false
		}
		return cubeboxes[i].CreatedAt > cubeboxes[j].CreatedAt
	})

	assert.False(t, cubeboxes[0].Status.IsTerminated(), "First element should not be terminated")
	assert.False(t, cubeboxes[1].Status.IsTerminated(), "Second element should not be terminated")
	assert.False(t, cubeboxes[2].Status.IsTerminated(), "Third element should not be terminated")
	assert.True(t, cubeboxes[3].Status.IsTerminated(), "Fourth element should be terminated")
	assert.True(t, cubeboxes[4].Status.IsTerminated(), "Fifth element should be terminated")

	assert.Equal(t, "box3", cubeboxes[0].ID, "Newest non-terminated should be first")
	assert.Equal(t, "box2", cubeboxes[1].ID, "Middle non-terminated should be second")
	assert.Equal(t, "box1", cubeboxes[2].ID, "Oldest non-terminated should be third")

	assert.Equal(t, "box4", cubeboxes[3].ID, "Older terminated should come before newer terminated")
	assert.Equal(t, "box5", cubeboxes[4].ID, "Newer terminated should come after older terminated")

	maxListCubebox := 3
	truncated := cubeboxes[:maxListCubebox]
	assert.Len(t, truncated, maxListCubebox, "Should truncate to maxListCubebox")

	assert.Equal(t, "box3", truncated[0].ID)
	assert.Equal(t, "box2", truncated[1].ID)
	assert.Equal(t, "box1", truncated[2].ID)

	for _, box := range truncated {
		assert.False(t, box.Status.IsTerminated(), "Truncated list should not contain terminated boxes")
	}
}

func TestValidateCommitSandboxTarget(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sandbox"},
	}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox",
			Config: &cubeboxv1.ContainerConfig{
				VolumeMounts: []*cubeboxv1.VolumeMounts{{
					Name:          "root",
					ContainerPath: "/",
				}},
			},
		},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: time.Now().UnixNano()}),
		IsPod:  true,
	})

	rootVolume, err := validateCommitSandboxTarget(cb)
	require.NoError(t, err)
	assert.Equal(t, "root", rootVolume)
}

func TestValidateCommitSandboxTargetRejectsHostPath(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sandbox"},
	}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox",
			Config: &cubeboxv1.ContainerConfig{
				VolumeMounts: []*cubeboxv1.VolumeMounts{{
					Name:          "root",
					ContainerPath: "/",
				}, {
					Name:     "host",
					HostPath: "/var/lib/data",
				}},
			},
		},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: time.Now().UnixNano()}),
		IsPod:  true,
	})

	_, err := validateCommitSandboxTarget(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hostPath")
}

func TestValidatePauseSandboxTargetAllowsHostPath(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sandbox"},
	}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox",
			Config: &cubeboxv1.ContainerConfig{
				VolumeMounts: []*cubeboxv1.VolumeMounts{{
					Name:          "root",
					ContainerPath: "/",
				}, {
					Name:     "host",
					HostPath: "/var/lib/data",
				}},
			},
		},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: time.Now().UnixNano()}),
		IsPod:  true,
	})

	root, err := validatePauseSandboxTarget(cb)
	require.NoError(t, err)
	assert.Equal(t, "root", root)
}

func TestValidatePauseSandboxTargetAllowsHostDirVolume(t *testing.T) {
	cb := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{{
		Name: "host",
		VolumeSource: &cubeboxv1.VolumeSource{
			HostDirVolumes: &cubeboxv1.HostDirVolumeSources{
				VolumeSources: []*cubeboxv1.HostDirSource{{
					Name:     "host",
					HostPath: "/var/lib/data",
				}},
			},
		},
	}}, []*cubeboxv1.VolumeMounts{{
		Name:          "root",
		ContainerPath: "/",
	}, {
		Name:          "host",
		ContainerPath: "/data",
	}})

	root, err := validatePauseSandboxTarget(cb)
	require.NoError(t, err)
	assert.Equal(t, "root", root)
}

func TestValidateCommitSandboxTargetRejectsHostDirVolume(t *testing.T) {
	cb := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{{
		Name: "host",
		VolumeSource: &cubeboxv1.VolumeSource{
			HostDirVolumes: &cubeboxv1.HostDirVolumeSources{
				VolumeSources: []*cubeboxv1.HostDirSource{{
					Name:     "host",
					HostPath: "/var/lib/data",
				}},
			},
		},
	}}, []*cubeboxv1.VolumeMounts{{
		Name:          "root",
		ContainerPath: "/",
	}, {
		Name:          "host",
		ContainerPath: "/data",
	}})

	_, err := validateCommitSandboxTarget(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host_dir")
}

func TestValidateCommitSandboxTargetRejectsSandboxPathHostBind(t *testing.T) {
	for _, sandboxPathType := range []string{
		cubeboxv1.SandboxPathType_Directory.String(),
		cubeboxv1.SandboxPathType_SharedBindMount.String(),
	} {
		t.Run(sandboxPathType, func(t *testing.T) {
			cb := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{{
				Name: "host",
				VolumeSource: &cubeboxv1.VolumeSource{
					SandboxPath: &cubeboxv1.SandboxPathVolumeSource{
						Type: sandboxPathType,
						Path: "/var/lib/data",
					},
				},
			}}, []*cubeboxv1.VolumeMounts{{
				Name:          "root",
				ContainerPath: "/",
			}, {
				Name:          "host",
				ContainerPath: "/data",
			}})

			_, err := validateCommitSandboxTarget(cb)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "sandbox_path")
		})
	}
}

func TestValidateCommitSandboxTargetRejectsPluginVolume(t *testing.T) {
	cb := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{{
		Name: "data",
		VolumeSource: &cubeboxv1.VolumeSource{
			PluginVolume: &volpluginv1.PluginVolumeSource{Driver: "cos-rpc"},
		},
	}}, []*cubeboxv1.VolumeMounts{{
		Name:          "root",
		ContainerPath: "/",
	}, {
		Name:          "data",
		ContainerPath: "/data/vol",
	}})

	_, err := validateCommitSandboxTarget(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin_volume")
}

func TestValidateCommitSandboxTargetRejectsPluginVolumeAnnotation(t *testing.T) {
	cb := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{{
		Name:         "data",
		VolumeSource: &cubeboxv1.VolumeSource{},
	}}, []*cubeboxv1.VolumeMounts{{
		Name:          "root",
		ContainerPath: "/",
	}, {
		Name:          "data",
		ContainerPath: "/data/vol",
	}})
	cb.Annotations = map[string]string{
		"plugin-volume-sources": `[{"name":"data","driver":"cos-rpc"}]`,
	}

	_, err := validateCommitSandboxTarget(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin_volume")
}

func TestValidatePauseSandboxTargetAllowsPluginVolume(t *testing.T) {
	cb := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{{
		Name: "data",
		VolumeSource: &cubeboxv1.VolumeSource{
			PluginVolume: &volpluginv1.PluginVolumeSource{Driver: "cos-rpc"},
		},
	}}, []*cubeboxv1.VolumeMounts{{
		Name:          "root",
		ContainerPath: "/",
	}, {
		Name:          "data",
		ContainerPath: "/data/vol",
	}})

	root, err := validatePauseSandboxTarget(cb)
	require.NoError(t, err)
	assert.Equal(t, "root", root)
}

func TestValidateCommitSandboxTargetRejectsUnknownLegacyVolumeSources(t *testing.T) {
	cb := newRunningCommitSandboxForTest(nil, []*cubeboxv1.VolumeMounts{{
		Name:          "root",
		ContainerPath: "/",
	}, {
		Name:          "data",
		ContainerPath: "/data",
	}})

	_, err := validateCommitSandboxTarget(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without persisted volume sources")
}

func TestValidateCommitSandboxTargetRequiresRunningSandbox(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sandbox"},
	}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox",
			Config: &cubeboxv1.ContainerConfig{
				VolumeMounts: []*cubeboxv1.VolumeMounts{{
					Name:          "root",
					ContainerPath: "/",
				}},
			},
		},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  true,
	})

	_, err := validateCommitSandboxTarget(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not running")
}

func newRunningCommitSandboxForTest(volumes []*cubeboxv1.Volume, mounts []*cubeboxv1.VolumeMounts) *cubeboxstore.CubeBox {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sandbox"},
		Volumes:  volumes,
	}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox",
			Config: &cubeboxv1.ContainerConfig{
				VolumeMounts: mounts,
			},
		},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: time.Now().UnixNano()}),
		IsPod:  true,
	})
	return cb
}

func TestAppSnapshotRequiresCowBeforeCreate(t *testing.T) {
	rsp, err := (&service{}).AppSnapshot(context.Background(), &cubeboxv1.AppSnapshotRequest{
		CreateRequest: &cubeboxv1.RunCubeSandboxRequest{
			RequestID: "req",
			Annotations: map[string]string{
				constants.MasterAnnotationsAppSnapshotCreate:    "true",
				constants.MasterAnnotationAppSnapshotTemplateID: "template",
			},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, rsp)
	assert.Equal(t, errorcode.ErrorCode_PreConditionFailed, rsp.GetRet().GetRetCode())
	assert.Contains(t, rsp.GetRet().GetRetMsg(), "storage_backend=cubecow")
}
