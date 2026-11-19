// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

// NormalizeWritableLayer returns "disk/<name>", or empty if it is not one directory.
func NormalizeWritableLayer(layer string) string {
	layer = strings.TrimSpace(layer)
	const prefix = "disk/"
	name, ok := strings.CutPrefix(layer, prefix)
	if !ok || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return prefix + name
}

// WritableLayerFromContainerID returns "disk/<id>" for a plain container id.
func WritableLayerFromContainerID(containerID string) string {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" || strings.ContainsAny(containerID, `/\`) {
		return ""
	}
	return NormalizeWritableLayer("disk/" + containerID)
}

// EffectiveWritableLayer is the overlay upper this sandbox has mounted.
func (cb *CubeBox) EffectiveWritableLayer(ctx context.Context) string {
	if cb == nil || mainRootfsReadonly(cb) {
		return ""
	}
	if layer := NormalizeWritableLayer(cb.RootfsWritableLayer); layer != "" {
		return layer
	}
	if layer := writableLayerFromLiveSpec(ctx, cb); layer != "" {
		return layer
	}
	return writableLayerFromAnnotations(cb.Annotations, cb.SandboxID)
}

// ReconcileRecoveredWritableLayer fills an empty RootfsWritableLayer from the running sandbox.
func (cb *CubeBox) ReconcileRecoveredWritableLayer(ctx context.Context) {
	if cb == nil || NormalizeWritableLayer(cb.RootfsWritableLayer) != "" || mainRootfsReadonly(cb) {
		return
	}
	if layer := cb.EffectiveWritableLayer(ctx); layer != "" {
		cb.RootfsWritableLayer = layer
	}
}

func mainRootfsReadonly(cb *CubeBox) bool {
	container := cb.FirstContainer()
	if container == nil || container.Config == nil || container.Config.GetSecurityContext() == nil {
		return false
	}
	return container.Config.GetSecurityContext().GetReadonlyRootfs()
}

func writableLayerFromLiveSpec(ctx context.Context, cb *CubeBox) string {
	container := cb.FirstContainer()
	if container == nil || container.Container == nil {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	spec, err := container.Container.Spec(ctx)
	if err != nil || spec == nil || spec.Annotations == nil {
		return ""
	}
	annotations := spec.Annotations
	// A memory restore's container id is the mounted layer. The wlayer annotation is not.
	if strings.TrimSpace(annotations[constants.AnnotationAppSnapshotRestore]) == "true" {
		if layer := WritableLayerFromContainerID(annotations[constants.AnnotationAppSnapshotContainerID]); layer != "" {
			return layer
		}
	}
	return NormalizeWritableLayer(annotations[constants.AnnotationsRootfsWlayerSubdir])
}

func writableLayerFromAnnotations(annotations map[string]string, sandboxID string) string {
	if annotations != nil {
		id := strings.TrimSpace(annotations[constants.MasterAnnotationJFSTemplateVariantID])
		if id == "" {
			id = strings.TrimSpace(annotations[constants.MasterAnnotationAppSnapshotTemplateID])
		}
		if layer := WritableLayerFromContainerID(id + "_0"); id != "" && layer != "" {
			return layer
		}
	}
	return WritableLayerFromContainerID(sandboxID)
}
