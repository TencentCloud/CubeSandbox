// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const (
	// AnnotationHostDirMount must match the annotation key that CubeAPI
	// writes when it lifts metadata["host-mount"] onto the sandbox
	// CreateSandboxRequest; see CubeAPI/src/handlers/sandboxes.rs
	// (const HOSTDIR_MOUNT_KEY). Keep these in lockstep, otherwise
	// host-mount requests are silently dropped.
	AnnotationHostDirMount = "host-mount"
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
	raw, ok := req.Annotations[AnnotationHostDirMount]
	if !ok || strings.TrimSpace(raw) == "" {
		log.G(ctx).Infof("[hostdir] annotation %q absent or empty, skip", AnnotationHostDirMount)
		return nil
	}
	log.G(ctx).Infof("[hostdir] raw annotation: %s", raw)

	var opts []HostDirMountOption
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return fmt.Errorf("invalid %q annotation: %w", AnnotationHostDirMount, err)
	}
	if len(opts) == 0 {
		log.G(ctx).Infof("[hostdir] annotation parsed to empty list, skip")
		return nil
	}
	log.G(ctx).Infof("[hostdir] parsed %d mount option(s)", len(opts))

	for i, o := range opts {
		if !strings.HasPrefix(o.HostPath, "/") {
			return fmt.Errorf("%q entry[%d]: hostPath must be an absolute path, got %q",
				AnnotationHostDirMount, i, o.HostPath)
		}
		if !strings.HasPrefix(o.MountPath, "/") {
			return fmt.Errorf("%q entry[%d]: mountPath must be an absolute path, got %q",
				AnnotationHostDirMount, i, o.MountPath)
		}
		cleaned, err := validateHostPath(o.HostPath)
		if err != nil {
			return fmt.Errorf("%q entry[%d]: %w", AnnotationHostDirMount, i, err)
		}
		opts[i].HostPath = cleaned
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

// validateHostPath checks that hostPath falls under one of the configured
// allowed prefixes (see config.GetAllowedHostMountPrefixes). It resolves
// ".." to prevent path-traversal bypasses and returns the cleaned path.
func validateHostPath(hostPath string) (string, error) {
	allowedPrefixes := config.GetAllowedHostMountPrefixes()
	cleaned := filepath.Clean(hostPath)
	check := cleaned + "/"
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(check, prefix) {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("hostPath %q is not within an allowed mount prefix", hostPath)
}

// AnnotationPluginVolumeMounts is the annotation key CubeAPI uses to forward
// VolumeMount entries for plugin_volume volumes.  The value is a JSON array of
// {name, container_path, readonly?} objects.
const AnnotationPluginVolumeMounts = "plugin-volume-mounts"

// pluginVolumeMountEntry mirrors the VolumeMount struct sent by CubeAPI.
type pluginVolumeMountEntry struct {
	Name          string `json:"name"`
	ContainerPath string `json:"container_path"`
	Readonly      bool   `json:"readonly,omitempty"`
}

// injectPluginVolumeMounts reads the "plugin-volume-mounts" annotation and
// appends the corresponding VolumeMounts to every container in the request.
// This is the counterpart to CubeAPI's annotation-based forwarding of
// volume_mounts for plugin_volume volumes.
func injectPluginVolumeMounts(ctx context.Context, req *types.CreateCubeSandboxReq) error {
	if req.Annotations == nil {
		return nil
	}
	raw, ok := req.Annotations[AnnotationPluginVolumeMounts]
	if !ok || raw == "" {
		return nil
	}

	var entries []pluginVolumeMountEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return fmt.Errorf("injectPluginVolumeMounts: parse annotation: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}

	log.G(ctx).Infof("[plugin-volume] injectPluginVolumeMounts: %d mount(s)", len(entries))

	for i := range req.Containers {
		ctr := req.Containers[i]
		if ctr == nil {
			continue
		}
		for _, e := range entries {
			vm := &cubeboxv1.VolumeMounts{
				Name:          e.Name,
				ContainerPath: e.ContainerPath,
				Readonly:      e.Readonly,
			}
			ctr.VolumeMounts = append(ctr.VolumeMounts, vm)
			log.G(ctx).Infof("[plugin-volume] injected VolumeMount %q → %s (ro=%v) into container %s",
				e.Name, e.ContainerPath, e.Readonly, ctr.Name)
		}
	}
	return nil
}
