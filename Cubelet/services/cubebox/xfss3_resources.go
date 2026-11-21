// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// xfss3PauseRefusedPrefix starts the message of an xfs-s3 pause refused
// before the guest is frozen. The sandbox is still running, so Master drops
// the pause binding instead of failing it; CubeMaster matches the same text.
const xfss3PauseRefusedPrefix = "xfs-s3 pause refused before freezing"

// validateXFSS3PauseResources refuses, before the guest is frozen, an
// xfs-s3 pause whose sandbox depends on something the checkpoint does not
// carry. The package holds one writable disk (the rootfs), guest memory and
// device state; everything else must be immutable and present on every node,
// or a restore elsewhere would run against missing or different data:
//
//   - host mounts and host_dir volumes, declared or not, and directory
//     sandbox_path volumes;
//   - plugin volumes, whose data lives with their driver on this node;
//   - more than one disk-backed empty_dir, as only the rootfs is exported;
//   - a writable pmem, which the guest could change outside the package.
//
// Other backends keep pausing what they paused before.
func validateXFSS3PauseResources(cb *cubeboxstore.CubeBox) error {
	hostMounts, err := declaredRawHostMounts(cb.Annotations)
	if err != nil {
		return fmt.Errorf("xfs-s3 checkpoint: %w", err)
	}
	if len(hostMounts) > 0 {
		return fmt.Errorf("xfs-s3 checkpoint cannot carry host mounts to another node")
	}
	for _, c := range cb.AllContainers() {
		if c == nil || c.Config == nil {
			continue
		}
		for _, m := range c.Config.GetVolumeMounts() {
			if m != nil && strings.TrimSpace(m.GetHostPath()) != "" {
				return fmt.Errorf("xfs-s3 checkpoint cannot carry host path %s to another node", m.GetHostPath())
			}
		}
	}
	if _, err := validateSnapshotSandboxTarget(cb, true); err != nil {
		return fmt.Errorf("xfs-s3 checkpoint: %w", err)
	}
	plugins, err := declaredPluginVolumes(cb.Annotations)
	if err != nil {
		return fmt.Errorf("xfs-s3 checkpoint: %w", err)
	}
	if len(plugins) > 0 {
		names := make([]string, 0, len(plugins))
		for name := range plugins {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("xfs-s3 checkpoint cannot carry plugin volumes %v to another node", names)
	}
	disks := 0
	for _, v := range cb.Volumes {
		source := v.GetVolumeSource()
		if source == nil {
			continue
		}
		if hd := source.GetHostDirVolumes(); hd != nil && len(hd.GetVolumeSources()) > 0 {
			return fmt.Errorf("xfs-s3 checkpoint cannot carry host_dir volume %s to another node", v.GetName())
		}
		if source.GetPluginVolume() != nil {
			return fmt.Errorf("xfs-s3 checkpoint cannot carry plugin volume %s to another node", v.GetName())
		}
		if ed := source.GetEmptyDir(); ed != nil && ed.GetMedium() == cubebox.StorageMedium_StorageMediumDefault {
			disks++
		}
	}
	if disks > 1 {
		return fmt.Errorf("xfs-s3 checkpoint carries only the rootfs disk, sandbox has %d disk volumes", disks)
	}
	return validateXFSS3Pmems(cb.Annotations[constants.AnnotationPmem])
}

// validateXFSS3Pmems accepts only pmems the guest cannot write: the shim maps
// a pmem without discard_writes shared and writable.
func validateXFSS3Pmems(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var pmems []struct {
		ID            string `json:"id"`
		File          string `json:"file"`
		DiscardWrites bool   `json:"discard_writes"`
	}
	if err := json.Unmarshal([]byte(raw), &pmems); err != nil {
		return fmt.Errorf("xfs-s3 checkpoint: decode %s: %w", constants.AnnotationPmem, err)
	}
	for _, p := range pmems {
		if !p.DiscardWrites {
			return fmt.Errorf("xfs-s3 checkpoint cannot carry writable pmem %s (%s)", p.ID, p.File)
		}
	}
	return nil
}
