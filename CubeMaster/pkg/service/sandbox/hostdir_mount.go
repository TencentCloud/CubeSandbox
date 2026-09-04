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

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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

// createRequestHasHostMount reports whether the persisted create spec binds a
// host directory. Resume uses this to pin placement to the origin node.
func createRequestHasHostMount(req *types.CreateCubeSandboxReq) bool {
	if req == nil {
		return false
	}
	if req.Annotations != nil {
		raw := strings.TrimSpace(req.Annotations[AnnotationHostDirMount])
		if raw != "" && raw != "[]" && !strings.EqualFold(raw, "null") {
			var opts []HostDirMountOption
			if err := json.Unmarshal([]byte(raw), &opts); err != nil {
				return true
			}
			for _, o := range opts {
				if strings.TrimSpace(o.HostPath) != "" || strings.TrimSpace(o.MountPath) != "" {
					return true
				}
			}
		}
	}
	for _, vol := range req.Volumes {
		if vol == nil || vol.VolumeSource == nil || vol.VolumeSource.HostDirVolumeSources == nil {
			continue
		}
		for _, src := range vol.VolumeSource.HostDirVolumeSources.VolumeSources {
			if src != nil && strings.TrimSpace(src.HostPath) != "" {
				return true
			}
		}
	}
	return false
}

// CreateRequestHasHostMount reports whether a create request carries a raw
// host-mount dependency. Snapshot restore uses it to keep that dependency on
// its origin node.
func CreateRequestHasHostMount(req *types.CreateCubeSandboxReq) bool {
	return createRequestHasHostMount(req)
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
		if err := ensureHostDirVolume(req, name, o.HostPath); err != nil {
			return err
		}
		for _, c := range req.Containers {
			if err := ensureHostDirVolumeMount(c, name, o); err != nil {
				return err
			}
		}
		log.G(ctx).Infof("[hostdir] ensured Volume and VolumeMount %q hostPath=%s containerPath=%s readOnly=%v",
			name, o.HostPath, o.MountPath, o.ReadOnly)
	}

	return nil
}

func ensureHostDirVolume(req *types.CreateCubeSandboxReq, name, hostPath string) error {
	var existing *types.Volume
	for _, volume := range req.Volumes {
		if volume == nil || volume.Name != name {
			continue
		}
		if existing != nil {
			return fmt.Errorf("host-mount volume %q is duplicated before injection", name)
		}
		existing = volume
	}
	if existing == nil {
		req.Volumes = append(req.Volumes, &types.Volume{
			Name: name,
			VolumeSource: &types.VolumeSource{
				HostDirVolumeSources: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{{
						Name:     name,
						HostPath: hostPath,
					}},
				},
			},
		})
		return nil
	}

	if existing.VolumeSource == nil {
		return fmt.Errorf("host-mount volume %q conflicts with existing volume source", name)
	}
	hostDirs := existing.VolumeSource.HostDirVolumeSources
	if hostDirs == nil || len(hostDirs.VolumeSources) != 1 {
		return fmt.Errorf("host-mount volume %q conflicts with existing volume source", name)
	}
	source := hostDirs.VolumeSources[0]
	if source == nil || source.Name != name || filepath.Clean(source.HostPath) != hostPath {
		return fmt.Errorf("host-mount volume %q conflicts with existing hostPath", name)
	}
	return nil
}

func ensureHostDirVolumeMount(container *types.Container, name string, option HostDirMountOption) error {
	if container == nil {
		return nil
	}
	var existing *cubeboxv1.VolumeMounts
	for _, mount := range container.VolumeMounts {
		if mount == nil || mount.GetName() != name {
			continue
		}
		if existing != nil {
			return fmt.Errorf("host-mount volume mount %q is duplicated before injection", name)
		}
		existing = mount
	}
	if existing == nil {
		container.VolumeMounts = append(container.VolumeMounts, &cubeboxv1.VolumeMounts{
			Name:          name,
			ContainerPath: option.MountPath,
			HostPath:      option.HostPath,
			Readonly:      option.ReadOnly,
		})
		return nil
	}
	if filepath.Clean(existing.GetHostPath()) != option.HostPath ||
		filepath.Clean(existing.GetContainerPath()) != filepath.Clean(option.MountPath) ||
		existing.GetReadonly() != option.ReadOnly {
		return fmt.Errorf("host-mount volume mount %q conflicts with existing mount", name)
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
