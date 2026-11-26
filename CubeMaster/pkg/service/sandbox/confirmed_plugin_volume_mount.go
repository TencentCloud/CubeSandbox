// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// GetEffectiveSandboxSpec reads the base spec and projects the confirmed
// runtime bindings into the create-request shape used by lifecycle consumers.
// The bindings retain their volume row IDs for capture and restore checks.
func GetEffectiveSandboxSpec(ctx context.Context, sandboxID string) (*types.CreateCubeSandboxReq, []sandboxspec.RuntimeVolumeMount, error) {
	stored, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	spec := stored.Request
	for _, mount := range stored.Mounts {
		spec, err = withConfirmedPluginVolumeMount(ctx, spec, pluginVolumeMountEntry{
			Name:          mount.VolumeID,
			ContainerPath: mount.ContainerPath,
			Readonly:      mount.Readonly,
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return spec, stored.Mounts, nil
}

// withConfirmedPluginVolumeMount projects a confirmed binding onto a copy of
// a canonical, persisted spec. A snapshot taken before the binding keeps its spec.
func withConfirmedPluginVolumeMount(ctx context.Context, base *types.CreateCubeSandboxReq, mount pluginVolumeMountEntry) (*types.CreateCubeSandboxReq, error) {
	spec, err := sandboxspec.CanonicalizeRequest(base)
	if err != nil {
		return nil, err
	}
	delete(spec.Annotations, AnnotationPluginVolumeSources)
	mount.Name = strings.TrimSpace(mount.Name)
	mount.ContainerPath = filepath.Clean(mount.ContainerPath)

	// Keep previous entries intact, including fields this reader does not use.
	var entries []json.RawMessage
	if raw := spec.Annotations[AnnotationPluginVolumeMounts]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("parse %s: %w", AnnotationPluginVolumeMounts, err)
		}
	}
	found := false
	for _, raw := range entries {
		var entry pluginVolumeMountEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("parse %s entry: %w", AnnotationPluginVolumeMounts, err)
		}
		if strings.TrimSpace(entry.Name) == mount.Name {
			if filepath.Clean(entry.ContainerPath) != mount.ContainerPath || entry.Readonly != mount.Readonly {
				return nil, fmt.Errorf("plugin volume %q already has a different binding", mount.Name)
			}
			found = true
		}
	}
	declared := false
	for _, volume := range spec.Volumes {
		if volume == nil || volume.Name != mount.Name {
			continue
		}
		if volume.VolumeSource != nil && volume.VolumeSource.PluginVolume == nil {
			return nil, fmt.Errorf("volume %q already has a non-plugin source", mount.Name)
		}
		declared = true
	}
	if !found {
		if !declared {
			spec.Volumes = append(spec.Volumes, &types.Volume{Name: mount.Name})
		}
		raw, err := json.Marshal(mount)
		if err != nil {
			return nil, err
		}
		annotation, err := json.Marshal(append(entries, raw))
		if err != nil {
			return nil, err
		}
		spec.Annotations[AnnotationPluginVolumeMounts] = string(annotation)
	}
	if err := injectPluginVolumeMounts(ctx, spec); err != nil {
		return nil, err
	}
	return spec, nil
}
