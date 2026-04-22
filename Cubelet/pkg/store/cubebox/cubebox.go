// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/apis/shimapi/shimtypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

const (
	CurrentCubeboxVersion = "v2"
	CubeboxVersionV1      = "v1"
	CubeboxVersionV2      = "v2"
)

type Metadata struct {
	ID string

	Name      string `json:"name"`
	SandboxID string `json:"sandbox_id"`

	Annotations map[string]string

	Labels map[string]string

	Config    *cubebox.ContainerConfig `json:"config"`
	Namespace string                   `json:"namespace,omitempty"`

	CreatedAt int64

	ResourceWithOverHead *ResourceWithOverHead
	InstanceType         string `json:"instance_type,omitempty"`

	DeletedTime *time.Time `json:"deleted_time,omitempty"`

	MetaLock sync.Mutex `json:"-"`
}

func (m *Metadata) AddAnnotations(annotations map[string]string) {
	m.MetaLock.Lock()
	defer m.MetaLock.Unlock()

	cloneAnnotations := maps.Clone(m.Annotations)
	if cloneAnnotations == nil {
		cloneAnnotations = make(map[string]string)
	}
	for k, v := range annotations {
		cloneAnnotations[k] = v
	}
	m.Annotations = cloneAnnotations
}

func (m *Metadata) AddLabels(labels map[string]string) {
	m.MetaLock.Lock()
	defer m.MetaLock.Unlock()

	cloneLabels := maps.Clone(m.Labels)
	if cloneLabels == nil {
		cloneLabels = make(map[string]string)
	}
	for k, v := range labels {
		cloneLabels[k] = v
	}
	m.Labels = cloneLabels
}

type ResourceWithOverHead struct {
	MemReq,
	HostCpuQ,
	HostMemQ,
	VmCpuQ,
	VmMemQ,
	PmemPageQ resource.Quantity
}

func (rq *ResourceWithOverHead) String() string {
	return fmt.Sprintf("MemReq: %v, HostCpuQ: %v, HostMemQ: %v, VmCpuQ: %v, VmMemQ: %v, PmemPageQ: %v",
		rq.MemReq.String(), rq.HostCpuQ.String(), rq.HostMemQ.String(), rq.VmCpuQ.String(), rq.VmMemQ.String(), rq.PmemPageQ.String())
}

type CubeBox struct {
	Metadata
	Namespace string `json:"namespace,omitempty"`
	AppID     string `json:"app_id,omitempty"`

	IP           string
	PortMappings []*cubebox.PortMapping
	CGroupPath   string

	FirstContainerName string `json:"first_container_name,omitempty"`
	Containers         map[string]*Container
	ContainersMap      *ContainersMap

	ExitCh <-chan containerd.ExitStatus `json:"-"`

	NumaNode int32 `json:"numa_node"`
	Queues   int64 `json:"queues"`

	OciRuntime *cubeconfig.Runtime   `json:"oci_runtime,omitempty"`
	Endpoint   sandboxstore.Endpoint `json:"endpoint,omitempty"`

	PodConfig *PodConfig `json:"pod_config,omitempty"`

	VirtiofsMap map[string]*virtiofs.VirtiofsConfig `json:"virtiofs_config_map,omitempty"`

	HotPlugDevices map[string]*shimtypes.CubeShimDevice `json:"hot_plug_devices,omitempty"`

	HotPlugDisk map[string]*shimtypes.ChDiskDevice `json:"hot_plug_disk,omitempty"`

	Version string `json:"version,omitempty"`

	Status *StatusStorage `json:"status,omitempty"`

	RequestSource string `json:"source,omitempty"`
	UserDeleteMark

	LocalRunTemplate *templatetypes.LocalRunTemplate

	ImageReferences map[string]ImageReference

	sync.RWMutex `json:"-"`
}

type UserDeleteMark struct {
	UserMarkDeletedTime *time.Time `json:"user_mark_deleted_time,omitempty"`
	DeleteRequestID     string     `json:"delete_request_id,omitempty"`
}

func (cb *CubeBox) MainStatus() *StatusStorage {
	return cb.mainContainerStatus()
}

func (cb *CubeBox) mainContainerStatus() *StatusStorage {
	if cb.FirstContainer() == nil {
		return nil
	}
	return cb.FirstContainer().Status
}

func (cb *CubeBox) GetStatus() *StatusStorage {
	if cb.mainContainerStatus() != nil {
		cb.Status = cb.mainContainerStatus()
	} else {
		if cb.FirstContainer() == nil {
			log.G(context.Background()).Warnf("cubebox %s has no main container", cb.ID)
		} else {
			cb.FirstContainer().Status = StoreStatus(Status{CreatedAt: cb.CreatedAt})
			cb.Status = cb.FirstContainer().Status
		}
	}
	if cb.Status == nil {
		cb.Status = StoreStatus(Status{CreatedAt: cb.CreatedAt})
	}
	return cb.Status
}

func (cb *CubeBox) Transmission() {
	if cb.Containers == nil {
		return
	}

	if cb.ContainersMap == nil {
		cb.ContainersMap = &ContainersMap{}
	}
	for _, ctr := range cb.Containers {
		cb.ContainersMap.AddContainer(ctr)
	}
}

func (cb *CubeBox) AddContainer(ctr *Container) {

	if cb.ContainersMap == nil {
		cb.ContainersMap = &ContainersMap{}
	}
	if ctr == nil {
		return
	}
	if ctr.IsPod {
		cb.FirstContainerName = ctr.ID
	}
	cb.ContainersMap.AddContainer(ctr)
}

func (cb *CubeBox) DeleteContainer(id string) {

	if cb.Containers != nil {
		delete(cb.Containers, id)
	}

	if cb.ContainersMap == nil {
		return
	}
	if container, err := cb.ContainersMap.Get(id); err == nil {
		container.MarkDeleted()
	}
}

func (cb *CubeBox) All() map[string]*Container {
	containerMap := make(map[string]*Container)
	for _, ctr := range cb.AllContainers() {
		if ctr.IsPod {
			continue
		}
		containerMap[ctr.ID] = ctr
	}
	return containerMap
}

func (cb *CubeBox) FirstContainer() *Container {
	id := cb.FirstContainerName
	if id == "" {
		id = cb.ID
	}
	ci, err := cb.ContainersMap.Get(id)
	if err != nil && log.IsDebug() {
		log.G(context.Background()).WithField("stack", string(debug.Stack())).Errorf("get first container fail for cubebox %s with %d subcontainers: %v", cb.ID,
			len(cb.ContainersMap.All()), err)
		return nil
	}
	if ci.ID == "" {
		ci.Metadata = cb.Metadata
	}
	return ci
}

func (cb *CubeBox) AllContainers() map[string]*Container {
	if cb.ContainersMap == nil {
		cb.ContainersMap = &ContainersMap{}
	}
	return cb.ContainersMap.All()
}

func (cb *CubeBox) Get(id string) (*Container, error) {
	if cb.ContainersMap == nil {
		cb.ContainersMap = &ContainersMap{}
	}
	return cb.ContainersMap.Get(id)
}

func (cb *CubeBox) GetVersion() string {
	if cb.Version != "" {
		return cb.Version
	}
	if cb.FirstContainerName != "" {
		return CubeboxVersionV2
	}
	return CubeboxVersionV1
}

func (cb *CubeBox) IsAllContainerNotRunning() bool {
	var runOrCreatingContainerCount int
	for _, c := range cb.All() {
		if c.Status != nil && (c.Status.Get().State() == cubebox.ContainerState_CONTAINER_RUNNING ||
			c.Status.Get().State() == cubebox.ContainerState_CONTAINER_CREATED ||
			c.Status.Get().State() == cubebox.ContainerState_CONTAINER_PAUSED ||
			c.Status.Get().State() == cubebox.ContainerState_CONTAINER_PAUSING) {
			runOrCreatingContainerCount++
		}
	}
	return runOrCreatingContainerCount == 0
}

func (cb *CubeBox) AddImageReference(reference ImageReference) {
	if cb.ImageReferences == nil {
		cb.ImageReferences = make(map[string]ImageReference)
	}
	cloneImageReferences := maps.Clone(cb.ImageReferences)
	cloneImageReferences[reference.ID] = reference
	cb.ImageReferences = cloneImageReferences
}

type Container struct {
	Metadata `json:",inline"`

	IP string `json:"ip,omitempty"`

	Container containerd.Container `json:"-"`

	Status *StatusStorage `json:"status,omitempty"`

	ExitCh <-chan containerd.ExitStatus `json:"-"`

	IsDebugStdout  bool                     `json:"is_debug_stdout,omitempty"`
	IsPod          bool                     `json:"is_pod,omitempty"`
	CubeRootfsInfo *virtiofs.CubeRootfsInfo `json:"cube_rootfs_info,omitempty"`

	Snapshotter string              `json:"snapshotter,omitempty"`
	SnapshotKey string              `json:"snapshot_key,omitempty"`
	HostImage   *cubehost.HostImage `json:"image,omitempty"`
}

func (ctr *Container) MarkDeleted() {
	if ctr.DeletedTime == nil {
		now := time.Now()
		ctr.DeletedTime = &now
	}
}

func (ctr *Container) GetContainerImageIDs() []string {
	var imageIDs []string
	if ctr.Labels != nil {
		if id, ok := ctr.Labels[constants.LabelContainerImageCosType]; ok {
			imageIDs = append(imageIDs, id)
		}
	} else {
		if ctr.Config != nil && ctr.Config.Image != nil {
			imageIDs = append(imageIDs, ctr.Config.Image.Image)
		}
	}

	return imageIDs
}

func SandboxToContainer(sb *CubeBox) *Container {
	return sb.FirstContainer()
}

type ContainersMap struct {
	sync.RWMutex `json:"-"`
	ContainerMap map[string]*Container
}

func (cm *ContainersMap) AddContainer(ctr *Container) {
	cm.Lock()
	defer cm.Unlock()
	if cm.ContainerMap == nil {
		cm.ContainerMap = make(map[string]*Container)
	}
	cm.ContainerMap[ctr.ID] = ctr
}

func (cm *ContainersMap) DeleteContainer(id string) {
	cm.Lock()
	defer cm.Unlock()
	if cm.ContainerMap == nil {
		return
	}
	delete(cm.ContainerMap, id)

}

func (cm *ContainersMap) All() map[string]*Container {
	cm.RLock()
	defer cm.RUnlock()
	m := make(map[string]*Container)
	if cm.ContainerMap == nil {
		return m
	}
	for k, ctr := range cm.ContainerMap {
		m[k] = ctr
	}
	return m
}

func (cm *ContainersMap) Get(id string) (*Container, error) {
	cm.RLock()
	defer cm.RUnlock()
	if cm.ContainerMap == nil {
		return &Container{Status: StoreStatus(Status{})}, fmt.Errorf("container map is nil")
	}
	if ctr, ok := cm.ContainerMap[id]; ok {
		return ctr, nil
	}
	return &Container{Status: StoreStatus(Status{})}, fmt.Errorf("container with id %s not found", id)
}

type ImageReference struct {
	ID         string
	References []string
	Medium     images.ImageStorageMediaType
}
