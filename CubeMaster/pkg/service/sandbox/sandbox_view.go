// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// pauseView is what a pause binding contributes on top of a node observation.
// Identity fields never come from the binding.
type pauseView struct {
	// override is false when the node's own status is reported as-is.
	override bool
	status   int32
	// stale marks a READY binding left behind for a sandbox the node reports running.
	stale    bool
	pauseErr string
}

// decidePauseView is the only place that maps a pause binding plus a node
// observation onto the status Info and List report.
func decidePauseView(rec *pausesnap.Record, observed int32, hasObserved bool) pauseView {
	if rec == nil {
		return pauseView{}
	}
	var (
		running = int32(cubebox.ContainerState_CONTAINER_RUNNING)
		pausing = int32(cubebox.ContainerState_CONTAINER_PAUSING)
		paused  = int32(cubebox.ContainerState_CONTAINER_PAUSED)
		unknown = int32(cubebox.ContainerState_CONTAINER_UNKNOWN)
	)
	nodePausing := hasObserved && (observed == pausing || observed == paused)
	nodeRunning := hasObserved && observed == running

	switch strings.ToUpper(strings.TrimSpace(rec.Status)) {
	case pausesnap.StatusCreating:
		if nodePausing {
			return pauseView{}
		}
		return pauseView{override: true, status: pausing}
	case pausesnap.StatusReady:
		if nodeRunning {
			return pauseView{stale: true}
		}
		return pauseView{override: true, status: paused}
	case pausesnap.StatusFailed:
		if nodePausing {
			return pauseView{}
		}
		return pauseView{override: true, status: unknown, pauseErr: pauseFailureMessage(rec)}
	case pausesnap.StatusDeleteFailed:
		if nodeRunning {
			return pauseView{}
		}
		return pauseView{override: true, status: paused}
	default:
		return pauseView{}
	}
}

func pauseFailureMessage(rec *pausesnap.Record) string {
	if rec != nil {
		if msg := strings.TrimSpace(rec.LastError); msg != "" {
			return msg
		}
	}
	return "pause failed; sandbox may be unrecoverable"
}

// isShimlessPauseStatus reports whether the binding, not a node scan, is the
// only remaining evidence of the sandbox.
func isShimlessPauseStatus(status string) bool {
	status = strings.TrimSpace(status)
	return strings.EqualFold(status, pausesnap.StatusReady) ||
		strings.EqualFold(status, pausesnap.StatusDeleteFailed)
}

// overlayPauseAnnotations adds the pause snapshot id, and the pause error
// when there is one, without touching identity annotations.
func overlayPauseAnnotations(ann map[string]string, rec *pausesnap.Record, pauseErr string) map[string]string {
	if rec == nil {
		return ann
	}
	if ann == nil {
		ann = map[string]string{}
	}
	if id := strings.TrimSpace(rec.SnapshotID); id != "" {
		ann[constants.CubeAnnotationPauseSnapshotID] = id
	}
	if pauseErr != "" {
		ann[constants.CubeAnnotationPauseError] = pauseErr
	}
	return ann
}

// setSandboxDataStatus sets the sandbox status and the primary container
// status. A view with no primary container gets one bare container so callers
// still see a status.
func setSandboxDataStatus(item *types.SandboxData, status int32) {
	if item == nil {
		return
	}
	item.Status = status
	for _, c := range item.Containers {
		if c != nil && c.ContainerID == item.SandboxID {
			c.Status = status
			return
		}
	}
	item.Containers = append(item.Containers, &types.ContainerInfo{
		ContainerID: item.SandboxID,
		Status:      status,
		Type:        "sandbox",
	})
}

// specIdentity is the part of a sandbox view that does not change after
// create, taken from the persisted create request.
type specIdentity struct {
	labels       map[string]string
	templateID   string
	annotations  map[string]string
	namespace    string
	primary      *types.Container
	volumeMounts []*types.VolumeMountInfo
}

func identityFromSpec(spec *types.CreateCubeSandboxReq) *specIdentity {
	if spec == nil {
		return nil
	}
	labels := stripUserCubeMasterLabels(spec.Labels)
	id := &specIdentity{
		labels:      labels,
		templateID:  templateIDFromLabels(labels),
		annotations: buildAnnotationsFromLabels(labels),
		namespace:   spec.Namespace,
	}
	if id.templateID == "" && spec.Annotations != nil {
		id.templateID = strings.TrimSpace(spec.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
		if id.templateID != "" {
			if id.annotations == nil {
				id.annotations = map[string]string{}
			}
			id.annotations[constants.CubeAnnotationAppSnapshotTemplateID] = id.templateID
		}
	}
	var mounts []*cubebox.VolumeMounts
	for _, c := range spec.Containers {
		if c == nil {
			continue
		}
		if id.primary == nil {
			id.primary = c
		}
		mounts = append(mounts, c.VolumeMounts...)
	}
	id.volumeMounts = volumeMountsToContainerInfo(mounts)
	return id
}

// sandboxDataFromSpec renders a sandbox the node did not report. CreateAt
// stays zero: the spec does not record when the sandbox started.
func sandboxDataFromSpec(sandboxID string, spec *types.CreateCubeSandboxReq) *types.SandboxData {
	one := &types.SandboxData{SandboxID: sandboxID}
	id := identityFromSpec(spec)
	if id == nil {
		return one
	}
	one.Labels = id.labels
	one.TemplateID = id.templateID
	one.Annotations = id.annotations
	one.NameSpace = id.namespace
	one.VolumeMounts = id.volumeMounts
	primary := &types.ContainerInfo{ContainerID: sandboxID, Type: "sandbox"}
	if c := id.primary; c != nil {
		primary.Name = c.Name
		if c.Image != nil {
			primary.Image = c.Image.Image
		}
		if c.Resources != nil {
			primary.Cpu = c.Resources.Cpu
			primary.Mem = c.Resources.Mem
			primary.CpuMilli = parseCPUMilli(c.Resources.Cpu)
			primary.MemoryMiB = parseMemoryMiB(c.Resources.Mem)
		}
	}
	one.Containers = []*types.ContainerInfo{primary}
	return one
}

// pauseBindingRow renders a binding the node did not report. CreateAt stays
// empty: the binding only knows when the pause happened, not when the sandbox
// was created.
func pauseBindingRow(rec *pausesnap.Record) *types.SandboxBriefData {
	if rec == nil {
		return nil
	}
	item := &types.SandboxBriefData{
		SandboxID:       rec.SandboxID,
		Status:          int32(cubebox.ContainerState_CONTAINER_PAUSED),
		HostID:          rec.NodeID,
		HostIP:          rec.NodeIP,
		Backend:         strings.TrimSpace(rec.Backend),
		PauseSnapshotID: rec.SnapshotID,
		RemoteStatus:    rec.RemoteStatus,
		PauseStatus:     strings.TrimSpace(rec.Status),
	}
	if !rec.UpdatedAt.IsZero() {
		item.PauseAt = rec.UpdatedAt.UnixNano()
	}
	return item
}

func briefFromPauseBinding(rec *pausesnap.Record, spec *types.CreateCubeSandboxReq) *types.SandboxBriefData {
	item := pauseBindingRow(rec)
	if item == nil {
		return nil
	}
	id := identityFromSpec(spec)
	if id == nil {
		return item
	}
	item.Labels = id.labels
	item.TemplateID = id.templateID
	item.Annotations = id.annotations
	item.NameSpace = id.namespace
	item.VolumeMounts = id.volumeMounts
	if c := id.primary; c != nil && c.Resources != nil {
		item.CpuCount = parseCPUCount(c.Resources.Cpu)
		item.MemoryMB = parseMemoryMB(c.Resources.Mem)
		item.CPUMilli = parseCPUMilli(c.Resources.Cpu)
		item.MemoryMiB = parseMemoryMiB(c.Resources.Mem)
	}
	if strings.TrimSpace(item.Backend) == "" && spec != nil {
		item.Backend = strings.TrimSpace(spec.Backend)
	}
	return item
}

var pauseBindingStaleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cube_pause_binding_stale_total",
	Help: "READY pause bindings observed for sandboxes the node reports running, by read path.",
}, []string{"path"})

func recordStalePauseBinding(ctx context.Context, path, sandboxID string, rec *pausesnap.Record) {
	if rec == nil {
		return
	}
	pauseBindingStaleTotal.WithLabelValues(path).Inc()
	log.G(ctx).Warnf("stale pause binding: sandbox=%s snapshot=%s node=%s reported running",
		sandboxID, rec.SnapshotID, rec.NodeIP)
}
