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

	selName         string
	rSelect         weighted.W
	resultWithScore node.NodeScoreList

	Affinity     Affinity
	InstanceType string
}

type Affinity struct {
	NodeSelector        affinity.NodeSelector
	BackoffNodeSelector affinity.NodeSelector
	NodePrefererd       affinity.PreferredSchedulingTerms
}

type RequestResource struct {
	Cpu            resource.Quantity
	Mem            resource.Quantity
	SystemDiskSize int64
	EnableSlowPath bool

	ErofsImages []*ImageSpec

	TemplateID             string
	TemplateNodeScope      []string
	EnforceSnapshotStorage bool
	// AllowNonLocalTemplate skips the "template／snapshot must already be
	// local on the node" check. Used for S3 remote_ready cross-node restore
	// where CubeCow loads objects on demand.
	AllowNonLocalTemplate bool
}

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
//
// Call it exactly once per scheduling request, BEFORE any filter narrows the
// candidate set: re-invoking after filtering re-clones the narrowed result
// and silently shrinks the "stable snapshot" SnapshotNodes advertises, and a
// fresh SnapshotVersion would desync external plugins mid-request. Note the
// frozen result and the snapshot share node pointers, so in-place mutation of
// a node (there should be none on the read path) is visible through both.
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

func (s *SelectorCtx) LeastNodes(n int) node.NodeList {
	size := s.result.Len()
	if n >= 0 && n <= size {
		return s.result[0:n]
	}
	return s.result
}

func (s *SelectorCtx) SetNodes(list node.NodeList) {
	s.result = list
	s.resultWithScore = nil
}

func (s *SelectorCtx) LeastScoreNodes(n int) node.NodeScoreList {
	size := s.resultWithScore.Len()
	if n >= 0 && n <= size {
		return s.resultWithScore[0:n]
	}
	return s.resultWithScore
}

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

func (s *SelectorCtx) GetResCpuFromCtx() *resource.Quantity {
	if s.ReqRes == nil {
		return nil
	}
	return &s.ReqRes.Cpu
}

func (s *SelectorCtx) GetResMemFromCtx() *resource.Quantity {
	if s.ReqRes == nil {
		return nil
	}
	return &s.ReqRes.Mem
}

func (s *SelectorCtx) GetReqRes() *RequestResource {
	return s.ReqRes
}

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

func (s *SelectorCtx) AddLastBadNode(n *node.Node) {
	s.lastBadFilters = append(s.lastBadFilters, n)
}

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
