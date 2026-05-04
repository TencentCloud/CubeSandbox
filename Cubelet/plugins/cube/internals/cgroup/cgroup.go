// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"os"
	"path"
	"time"

	"encoding/json"

	"github.com/shopspring/decimal"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const pageTableCoeff = 64

func getMbSize(q resource.Quantity) int64 {
	return q.Value() / 1024 / 1024
}

type CubeVMMResource struct {
	CPU int64 `json:"cpu"`

	Memory int64 `json:"memory"`

	SnapMemory int64 `json:"snap_memory"`

	PreservedMemory     int64 `json:"preserve_memory"`
	SnapPreservedMemory int64 `json:"-"`
}

type VmSnapshotSpec struct {
	CPU                 int    `json:"cpu"`
	Memory              int    `json:"memory"`
	Product             string `json:"product"`
	SnapPreservedMemory int    `json:"preserve_memory"`
}

func vmSpecOrderByCPU(s1, s2 *VmSnapshotSpec) bool {
	return s1.CPU < s2.CPU
}

func vmSpecOrderByMemory(s1, s2 *VmSnapshotSpec) bool {
	return s1.Memory < s2.Memory
}

func vmSpecOrderByProduct(s1, s2 *VmSnapshotSpec) bool {
	return s1.Product < s2.Product
}

type OverheadConfig struct {
	VmMemoryBase        resource.Quantity
	VmMemoryCoefficient decimal.Decimal
	HostMemoryBase      resource.Quantity
	CubeMsgMemory       resource.Quantity

	VmCpu   resource.Quantity
	HostCpu resource.Quantity

	SnapshotDiskDir string
}

func loadVmSnapshotSpec(path string, overhead *OverheadConfig) (VMSnapshotSpecsByProduct, error) {
	if path == "" {
		return nil, nil
	}
	var specs []VmSnapshotSpec
	bb, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(bb, &specs)
	if err != nil {
		return nil, err
	}

	utils.OrderedBy(vmSpecOrderByProduct, vmSpecOrderByCPU, vmSpecOrderByMemory).Sort(specs)

	specMap := make(VMSnapshotSpecsByProduct)
	for _, s := range specs {
		insType := constants.GetInstanceTypeWithDefault(s.Product)

		specMap[insType] = append(specMap[insType], s)
	}

	CubeLog.Errorf("loaded vm snapshot spec: %+v", specMap)
	return specMap, nil
}

func (overhead *OverheadConfig) GetResourceWithOverhead(ctx context.Context, realReq *cubebox.RunCubeSandboxRequest, volumeInfo interface{}) (rq *cubeboxstore.ResourceWithOverHead, err error) {
	ctns := realReq.Containers
	instanceType := realReq.GetInstanceType()
	if len(ctns) == 0 {
		return nil, fmt.Errorf("no containers")
	}
	rq = &cubeboxstore.ResourceWithOverHead{}
	cpuQuantity := resource.MustParse("0")
	memQuantity := resource.MustParse("0")

	for _, ctr := range ctns {
		ctncpuQuantity, err := resource.ParseQuantity(ctr.GetResources().GetCpu())
		if err != nil {
			return nil, fmt.Errorf("parse container %q cpu limit: %w", ctr.Name, err)
		}
		ctnmemQuantity, err := resource.ParseQuantity(ctr.GetResources().GetMem())
		if err != nil {
			return nil, fmt.Errorf("parse container %q mem limit: %w", ctr.Name, err)
		}
		cpuQuantity.Add(ctncpuQuantity)
		memQuantity.Add(ctnmemQuantity)

	}

	if instanceType == cubebox.InstanceType_cubebox.String() {

		rq.VmCpuQ = cpuQuantity.DeepCopy()
		rq.HostCpuQ = cpuQuantity.DeepCopy()
		rq.HostMemQ = memQuantity.DeepCopy()
		rq.VmCpuQ = cpuQuantity.DeepCopy()
		rq.VmMemQ = memQuantity.DeepCopy()
		rq.PmemPageQ = resource.MustParse("0")
		return rq, nil
	}

	cpuQuantity.Add(overhead.VmCpu)
	rq.VmCpuQ = cpuQuantity.DeepCopy()

	cpuQuantity.Add(overhead.HostCpu)
	rq.HostCpuQ = cpuQuantity.DeepCopy()

	memQuantity.Add(overhead.CubeMsgMemory)
	memQuantity.Add(overhead.VmMemoryBase)

	rq.MemReq = memQuantity

	one := decimal.NewFromInt(1)
	n := overhead.VmMemoryCoefficient
	memOverheadVar := decimal.NewFromInt(memQuantity.Value()).Div(one.Sub(one.Div(n)))
	memOverheadVarQuantity := resource.NewQuantity(memOverheadVar.Ceil().IntPart(), memQuantity.Format)

	memOverheadVarQuantity.Add(rq.PmemPageQ)
	rq.VmMemQ = ceilMemQuota(*memOverheadVarQuantity)

	return rq, nil
}

func (overhead *OverheadConfig) MatchVMSnapshotSpec(ctx context.Context, req cubeboxstore.ResourceWithOverHead,
	snaps []VmSnapshotSpec, instanceType string) (CubeVMMResource, *cubeboxstore.ResourceWithOverHead) {
	var vmResource CubeVMMResource

	if instanceType == cubebox.InstanceType_cubebox.String() {

		vmResource = CubeVMMResource{
			CPU:        req.VmCpuQ.Value(),
			Memory:     getMbSize(req.VmMemQ),
			SnapMemory: getMbSize(req.VmMemQ),
		}
		return vmResource, &req
	}

	defer func() {
		var memoryForHost int64
		if vmResource.SnapMemory > 0 {

			memoryForHost = vmResource.SnapMemory
		} else {

			memoryForHost = vmResource.Memory
		}
		newHostMemQ := resource.NewQuantity(memoryForHost*1024*1024, resource.BinarySI)
		newHostMemQ.Add(overhead.HostMemoryBase)
		req.HostMemQ = *newHostMemQ
	}()

	memReq := getMbSize(req.MemReq)
	vmCPU := req.VmCpuQ.Value()
	vmMem := getMbSize(req.VmMemQ)
	pmemPage := getMbSize(req.PmemPageQ)

	for _, snap := range snaps {

		snapMemPage := snap.Memory / pageTableCoeff

		preservedMemory := snap.Memory - snapMemPage - int(memReq) - int(pmemPage)
		if vmCPU == int64(snap.CPU) && preservedMemory >= 0 {

			vmResource = CubeVMMResource{
				CPU:                 int64(snap.CPU),
				Memory:              vmMem,
				SnapMemory:          int64(snap.Memory),
				PreservedMemory:     int64(preservedMemory),
				SnapPreservedMemory: int64(snap.SnapPreservedMemory),
			}
			return vmResource, &req
		}
	}

	log.G(ctx).Errorf("no vm snapshot spec matched vmCPU: %v,memReq:%v, pmemPage: %v, 冷启动规格 :%v", vmCPU, memReq, pmemPage, vmMem)
	vmResource = CubeVMMResource{
		CPU:    vmCPU,
		Memory: vmMem,
	}

	return vmResource, &req
}

func SetCubeboxCgroupLimit(ctx context.Context, group string, cpuQ resource.Quantity, memQ resource.Quantity, usePoolV2 bool) error {
	if usePoolV2 {
		return l.poolV2Handle.Update(ctx, group, cpuQ, memQ)
	}
	return l.poolV1Handle.Update(ctx, group, cpuQ, memQ)
}

func AddProc(path string, pid uint64) error {
	var err error

	delay := 10 * time.Millisecond
	for i := 0; i < 3; i++ {
		if i != 0 {
			time.Sleep(delay)
			delay *= 2
		}

		if err = l.poolV1Handle.AddProc(path, pid); err == nil {
			return nil
		}
	}
	return fmt.Errorf("unable to add proc, pid: %v, path: %v %w", pid, path, err)
}

func MakeCgroupPoolV1PathByString(group string) string {
	return path.Join(handle.DefaultPathPoolV1, group)
}

func MakeCgroupPoolV2PathByString(group string) string {
	return path.Join(handle.DefaultPathPoolV2, group)
}

func MakeCgroupPathByID(id uint32) string {
	if id < CgPoolV1IdLimit {
		return path.Join(handle.DefaultPathPoolV1, fmt.Sprintf("%d", id))
	} else {
		numa := int(id/CgPoolIdLimit) - 1

		idInNuma := id % CgPoolIdLimit
		return path.Join(handle.DefaultPathPoolV2, fmt.Sprintf("numa%d/%d", numa, idInNuma))
	}
}

func MakeNoPrefixCgroupPathByID(id uint32) string {
	if id < CgPoolV1IdLimit {
		return fmt.Sprintf("%d", id)
	} else {
		numa := int(id/CgPoolIdLimit) - 1

		idInNuma := id % CgPoolIdLimit
		return fmt.Sprintf("numa%d/%d", numa, idInNuma)
	}
}

func MakeFullCgID(id uint16, usePoolV2 bool, numa int32) uint32 {
	if usePoolV2 {
		return uint32(numa+1)*CgPoolIdLimit + uint32(id)
	} else {
		return uint32(id)
	}

}

func CgroupID2NumaID(fullCgID uint32) (int, error) {
	if fullCgID < CgPoolV1IdLimit {
		return -1, fmt.Errorf("fullCgID is not in pool v2 range")
	}
	numa := int(fullCgID/CgPoolIdLimit) - 1
	return numa, nil
}
