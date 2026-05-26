// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

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

	assert.Equal(t, int32(1), s.getNextNumaNode(), "First call should return 1")

	assert.Equal(t, int32(0), s.getNextNumaNode(), "Second call should return 0")
	assert.Equal(t, int32(1), s.getNextNumaNode(), "Third call should return 1")
	assert.Equal(t, int32(0), s.getNextNumaNode(), "Fourth call should return 0")

	for i := 0; i < 10; i++ {
		s.getNextNumaNode()
	}
	assert.Equal(t, int32(1), s.getNextNumaNode(), "After multiple calls, it should still cycle between 0 and 1")
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

func TestWriteMemoryDevFile(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, writeMemoryDevFile(dir, "/dev/mapper/tpl-snapshot-memory"))

	got, err := os.ReadFile(filepath.Join(dir, "memory.dev"))
	require.NoError(t, err)
	assert.Equal(t, "/dev/mapper/tpl-snapshot-memory\n", string(got))
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
