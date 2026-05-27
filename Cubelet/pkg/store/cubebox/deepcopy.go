// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"maps"

	"google.golang.org/protobuf/proto"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/apis/shimapi/shimtypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
)

func (cb *CubeBox) DeepCopy() *CubeBox {
	if cb == nil {
		return nil
	}

	copied := &CubeBox{
		Metadata:           cb.Metadata.DeepCopy(),
		Namespace:          cb.Namespace,
		AppID:              cb.AppID,
		IP:                 cb.IP,
		CGroupPath:         cb.CGroupPath,
		FirstContainerName: cb.FirstContainerName,
		NumaNode:           cb.NumaNode,
		Queues:             cb.Queues,
		Endpoint:           cb.Endpoint,
		Version:            cb.Version,
		RequestSource:      cb.RequestSource,
		UserDeleteMark:     cb.UserDeleteMark.DeepCopy(),
	}

	if cb.PortMappings != nil {
		copied.PortMappings = make([]*cubebox.PortMapping, len(cb.PortMappings))
		for i, pm := range cb.PortMappings {
			if pm != nil {
				copied.PortMappings[i] = proto.Clone(pm).(*cubebox.PortMapping)
			}
		}
	}

	if cb.Containers != nil {
		copied.Containers = make(map[string]*Container, len(cb.Containers))
		for k, v := range cb.Containers {
			copied.Containers[k] = v.DeepCopy()
		}
	}

	if cb.ContainersMap != nil {
		copied.ContainersMap = cb.ContainersMap.DeepCopy()
	}

	if cb.OciRuntime != nil {
		runtimeCopy := *cb.OciRuntime
		copied.OciRuntime = &runtimeCopy
	}

	if cb.PodConfig != nil {
		podConfigCopy := *cb.PodConfig
		copied.PodConfig = &podConfigCopy
	}

	if cb.VirtiofsMap != nil {
		copied.VirtiofsMap = make(map[string]*virtiofs.VirtiofsConfig, len(cb.VirtiofsMap))
		for k, v := range cb.VirtiofsMap {
			if v != nil {
				vCopy := *v
				copied.VirtiofsMap[k] = &vCopy
			}
		}
	}

	if cb.HotPlugDevices != nil {
		copied.HotPlugDevices = make(map[string]*shimtypes.CubeShimDevice, len(cb.HotPlugDevices))
		for k, v := range cb.HotPlugDevices {
			if v != nil {
				vCopy := *v
				copied.HotPlugDevices[k] = &vCopy
			}
		}
	}

	if cb.HotPlugDisk != nil {
		copied.HotPlugDisk = make(map[string]*shimtypes.ChDiskDevice, len(cb.HotPlugDisk))
		for k, v := range cb.HotPlugDisk {
			if v != nil {
				vCopy := *v
				copied.HotPlugDisk[k] = &vCopy
			}
		}
	}

	if cb.Status != nil {
		copied.Status = cb.Status.DeepCopy()
	}

	if cb.LocalRunTemplate != nil {
		templateCopy := *cb.LocalRunTemplate
		copied.LocalRunTemplate = &templateCopy
	}

	if cb.ImageReferences != nil {
		copied.ImageReferences = make(map[string]ImageReference, len(cb.ImageReferences))
		for k, v := range cb.ImageReferences {
			copied.ImageReferences[k] = v.DeepCopy()
		}
	}

	return copied
}

func (m Metadata) DeepCopy() Metadata {
	copied := Metadata{
		ID:           m.ID,
		Name:         m.Name,
		SandboxID:    m.SandboxID,
		Namespace:    m.Namespace,
		CreatedAt:    m.CreatedAt,
		InstanceType: m.InstanceType,
	}

	if m.Annotations != nil {
		copied.Annotations = make(map[string]string, len(m.Annotations))
		maps.Copy(copied.Annotations, m.Annotations)
	}

	if m.Labels != nil {
		copied.Labels = make(map[string]string, len(m.Labels))
		maps.Copy(copied.Labels, m.Labels)
	}

	if m.Config != nil {
		copied.Config = proto.Clone(m.Config).(*cubebox.ContainerConfig)
	}

	if m.ResourceWithOverHead != nil {
		copied.ResourceWithOverHead = m.ResourceWithOverHead.DeepCopy()
	}

	if m.DeletedTime != nil {
		timeCopy := *m.DeletedTime
		copied.DeletedTime = &timeCopy
	}

	return copied
}

func (r *ResourceWithOverHead) DeepCopy() *ResourceWithOverHead {
	if r == nil {
		return nil
	}
	return &ResourceWithOverHead{
		MemReq:            r.MemReq.DeepCopy(),
		HostCpuQ:          r.HostCpuQ.DeepCopy(),
		HostMemQ:          r.HostMemQ.DeepCopy(),
		VmCpuQ:            r.VmCpuQ.DeepCopy(),
		VmMemQ:            r.VmMemQ.DeepCopy(),
		PmemPageQ:         r.PmemPageQ.DeepCopy(),
		HostDataDiskMB:    r.HostDataDiskMB,
		HostStorageDiskMB: r.HostStorageDiskMB,
	}
}

func (u UserDeleteMark) DeepCopy() UserDeleteMark {
	copied := UserDeleteMark{
		DeleteRequestID: u.DeleteRequestID,
	}
	if u.UserMarkDeletedTime != nil {
		timeCopy := *u.UserMarkDeletedTime
		copied.UserMarkDeletedTime = &timeCopy
	}
	return copied
}

func (c *Container) DeepCopy() *Container {
	if c == nil {
		return nil
	}

	copied := &Container{
		Metadata:      c.Metadata.DeepCopy(),
		IP:            c.IP,
		IsDebugStdout: c.IsDebugStdout,
		IsPod:         c.IsPod,
		Snapshotter:   c.Snapshotter,
		SnapshotKey:   c.SnapshotKey,
	}

	if c.Status != nil {
		copied.Status = c.Status.DeepCopy()
	}

	if c.CubeRootfsInfo != nil {
		rootfsCopy := *c.CubeRootfsInfo
		copied.CubeRootfsInfo = &rootfsCopy
	}

	if c.HostImage != nil {
		copied.HostImage = proto.Clone(c.HostImage).(*cubehost.HostImage)
	}

	return copied
}

func (cm *ContainersMap) DeepCopy() *ContainersMap {
	if cm == nil {
		return nil
	}

	cm.RLock()
	defer cm.RUnlock()

	copied := &ContainersMap{}
	if cm.ContainerMap != nil {
		copied.ContainerMap = make(map[string]*Container, len(cm.ContainerMap))
		for k, v := range cm.ContainerMap {
			copied.ContainerMap[k] = v.DeepCopy()
		}
	}

	return copied
}

func (ir ImageReference) DeepCopy() ImageReference {
	copied := ImageReference{
		ID:     ir.ID,
		Medium: ir.Medium,
	}

	if ir.References != nil {
		copied.References = make([]string, len(ir.References))
		copy(copied.References, ir.References)
	}

	return copied
}

func (s *StatusStorage) DeepCopy() *StatusStorage {
	if s == nil {
		return nil
	}
	s.RLock()
	defer s.RUnlock()
	return &StatusStorage{
		Status: s.Status,
	}
}
