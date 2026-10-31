// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"path"
	"path/filepath"
	"sort"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// coldStartHostDirMounts maps host_dir backends to container rootfs mounts for
// cold-start sandboxes (no template restore, including restarts).
//
// Template restore delivers host_dir volumes via cube.propagation.exec.mounts,
// which the guest agent only applies on the restore path. On cold start the
// virtiofs channels have no propagation mount name, so the guest mounts them at
// /run/virtiofs/<virtio_rw|virtio_ro>. Emitting them as cube.rootfs.info mounts
// with VirtiofsID set lets the shim resolve the guest path and rustjail bind
// them while building the rootfs, before the container process starts.
func coldStartHostDirMounts(ctx context.Context, flowOpts *workflow.CreateContext,
	containerReq *cubebox.ContainerConfig) []virtiofs.CubeRootfsMount {
	if flowOpts == nil || containerReq == nil {
		return nil
	}
	// Only cubebox sandboxes get the virtio_rw/virtio_ro channels from the
	// cubebox CBRI plugin.
	if flowOpts.GetInstanceType() != cubebox.InstanceType_cubebox.String() {
		return nil
	}
	if flowOpts.IsCreateSnapshot() || flowOpts.WantsSnapshotRestore() {
		return nil
	}
	storageInfo, ok := flowOpts.StorageInfo.(*storage.StorageInfo)
	if !ok || storageInfo == nil || len(storageInfo.HostDirBackendInfos) == 0 {
		return nil
	}

	type indexedVolumeMount struct {
		mount *cubebox.VolumeMounts
		index int
	}
	vmByVolName := make(map[string]indexedVolumeMount)
	for i, vm := range containerReq.GetVolumeMounts() {
		vmByVolName[vm.GetName()] = indexedVolumeMount{mount: vm, index: i}
	}

	type candidate struct {
		mount        virtiofs.CubeRootfsMount
		requestIndex int
		backendKey   string
	}
	var candidates []candidate
	for backendKey, info := range storageInfo.HostDirBackendInfos {
		indexedVM, ok := vmByVolName[info.VolumeName]
		if !ok {
			continue
		}
		containerPath := indexedVM.mount.GetContainerPath()
		if containerPath == "" {
			log.G(ctx).Warnf("[hostdir] coldStartHostDirMounts: VolumeMount for volume %q has empty container path, skip", info.VolumeName)
			continue
		}

		virtiofsID := constants.PropagationVirtioRw
		accessOpt := constants.MountOptReadWrite
		if info.ReadOnly {
			virtiofsID = constants.PropagationVirtioRo
			accessOpt = constants.MountOptReadOnly
		}
		m := virtiofs.CubeRootfsMount{
			// HostSource stays empty: the backend is already shared through the
			// sandbox-level virtio_rw/virtio_ro channel, not the cube.fs share.
			VirtiofsID:     virtiofsID,
			VirtiofsSource: filepath.Base(info.BindPath),
			ContainerDest:  containerPath,
			Type:           constants.MountTypeBind,
			Options:        []string{constants.MountOptBindRO, accessOpt},
		}
		log.G(ctx).Infof("[hostdir] cold start rootfs mount: %s:%s -> %s",
			m.VirtiofsID, m.VirtiofsSource, m.ContainerDest)
		candidates = append(candidates, candidate{mount: m, requestIndex: indexedVM.index, backendKey: backendKey})
	}

	// Parent destinations go before nested ones; equal destinations keep the
	// request order so the last VolumeMount wins, matching the restore path.
	sort.Slice(candidates, func(i, j int) bool {
		left := path.Clean(candidates[i].mount.ContainerDest)
		right := path.Clean(candidates[j].mount.ContainerDest)
		if left != right {
			return left < right
		}
		if candidates[i].requestIndex != candidates[j].requestIndex {
			return candidates[i].requestIndex < candidates[j].requestIndex
		}
		return candidates[i].backendKey < candidates[j].backendKey
	})

	mounts := make([]virtiofs.CubeRootfsMount, 0, len(candidates))
	for _, c := range candidates {
		mounts = append(mounts, c.mount)
	}
	return mounts
}
