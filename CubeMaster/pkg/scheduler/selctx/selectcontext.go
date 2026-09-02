// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package selctx provides context with selected node
package selctx

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/smallnest/weighted"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"golang.org/x/exp/rand"
	"k8s.io/apimachinery/pkg/api/resource"
)

// SelectorCtx 一次调度选择的上下文：贯穿预过滤、过滤、评分、最终选择全过程，
// 保存候选节点列表、节点评分列表、请求资源与亲和性配置等
type SelectorCtx struct {
	Ctx             context.Context
	ReqRes          *RequestResource
	RequestLabels   map[string]string
	ProfileName     string
	SnapshotVersion string
	lastBadFilters  []*node.Node
	result          node.NodeList
	snapshot        node.NodeList
	snapshotFacts   map[string]SnapshotNodeFacts

	selName         string // 选择算法名称（random/sw/rw/rrw）
	rSelect         weighted.W
	resultWithScore node.NodeScoreList

	Affinity     Affinity
	InstanceType string
}

// Affinity 节点亲和性配置：硬性选择器、兜底选择器与软性偏好评分
type Affinity struct {
	NodeSelector        affinity.NodeSelector             // 硬性约束：不满足则节点被排除
	BackoffNodeSelector affinity.NodeSelector             // 兜底路径使用的宽松选择器
	NodePrefererd       affinity.PreferredSchedulingTerms // 软性偏好：用于评分
}

// RequestResource 调度请求的资源描述
type RequestResource struct {
	Cpu            resource.Quantity
	Mem            resource.Quantity
	SystemDiskSize int64
	EnableSlowPath bool // 是否启用慢路径（放宽过滤条件）

	ErofsImages []*ImageSpec

	TemplateID             string   // 模板 ID（用于模板本地化过滤）
	TemplateNodeScope      []string // 模板允许调度的节点范围
	EnforceSnapshotStorage bool     // 是否强制要求快照存储可用
	// AllowNonLocalTemplate skips the "template／snapshot must already be
	// local on the node" check. Used for S3 remote_ready cross-node restore
	// where CubeCow loads objects on demand.
	AllowNonLocalTemplate bool
}

// ImageSpec 镜像描述
type ImageSpec struct {
	ImageID string
}

type SnapshotNodeFacts struct {
	TemplateLocal          bool
	TemplateLocalKnown     bool
	SnapshotStorageAllowed bool
	SnapshotStorageKnown   bool
}

var snapshotSequence atomic.Uint64

// FreezeSnapshot defensively clones all mutable request and node data used by
// plugins and assigns a version shared by the whole scheduling pipeline. The
// local cache already returns clones; this second boundary makes the snapshot
// ownership explicit and protects callers that inject nodes in tests or
// benchmark simulations.
func (s *SelectorCtx) FreezeSnapshot() {
	if s == nil {
		return
	}
	if s.Ctx == nil {
		s.Ctx = context.Background()
	}
	if s.result != nil {
		frozen := make(node.NodeList, 0, len(s.result))
		for _, candidate := range s.result {
			frozen = append(frozen, candidate.Clone())
		}
		s.result = frozen
		s.snapshot = append(node.NodeList(nil), frozen...)
	}
	if s.ReqRes != nil {
		request := *s.ReqRes
		request.TemplateNodeScope = append([]string(nil), s.ReqRes.TemplateNodeScope...)
		if s.ReqRes.ErofsImages != nil {
			request.ErofsImages = make([]*ImageSpec, 0, len(s.ReqRes.ErofsImages))
			for _, image := range s.ReqRes.ErofsImages {
				if image == nil {
					request.ErofsImages = append(request.ErofsImages, nil)
					continue
				}
				cloned := *image
				request.ErofsImages = append(request.ErofsImages, &cloned)
			}
		}
		s.ReqRes = &request
	}
	if s.RequestLabels != nil {
		labels := make(map[string]string, len(s.RequestLabels))
		for key, value := range s.RequestLabels {
			labels[key] = value
		}
		s.RequestLabels = labels
	}
	if s.snapshotFacts != nil {
		facts := make(map[string]SnapshotNodeFacts, len(s.snapshotFacts))
		for nodeID, value := range s.snapshotFacts {
			facts[nodeID] = value
		}
		s.snapshotFacts = facts
	}
	s.SnapshotVersion = fmt.Sprintf("%d-%d", time.Now().UnixNano(), snapshotSequence.Add(1))
}

// New 创建选择上下文，并按名称初始化最终的加权随机选择器：
// random 随机选择；sw 平滑加权轮询；rw 随机加权；rrw 轮询加权
func New(name string) *SelectorCtx {
	s := &SelectorCtx{
		selName: name,
	}
	switch name {
	case "random":
		s.rSelect = &randomSelect{
			r: rand.New(rand.NewSource(uint64(time.Now().UnixNano()))),
		}
	case "sw":
		s.rSelect = &weighted.SW{}
	case "rw":
		s.rSelect = weighted.NewRandW()
	case "rrw":
		s.rSelect = &weighted.RRW{}
	default:
		s.rSelect = &randomSelect{
			r: rand.New(rand.NewSource(uint64(time.Now().UnixNano()))),
		}
	}
	return s
}

// Nodes 返回当前候选节点列表
func (s *SelectorCtx) Nodes() node.NodeList {
	return s.result
}

// SnapshotNodes returns the complete frozen node set for SnapshotVersion. It
// remains stable while result is narrowed by Filter plugins.
func (s *SelectorCtx) SnapshotNodes() node.NodeList {
	return s.snapshot
}

func (s *SelectorCtx) SetSnapshotFacts(facts map[string]SnapshotNodeFacts) {
	s.snapshotFacts = facts
}

func (s *SelectorCtx) SnapshotFacts(nodeID string) (SnapshotNodeFacts, bool) {
	if s == nil || s.snapshotFacts == nil {
		return SnapshotNodeFacts{}, false
	}
	facts, ok := s.snapshotFacts[nodeID]
	return facts, ok
}

// LeastNodes 返回候选列表中的前 n 个节点（n 超出范围时返回全部）
func (s *SelectorCtx) LeastNodes(n int) node.NodeList {
	size := s.result.Len()
	if n >= 0 && n <= size {
		return s.result[0:n]
	}
	return s.result
}

// SetNodes 设置候选节点列表
func (s *SelectorCtx) SetNodes(list node.NodeList) {
	s.result = list
	s.resultWithScore = nil
}

// LeastScoreNodes 返回评分列表中的前 n 个节点
func (s *SelectorCtx) LeastScoreNodes(n int) node.NodeScoreList {
	size := s.resultWithScore.Len()
	if n >= 0 && n <= size {
		return s.resultWithScore[0:n]
	}
	return s.resultWithScore
}

// SetNodeScoreList 设置评分结果列表，并同步更新候选节点列表为评分对应的节点
func (s *SelectorCtx) SetNodeScoreList(list node.NodeScoreList) {
	s.resultWithScore = list

	if list.Len() == 0 {
		return
	}
	s.result = s.result[:0]

	for i := range list {
		s.result = append(s.result, list[i].OrigNode)
	}
}

// GetResCpuFromCtx 返回请求的 CPU 资源
func (s *SelectorCtx) GetResCpuFromCtx() *resource.Quantity {
	if s.ReqRes == nil {
		return nil
	}
	return &s.ReqRes.Cpu
}

// GetResMemFromCtx 返回请求的内存资源
func (s *SelectorCtx) GetResMemFromCtx() *resource.Quantity {
	if s.ReqRes == nil {
		return nil
	}
	return &s.ReqRes.Mem
}

// GetReqRes 返回请求资源描述
func (s *SelectorCtx) GetReqRes() *RequestResource {
	return s.ReqRes
}

// LeastRandomSelect 从评分最高的前 n 个节点中选择一个候选节点。
// 是否使用传入权重取决于配置的 selector；默认 random selector 会忽略权重并均匀随机。
func (s *SelectorCtx) LeastRandomSelect(n int) *node.Node {
	if s.resultWithScore.Len() == 0 {

		leastNodes := s.LeastNodes(n)
		for i := range leastNodes {
			s.rSelect.Add(leastNodes[i], 1)
		}
	} else if s.resultWithScore.Len() > 0 {
		leastNodes := s.LeastScoreNodes(n)
		for i := range leastNodes {
			s.rSelect.Add(leastNodes[i].OrigNode, int(leastNodes[i].Score*1e6))
		}
	} else {
		return nil
	}

	item := s.rSelect.Next()
	rn, ok := item.(*node.Node)
	if !ok {
		return nil
	}
	return rn
}

// AddLastBadNode 记录一个最近过滤掉的节点，用于避免短时间内重复选中
func (s *SelectorCtx) AddLastBadNode(n *node.Node) {
	s.lastBadFilters = append(s.lastBadFilters, n)
}

// FilterOut 判断节点是否在最近被过滤掉的名单中
func (s *SelectorCtx) FilterOut(n *node.Node) bool {
	if s.lastBadFilters == nil {
		return false
	}
	if n == nil {
		return true
	}
	for i := range s.lastBadFilters {
		if s.lastBadFilters[i].ID() == n.ID() {
			return true
		}
	}
	return false
}
