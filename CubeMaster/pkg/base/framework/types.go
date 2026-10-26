// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package framework

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
)

type ImageStateSummary struct {
	OssClusterLabel string

	Size int64

	NumNodes int

	nodes sets.Set[string]

	mu sync.RWMutex

	ScaledImageScore int64
	UpdateAt         time.Time
}

func NewImageStateSummary(size int64, ossClusterLabel string, initialNodes ...string) *ImageStateSummary {
	return &ImageStateSummary{
		Size:            size,
		OssClusterLabel: ossClusterLabel,
		nodes:           sets.New(initialNodes...),
		UpdateAt:        time.Now(),
	}
}

func (iss *ImageStateSummary) AddNode(nodeName string) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	if iss.nodes == nil {
		iss.nodes = sets.New[string]()
	}
	iss.nodes.Insert(nodeName)
}

func (iss *ImageStateSummary) RemoveNode(nodeName string) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	if iss.nodes != nil {
		iss.nodes.Delete(nodeName)
	}
}

func (iss *ImageStateSummary) HasNode(nodeName string) bool {
	iss.mu.RLock()
	defer iss.mu.RUnlock()
	if iss.nodes == nil {
		return false
	}
	return iss.nodes.Has(nodeName)
}

func (iss *ImageStateSummary) GetNumNodes() int {
	iss.mu.RLock()
	defer iss.mu.RUnlock()
	if iss.nodes == nil {
		return 0
	}
	return iss.nodes.Len()
}

func (iss *ImageStateSummary) Snapshot() *ImageStateSummary {
	iss.mu.RLock()
	defer iss.mu.RUnlock()

	numNodes := 0
	if iss.nodes != nil {
		numNodes = iss.nodes.Len()
	}

	return &ImageStateSummary{
		Size:     iss.Size,
		NumNodes: numNodes,
	}
}
