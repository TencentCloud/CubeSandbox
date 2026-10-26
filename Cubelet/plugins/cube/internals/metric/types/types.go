// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import "sync"

type GetMetric func() (any, error)

type CollectRegister struct {
	mu sync.Mutex
	r  map[MetricType][]GetMetric
}

func NewCollectRegister() *CollectRegister {
	return &CollectRegister{
		r: make(map[MetricType][]GetMetric),
	}
}

func (c *CollectRegister) AddCollector(t MetricType, f GetMetric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.r[t] = append(c.r[t], f)
}

func (c *CollectRegister) Get(t MetricType) []GetMetric {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.r[t]
}

type MetricProvider interface {
	RegisterMetrics(register *CollectRegister) error
}

type MetricType string

const (
	MetricTypeOSS = "oss"

	MetricTypeCLS = "cls"
)

type Metric struct {
	Type  MetricType
	Value any
}
