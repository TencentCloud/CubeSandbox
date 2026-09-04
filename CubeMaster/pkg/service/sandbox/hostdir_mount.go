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

const (
	// AnnotationPluginVolumeMounts is the annotation key CubeAPI uses to
	// forward VolumeMount entries for plugin_volume volumes.
	AnnotationPluginVolumeMounts = "plugin-volume-mounts"
	// AnnotationPluginVolumeSources is generated by CubeMaster for Cubelet.
	// It is runtime metadata and must be rebuilt from VolumeRecord on create.
	AnnotationPluginVolumeSources = "plugin-volume-sources"
)

// pluginVolumeMountEntry mirrors the VolumeMount struct sent by CubeAPI.
type pluginVolumeMountEntry struct {
	Name          string `json:"name"`
	ContainerPath string `json:"container_path"`
	Readonly      bool   `json:"readonly,omitempty"`
}

// CreateRequestHasPluginVolume reports whether a create request depends on a
// managed plugin volume. Reference-only snapshots use this to stay on their
// origin node until drivers can declare portability and topology.
func CreateRequestHasPluginVolume(req *types.CreateCubeSandboxReq) bool {
	if req == nil {
		return false
	}
	for _, key := range []string{AnnotationPluginVolumeMounts, AnnotationPluginVolumeSources} {
		raw := strings.TrimSpace(req.Annotations[key])
		if raw != "" && raw != "[]" && !strings.EqualFold(raw, "null") {
			return true
		}
	}
	for _, volume := range req.Volumes {
		if volume != nil && volume.Name != "" &&
			(volume.VolumeSource == nil || volume.VolumeSource.PluginVolume != nil) {
			return true
		}
	}
	return false
}

// injectPluginVolumeMounts reads the "plugin-volume-mounts" annotation and
// ensures the corresponding VolumeMounts exist on every container.
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
	seen := make(map[string]struct{}, len(entries))
	declared := make(map[string]int, len(req.Volumes))
	for _, volume := range req.Volumes {
		if volume != nil && strings.TrimSpace(volume.Name) != "" {
			declared[strings.TrimSpace(volume.Name)]++
		}
	}
	for i, entry := range entries {
		entry.Name = strings.TrimSpace(entry.Name)
		entry.ContainerPath = filepath.Clean(entry.ContainerPath)
		if entry.Name == "" {
			return fmt.Errorf("plugin-volume-mounts entry[%d]: name must not be empty", i)
		}
		if !filepath.IsAbs(entry.ContainerPath) {
			return fmt.Errorf("plugin-volume-mounts entry[%d]: container_path must be absolute", i)
		}
		if entry.ContainerPath == "/" {
			return fmt.Errorf("plugin-volume-mounts entry[%d]: container_path / is reserved for rootfs", i)
		}
		if declared[entry.Name] != 1 {
			return fmt.Errorf("plugin-volume-mounts entry[%d]: volume %q must have exactly one declaration", i, entry.Name)
		}
		if _, ok := seen[entry.Name]; ok {
			return fmt.Errorf("plugin-volume-mounts entry[%d]: volume %q is duplicated", i, entry.Name)
		}
		seen[entry.Name] = struct{}{}
		entries[i] = entry
	}

	for i := range req.Containers {
		ctr := req.Containers[i]
		if ctr == nil {
			continue
		}
		for _, e := range entries {
			if err := ensurePluginVolumeMount(ctr, e); err != nil {
				return err
			}
			log.G(ctx).Infof("[plugin-volume] ensured VolumeMount %q → %s (ro=%v) in container %s",
				e.Name, e.ContainerPath, e.Readonly, ctr.Name)
		}
	}
	return nil
}

func ensurePluginVolumeMount(container *types.Container, entry pluginVolumeMountEntry) error {
	var existing *cubeboxv1.VolumeMounts
	for _, mount := range container.VolumeMounts {
		if mount == nil {
			continue
		}
		if filepath.Clean(mount.GetContainerPath()) == entry.ContainerPath &&
			mount.GetName() != entry.Name {
			return fmt.Errorf("plugin volume mount %q conflicts with volume %q at container path %s",
				entry.Name, mount.GetName(), entry.ContainerPath)
		}
		if mount.GetName() != entry.Name {
			continue
		}
		if existing != nil {
			return fmt.Errorf("plugin volume mount %q is duplicated before injection", entry.Name)
		}
		existing = mount
	}
	if existing == nil {
		container.VolumeMounts = append(container.VolumeMounts, &cubeboxv1.VolumeMounts{
			Name:          entry.Name,
			ContainerPath: entry.ContainerPath,
			Readonly:      entry.Readonly,
		})
		return nil
	}
	if existing.GetHostPath() != "" ||
		filepath.Clean(existing.GetContainerPath()) != entry.ContainerPath ||
		existing.GetReadonly() != entry.Readonly {
		return fmt.Errorf("plugin volume mount %q conflicts with existing mount", entry.Name)
	}
	return nil
}
