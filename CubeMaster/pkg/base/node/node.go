// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package node is the basic unit of a host
package node

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
)

type Node struct {
	Index int    `json:"Index,omitempty"`
	InsID string `json:"InstanceID,omitempty"`
	UUID  string `json:"uuid,omitempty"`
	IP    string `json:"IP,omitempty"`

	CpuTotal int `json:"CpuTotal,omitempty"`

	MemMBTotal int64  `json:"MemMBTotal,omitempty"`
	Zone       string `json:"Zone,omitempty"`
	Region     string `json:"Region,omitempty"`

	SystemDiskSize int64 `json:"SystemDiskSize,omitempty"`

	DataDiskSize    int64  `json:"DataDiskSize,omitempty"`
	CPUType         string `json:"CpuType,omitempty"`
	ClusterLabel    string `json:"ClusterLabel,omitempty"`
	InstanceType    string `json:"InstanceType,omitempty"`
	OssClusterLabel string `json:"OssClusterLabel,omitempty"`

	DeviceClass           string  `json:"DeviceClass,omitempty"`
	DeviceID              int64   `json:"DeviceId,omitempty"`
	MachineHostIP         string  `json:"MachineHostIP,omitempty"`
	InstanceFamily        string  `json:"InstanceFamily,omitempty"`
	DedicatedClusterId    string  `json:"DedicatedClusterId,omitempty"`
	VirtualNodeQuotaArray []int64 `json:"VirtualNodeQuotaArray,omitempty" `

	HostStatus string `json:"HostStatus,omitempty"`

	CreateConcurrentNum int64 `json:"CreateConcurrentNum,omitempty"`

	MaxMvmLimit int64 `json:"MaxMvmLimit,omitempty"`

	QuotaCpu int64 `json:"QuotaCpu,omitempty"`

	QuotaMem int64 `json:"QuotaMem,omitempty"`

	MetaDataUpdateAt time.Time `json:"MetaDataUpdateAt,omitempty"`

	ReportedReady bool `json:"-"`

	Healthy bool `json:"Healthy"`

	UnhealthyReason string `json:"UnhealthyReason,omitempty"`

	Score float64 `json:"Score,omitempty"`

	QuotaCpuUsage int64 `json:"QuotaCpuUsage,omitempty"`

	QuotaMemUsage int64 `json:"QuotaMemUsage,omitempty"`

	CpuUtil float64 `json:"CpuUtil,omitempty"`

	CpuLoadUsage float64 `json:"CpuLoadUsage,omitempty"`

	MemUsage int64 `json:"MemUsage,omitempty"`

	DataDiskUsagePer    float64 `json:"DataDiskUsagePer,omitempty"`
	StorageDiskUsagePer float64 `json:"StorageDiskUsagePer,omitempty"`
	SysDiskUsagePer     float64 `json:"SysDiskUsagePer,omitempty"`

	MvmNum int64 `json:"mvm_num,omitempty"`

	MetricUpdate time.Time `json:"MetricUpdateAt,omitempty"`

	MetricLocalUpdateAt time.Time `json:"MetricLocalUpdateAt,omitempty"`

	RealTimeCreateNum int64 `json:"RealTimeCreateNum,omitempty"`

	LocalCreateNum int64 `json:"LocalCreateNum,omitempty"`
	NicQueues      int64 `json:"nic_queues,omitempty"`

	NodeLabels map[string]string `json:"NodeLabels,omitempty"`

	// schedulingDisabled is the cordon flag (true → block new sandboxes).
	// Exposed via SchedulingDisabled() / JSON as "SchedulingDisabled".
	schedulingDisabled atomic.Bool

	labelsCache *nodeLabelsCacheStore
}

// DecodeSchedulingDisabled reports whether labels cordon the node.
// Key absent → false; key present (canonical "true" or any other value) → true
// so corrupt/non-canonical values fail closed.
func DecodeSchedulingDisabled(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	_, ok := labels[constants.LabelSchedulingDisabled]
	return ok
}

// SetSchedulingDisabled stores the concurrent-safe cordon flag.
func (n *Node) SetSchedulingDisabled(disabled bool) {
	if n == nil {
		return
	}
	n.schedulingDisabled.Store(disabled)
}

// SchedulingDisabled reports whether this node is cordoned.
func (n *Node) SchedulingDisabled() bool {
	return n != nil && n.schedulingDisabled.Load()
}

// SchedulingAllowed reports whether this node may receive new sandboxes based
// solely on cordon state (health / metrics are orthogonal).
func (n *Node) SchedulingAllowed() bool {
	return n != nil && !n.schedulingDisabled.Load()
}

// MarshalJSON emits SchedulingDisabled from the atomic cordon flag.
func (n *Node) MarshalJSON() ([]byte, error) {
	type Alias Node
	return json.Marshal(&struct {
		*Alias
		SchedulingDisabled bool `json:"SchedulingDisabled"`
	}{
		Alias:              (*Alias)(n),
		SchedulingDisabled: n.SchedulingDisabled(),
	})
}

// UnmarshalJSON loads SchedulingDisabled into the atomic cordon flag.
func (n *Node) UnmarshalJSON(data []byte) error {
	type Alias Node
	aux := &struct {
		*Alias
		SchedulingDisabled bool `json:"SchedulingDisabled"`
	}{
		Alias: (*Alias)(n),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	n.SetSchedulingDisabled(aux.SchedulingDisabled)
	return nil
}

type nodeLabelsCache struct {
	labels map[string]string
}

type nodeLabelsCacheStore struct {
	cache atomic.Pointer[nodeLabelsCache]
}

var nodeLabelsCacheInitMu sync.Mutex

func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	// Clone provides a best-effort read-side snapshot. Mutable counters such
	// as LocalCreateNum and schedulingDisabled are refreshed via atomic loads
	// after the structural copy so cloned read models stay aligned with the
	// write path under concurrent updates.
	localCreateNum := atomic.LoadInt64(&n.LocalCreateNum)
	schedulingDisabled := n.SchedulingDisabled()
	cloned := *n
	cloned.LocalCreateNum = localCreateNum
	cloned.schedulingDisabled = atomic.Bool{}
	cloned.SetSchedulingDisabled(schedulingDisabled)
	cloned.labelsCache = nil
	if n.VirtualNodeQuotaArray != nil {
		cloned.VirtualNodeQuotaArray = append([]int64(nil), n.VirtualNodeQuotaArray...)
	}
	if n.NodeLabels != nil {
		cloned.NodeLabels = make(map[string]string, len(n.NodeLabels))
		for k, v := range n.NodeLabels {
			cloned.NodeLabels[k] = v
		}
	}
	return &cloned
}

func (n *Node) labelsCacheStore() *nodeLabelsCacheStore {
	if n.labelsCache != nil {
		return n.labelsCache
	}
	nodeLabelsCacheInitMu.Lock()
	defer nodeLabelsCacheInitMu.Unlock()
	if n.labelsCache == nil {
		n.labelsCache = &nodeLabelsCacheStore{}
	}
	return n.labelsCache
}

func (n *Node) ID() string {
	if n.InsID == "" {
		return n.IP
	}
	return n.InsID
}

func (n *Node) HostIP() string { return n.IP }

func (n *Node) LocalCreateNumIncrBy(i int64) int64 {
	return atomic.AddInt64(&n.LocalCreateNum, i)
}

func (n *Node) Labels() map[string]string {
	cacheStore := n.labelsCacheStore()
	if cache := cacheStore.cache.Load(); cache != nil {
		return cache.labels
	}

	// Canonical affinity keys derived from Node struct fields always take
	// priority over node-reported labels, so they are written last.
	labels := make(map[string]string, len(n.NodeLabels)+6)
	for k, v := range n.NodeLabels {
		labels[k] = v
	}
	labels[constants.AffinityKeyZone] = n.Zone
	labels[constants.AffinityKeyClusterID] = n.ClusterLabel
	labels[constants.AffinityKeyCPUType] = n.CPUType
	labels[constants.AffinityKeyMemorySize] = fmt.Sprintf("%dMi", n.QuotaMem)
	labels[constants.AffinityKeyCPUCores] = fmt.Sprintf("%dm", n.QuotaCpu)
	labels[constants.AffinityKeyInstanceType] = n.InstanceType
	cache := &nodeLabelsCache{labels: labels}
	if cacheStore.cache.CompareAndSwap(nil, cache) {
		return labels
	}
	if cache := cacheStore.cache.Load(); cache != nil {
		return cache.labels
	}
	return labels
}

func (n *Node) InvalidateLabelsCache() {
	if n.labelsCache != nil {
		n.labelsCache.cache.Store(nil)
	}
}

type NodeList []*Node

func (l *NodeList) Append(value ...*Node) NodeList {
	*l = append(*l, value...)
	return *l
}

func (l *NodeList) Remove(elems ...*Node) {
	for _, n := range elems {
		for i, v := range *l {
			if v.ID() == n.ID() {
				*l = append((*l)[:i], (*l)[i+1:]...)
			}
		}
	}
}

func (l *NodeList) Add(elems ...*Node) NodeList {
	for _, n := range elems {
		exist := false
		for _, v := range *l {
			if v.ID() == n.ID() {
				exist = true
			}
		}
		if exist {
			continue
		} else {
			*l = append(*l, n)
		}
	}
	return *l
}

func (l NodeList) Len() int {
	return len(l)
}

func (l NodeList) String() string {
	return utils.InterfaceToString(l)
}

func (l NodeList) AllSortByIndex() NodeList {
	sort.Slice(l, func(i, j int) bool {
		return l[i].Index < l[j].Index
	})
	return l
}

func (l NodeList) IndexByPage(index, pageSize int) ([]*Node, int) {
	size := l.Len()
	if size == 0 {
		return nil, -1
	}
	if pageSize <= 0 || index < 0 {
		return nil, -1
	}

	if !l.supportsIndexPagination() {
		start := index
		if start <= 0 {
			start = 1
		}
		if start > size {
			return nil, -1
		}
		startPos := start - 1
		endPos := startPos + pageSize
		if endPos > size {
			endPos = size
		}
		return l[startPos:endPos], endPos
	}

	maxIndex := l[size-1].Index
	if index > maxIndex {
		return nil, -1
	}

	if index == maxIndex {
		return l[size-1:], maxIndex
	}

	startIndex := 0
	for i, v := range l {
		if v.Index >= index {
			startIndex = i
			break
		}
	}

	endIndex := startIndex + pageSize
	if endIndex > size {
		endIndex = size
	}

	return l[startIndex:endIndex], l[endIndex-1].Index
}

func (l NodeList) supportsIndexPagination() bool {
	if len(l) == 0 {
		return false
	}
	prev := 0
	for _, n := range l {
		if n == nil || n.Index <= 0 {
			return false
		}
		if prev > 0 && n.Index < prev {
			return false
		}
		prev = n.Index
	}
	return true
}

type NodeScoreList []*NodeScore

type NodeScore struct {
	InsID string

	OrigNode *Node

	Score float64

	MvmNum int64
}

func (n *NodeScore) ID() string {
	return n.InsID
}

func (l *NodeScoreList) Append(value ...*NodeScore) NodeScoreList {
	*l = append(*l, value...)
	return *l
}

func (l *NodeScoreList) Remove(elems ...*NodeScore) {
	for _, n := range elems {
		for i, v := range *l {
			if v.InsID == n.InsID {
				*l = append((*l)[:i], (*l)[i+1:]...)
			}
		}
	}
}

func (l NodeScoreList) Len() int {
	return len(l)
}

func (l NodeScoreList) String() string {
	return utils.InterfaceToString(l)
}

func (l NodeScoreList) AllSortByScore() NodeScoreList {
	sort.Slice(l, func(i, j int) bool {
		return l[i].Score > l[j].Score
	})
	return l
}
