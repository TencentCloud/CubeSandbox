// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const (
	// AnnotationHostDirMount must match the canonical annotation key that CubeAPI
	// writes when it lifts metadata["host-mount"] onto the sandbox
	// CreateSandboxRequest; see CubeAPI/src/handlers/sandboxes.rs
	// (const HOSTDIR_MOUNT_KEY). Keep these in lockstep, otherwise
	// host-mount requests are silently dropped.
	AnnotationHostDirMount       = "host-mount"
	AnnotationHostDirMountLegacy = "hostdir-mount"
)

type HostDirMountOption struct {
	HostPath string `json:"hostPath"`

	MountPath string `json:"mountPath"`

	ReadOnly bool `json:"readOnly,omitempty"`
}

func injectHostDirMounts(ctx context.Context, req *types.CreateCubeSandboxReq) error {
	if req.Annotations == nil {
		log.G(ctx).Infof("[hostdir] no annotations, skip")
		return nil
	}
	annotationKey := AnnotationHostDirMount
	raw, ok := req.Annotations[annotationKey]
	if !ok || strings.TrimSpace(raw) == "" {
		annotationKey = AnnotationHostDirMountLegacy
		raw, ok = req.Annotations[annotationKey]
	}
	if !ok || strings.TrimSpace(raw) == "" {
		log.G(ctx).Infof("[hostdir] annotation %q/%q absent or empty, skip", AnnotationHostDirMount, AnnotationHostDirMountLegacy)
		return nil
	}
	log.G(ctx).Infof("[hostdir] raw annotation %q: %s", annotationKey, raw)

	var opts []HostDirMountOption
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return fmt.Errorf("invalid %q annotation: %w", annotationKey, err)
	}
	if len(opts) == 0 {
		log.G(ctx).Infof("[hostdir] annotation parsed to empty list, skip")
		return nil
	}
	log.G(ctx).Infof("[hostdir] parsed %d mount option(s)", len(opts))

	for i, o := range opts {
		if !strings.HasPrefix(o.HostPath, "/") {
			return fmt.Errorf("%q entry[%d]: hostPath must be an absolute path, got %q",
				annotationKey, i, o.HostPath)
		}
		if !strings.HasPrefix(o.MountPath, "/") {
			return fmt.Errorf("%q entry[%d]: mountPath must be an absolute path, got %q",
				annotationKey, i, o.MountPath)
		}
	}

	for i, o := range opts {
		name := fmt.Sprintf("hostdir-%d", i)
		vol := &types.Volume{
			Name: name,
			VolumeSource: &types.VolumeSource{
				HostDirVolumeSources: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{
						{
							Name:     name,
							HostPath: o.HostPath,
						},
					},
				},
			},
		}
		req.Volumes = append(req.Volumes, vol)
		log.G(ctx).Infof("[hostdir] injected Volume %q hostPath=%s", name, o.HostPath)
	}

	vm := make([]*cubeboxv1.VolumeMounts, 0, len(opts))
	for i, o := range opts {
		name := fmt.Sprintf("hostdir-%d", i)
		vm = append(vm, &cubeboxv1.VolumeMounts{
			Name:          name,
			ContainerPath: o.MountPath,
			Readonly:      o.ReadOnly,
		})
		log.G(ctx).Infof("[hostdir] injected VolumeMount %q containerPath=%s readOnly=%v", name, o.MountPath, o.ReadOnly)
	}
	for _, c := range req.Containers {
		c.VolumeMounts = append(c.VolumeMounts, vm...)
	}

	return nil
}
