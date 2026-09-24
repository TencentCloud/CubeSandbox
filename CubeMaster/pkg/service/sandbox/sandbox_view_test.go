// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestDecidePauseViewTable(t *testing.T) {
	var (
		created  = int32(cubebox.ContainerState_CONTAINER_CREATED)
		running  = int32(cubebox.ContainerState_CONTAINER_RUNNING)
		exited   = int32(cubebox.ContainerState_CONTAINER_EXITED)
		unknown  = int32(cubebox.ContainerState_CONTAINER_UNKNOWN)
		pausing  = int32(cubebox.ContainerState_CONTAINER_PAUSING)
		paused   = int32(cubebox.ContainerState_CONTAINER_PAUSED)
		keep     pauseView
		asPause  = pauseView{override: true, status: pausing}
		asPaused = pauseView{override: true, status: paused}
		asFailed = pauseView{override: true, status: unknown, pauseErr: "boom"}
		stale    = pauseView{stale: true}
	)
	type cell struct {
		status   string
		has      bool
		observed int32
		want     pauseView
	}
	var cells []cell
	add := func(status string, has bool, observed int32, want pauseView) {
		cells = append(cells, cell{status, has, observed, want})
	}
	for _, obs := range []int32{created, running, exited, unknown} {
		add(pausesnap.StatusCreating, true, obs, asPause)
	}
	add(pausesnap.StatusCreating, false, 0, asPause)
	add(pausesnap.StatusCreating, true, pausing, keep)
	add(pausesnap.StatusCreating, true, paused, keep)

	add(pausesnap.StatusReady, false, 0, asPaused)
	add(pausesnap.StatusReady, true, running, stale)
	for _, obs := range []int32{created, exited, unknown, pausing, paused} {
		add(pausesnap.StatusReady, true, obs, asPaused)
	}

	add(pausesnap.StatusFailed, false, 0, asFailed)
	for _, obs := range []int32{created, running, exited, unknown} {
		add(pausesnap.StatusFailed, true, obs, asFailed)
	}
	add(pausesnap.StatusFailed, true, pausing, keep)
	add(pausesnap.StatusFailed, true, paused, keep)

	add(pausesnap.StatusDeleteFailed, false, 0, asPaused)
	add(pausesnap.StatusDeleteFailed, true, running, keep)
	for _, obs := range []int32{created, exited, unknown, pausing, paused} {
		add(pausesnap.StatusDeleteFailed, true, obs, asPaused)
	}

	add("nope", false, 0, keep)
	add("nope", true, running, keep)
	add(" ready ", true, running, stale)
	add("creating", false, 0, asPause)

	for _, tc := range cells {
		t.Run(tc.status+"/"+stateName(tc.has, tc.observed), func(t *testing.T) {
			got := decidePauseView(&pausesnap.Record{Status: tc.status, LastError: "boom", SnapshotID: "snap"}, tc.observed, tc.has)
			require.Equal(t, tc.want, got)
		})
	}
	require.Equal(t, keep, decidePauseView(nil, running, true))
}

func stateName(has bool, observed int32) string {
	if !has {
		return "none"
	}
	return cubebox.ContainerState(observed).String()
}

func TestDecidePauseViewFailedMessageFallback(t *testing.T) {
	got := decidePauseView(&pausesnap.Record{Status: pausesnap.StatusFailed}, 0, false)
	require.True(t, got.override)
	require.Equal(t, "pause failed; sandbox may be unrecoverable", got.pauseErr)
}

func specWithIdentity() *types.CreateCubeSandboxReq {
	return &types.CreateCubeSandboxReq{
		Namespace: "ns-a",
		Backend:   constants.SnapshotBackendXFS,
		Labels: map[string]string{
			"app": "demo",
			constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
			constants.CubeAnnotationPauseSnapshotID:       "forged-snap",
		},
		Annotations: map[string]string{
			AnnotationHostDirMount:                        `[{"hostPath":"/data/shared/a","mountPath":"/mnt"}]`,
			constants.CubeAnnotationStorageBackend:        "s3",
			constants.CubeAnnotationAppSnapshotTemplateID: "tpl-from-annotation",
		},
		Containers: []*types.Container{
			{
				Name:  "main",
				Image: &types.ImageSpec{Image: "img:1"},
				Resources: &types.Resource{
					Cpu: "500m",
					Mem: "512Mi",
				},
				VolumeMounts: []*cubebox.VolumeMounts{
					{Name: "root", ContainerPath: "/"},
					{Name: "data", ContainerPath: "/data"},
				},
			},
			{
				Name: "side",
				VolumeMounts: []*cubebox.VolumeMounts{
					{Name: "data", ContainerPath: "/data"},
					{Name: "extra", ContainerPath: "/extra", Readonly: true},
				},
			},
		},
	}
}

func TestIdentityFromSpecStripsReservedLabels(t *testing.T) {
	id := identityFromSpec(specWithIdentity())
	require.Equal(t, "demo", id.labels["app"])
	require.NotContains(t, id.labels, constants.CubeAnnotationPauseSnapshotID)
	require.Equal(t, "tpl-1", id.templateID)
}

func TestIdentityFromSpecTemplateIDFallback(t *testing.T) {
	spec := specWithIdentity()
	delete(spec.Labels, constants.CubeAnnotationAppSnapshotTemplateID)
	id := identityFromSpec(spec)
	require.Equal(t, "tpl-from-annotation", id.templateID)
	require.Equal(t, "tpl-from-annotation", id.annotations[constants.CubeAnnotationAppSnapshotTemplateID])

	spec.Labels[constants.CubeAnnotationAppSnapshotTemplateID] = "tpl-label"
	id = identityFromSpec(spec)
	require.Equal(t, "tpl-label", id.templateID)
}

func TestIdentityFromSpecDoesNotLeakSpecAnnotations(t *testing.T) {
	id := identityFromSpec(specWithIdentity())
	require.NotContains(t, id.annotations, AnnotationHostDirMount)
	require.NotContains(t, id.annotations, constants.CubeAnnotationStorageBackend)
}

func TestSandboxDataFromSpecPrimaryContainer(t *testing.T) {
	one := sandboxDataFromSpec("sb-1", specWithIdentity())
	require.Equal(t, "sb-1", one.SandboxID)
	require.Equal(t, "demo", one.Labels["app"])
	require.Equal(t, "tpl-1", one.TemplateID)
	require.Equal(t, "ns-a", one.NameSpace)
	require.Len(t, one.Containers, 1)
	primary := one.Containers[0]
	require.Equal(t, "sb-1", primary.ContainerID)
	require.Equal(t, "sandbox", primary.Type)
	require.Equal(t, "main", primary.Name)
	require.Equal(t, "img:1", primary.Image)
	require.Equal(t, "500m", primary.Cpu)
	require.Equal(t, "512Mi", primary.Mem)
	require.Equal(t, parseCPUMilli("500m"), primary.CpuMilli)
	require.Equal(t, parseMemoryMiB("512Mi"), primary.MemoryMiB)
	require.Zero(t, primary.CreateAt)
}

func TestSandboxDataFromSpecVolumeMounts(t *testing.T) {
	one := sandboxDataFromSpec("sb-1", specWithIdentity())
	require.Len(t, one.VolumeMounts, 2)
	require.Equal(t, "data", one.VolumeMounts[0].Name)
	require.Equal(t, "/extra", one.VolumeMounts[1].ContainerPath)
	require.True(t, one.VolumeMounts[1].Readonly)
}

func TestSandboxDataFromSpecNil(t *testing.T) {
	one := sandboxDataFromSpec("sb-1", nil)
	require.Equal(t, "sb-1", one.SandboxID)
	require.Empty(t, one.Labels)
	require.Empty(t, one.Containers)
}

func TestSpecIdentityConsistentAcrossInfoAndList(t *testing.T) {
	spec := specWithIdentity()
	rec := &pausesnap.Record{
		SandboxID:  "sb-1",
		SnapshotID: "snap-1",
		Status:     pausesnap.StatusReady,
		NodeID:     "node-a",
		NodeIP:     "10.0.0.1",
	}
	info := sandboxDataFromSpec("sb-1", spec)
	setSandboxDataStatus(info, int32(cubebox.ContainerState_CONTAINER_PAUSED))
	info.Annotations = overlayPauseAnnotations(info.Annotations, rec, "")

	brief := briefFromPauseBinding(rec, spec)
	brief.Annotations = overlayPauseAnnotations(brief.Annotations, rec, "")

	require.Equal(t, info.Labels, brief.Labels)
	require.Equal(t, info.TemplateID, brief.TemplateID)
	require.Equal(t, info.Annotations, brief.Annotations)
	require.Equal(t, info.NameSpace, brief.NameSpace)
	require.Equal(t, info.VolumeMounts, brief.VolumeMounts)
	require.Equal(t, info.Containers[0].CpuMilli, brief.CPUMilli)
	require.Equal(t, info.Containers[0].MemoryMiB, brief.MemoryMiB)
	require.Equal(t, info.Status, brief.Status)
	require.Equal(t, "snap-1", brief.Annotations[constants.CubeAnnotationPauseSnapshotID])
}

func TestSetSandboxDataStatusOnlyTouchesPrimary(t *testing.T) {
	item := &types.SandboxData{
		SandboxID: "sb-1",
		Containers: []*types.ContainerInfo{
			{ContainerID: "side", Status: int32(cubebox.ContainerState_CONTAINER_RUNNING)},
			{ContainerID: "sb-1", Status: int32(cubebox.ContainerState_CONTAINER_RUNNING)},
		},
	}
	setSandboxDataStatus(item, int32(cubebox.ContainerState_CONTAINER_PAUSING))
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSING), item.Status)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_RUNNING), item.Containers[0].Status)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSING), item.Containers[1].Status)

	bare := &types.SandboxData{SandboxID: "sb-2"}
	setSandboxDataStatus(bare, int32(cubebox.ContainerState_CONTAINER_PAUSED))
	require.Len(t, bare.Containers, 1)
	require.Equal(t, "sb-2", bare.Containers[0].ContainerID)
	require.Equal(t, "sandbox", bare.Containers[0].Type)
}

func TestOverlayPauseAnnotationsKeepsIdentity(t *testing.T) {
	ann := overlayPauseAnnotations(map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
	}, &pausesnap.Record{SnapshotID: "snap-1"}, "disk full")
	require.Equal(t, "tpl-1", ann[constants.CubeAnnotationAppSnapshotTemplateID])
	require.Equal(t, "snap-1", ann[constants.CubeAnnotationPauseSnapshotID])
	require.Equal(t, "disk full", ann[constants.CubeAnnotationPauseError])
}

func pauseStaleCount(path string) float64 {
	metric := &dto.Metric{}
	if err := pauseBindingStaleTotal.WithLabelValues(path).Write(metric); err != nil {
		return -1
	}
	return metric.GetCounter().GetValue()
}

func TestRecordStalePauseBindingIncrementsMetric(t *testing.T) {
	rec := &pausesnap.Record{SnapshotID: "snap", NodeIP: "10.0.0.1"}
	for _, path := range []string{"info", "list"} {
		before := pauseStaleCount(path)
		recordStalePauseBinding(context.Background(), path, "sb", rec)
		require.Equal(t, before+1, pauseStaleCount(path))
	}
}

func TestSandboxSpecCarriesInjectedMounts(t *testing.T) {
	ensureSandboxTestConfig(t)
	req := &types.CreateCubeSandboxReq{
		Request: &types.Request{RequestID: "req-mount"},
		Containers: []*types.Container{{
			Name:      "main",
			Image:     &types.ImageSpec{Image: "busybox:latest"},
			Resources: &types.Resource{Cpu: "1", Mem: "1Gi"},
		}},
		Annotations: map[string]string{
			AnnotationHostDirMount: `[{"hostPath":"/data/shared/data","mountPath":"/mnt/data","readOnly":true}]`,
		},
	}
	if _, err := ConstructCubeletReq(context.Background(), req); err != nil {
		t.Fatalf("ConstructCubeletReq: %v", err)
	}
	require.NotEmpty(t, req.Containers[0].VolumeMounts)
	require.Equal(t, "hostdir-0", req.Containers[0].VolumeMounts[0].GetName())
	require.Equal(t, "/mnt/data", req.Containers[0].VolumeMounts[0].GetContainerPath())
}
