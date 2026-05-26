// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package selctx provides context with selected node
package selctx

import (
	"context"
	"time"

	"github.com/smallnest/weighted"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"golang.org/x/exp/rand"
	"k8s.io/apimachinery/pkg/api/resource"
)

type SelectorCtx struct {
	Ctx            context.Context
	ReqRes         *RequestResource
	lastBadFilters []*node.Node
	result         node.NodeList

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
}

type ImageSpec struct {
	ImageID string
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

func (s *SelectorCtx) LeastNodes(n int) node.NodeList {
	size := s.result.Len()
	if n >= 0 && n <= size {
		return s.result[0:n]
	}
	return s.result
}

func (s *SelectorCtx) SetNodes(list node.NodeList) {
	s.result = list
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
