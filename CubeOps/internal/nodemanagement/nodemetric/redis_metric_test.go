// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"errors"
	"testing"
	"time"
)

func TestWriteNodeMetric_NilPool(t *testing.T) {
	cleanup := SetWriteNodeMetricHook(nil)
	defer cleanup()

	m := &NodeMetric{
		NodeID:        "node-1",
		MetricTime:    time.Now(),
		HasAllocated:  true,
		MilliCPUUsage: 1000,
	}
	if err := WriteNodeMetric(m); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
}

func TestWriteNodeMetric_EmptyMetrics(t *testing.T) {
	var called bool
	cleanup := SetWriteNodeMetricHook(func(m *NodeMetric) error {
		called = true
		return nil
	})
	defer cleanup()

	m := &NodeMetric{NodeID: "node-1", MetricTime: time.Now()}
	if err := WriteNodeMetric(m); err != nil {
		t.Fatalf("write: %v", err)
	}
	if called {
		t.Error("expected hook not to be called for empty metrics")
	}
}

func TestWriteNodeMetric_MissingNodeID(t *testing.T) {
	cleanup := SetWriteNodeMetricHook(func(m *NodeMetric) error {
		return nil
	})
	defer cleanup()

	m := &NodeMetric{HasAllocated: true, MilliCPUUsage: 1000}
	if err := WriteNodeMetric(m); err == nil {
		t.Error("expected error for missing node id")
	}
}

func TestWriteNodeMetric_HookErrorPropagated(t *testing.T) {
	wantErr := errors.New("redis down")
	cleanup := SetWriteNodeMetricHook(func(m *NodeMetric) error {
		return wantErr
	})
	defer cleanup()

	m := &NodeMetric{NodeID: "node-1", MetricTime: time.Now(), HasAllocated: true, MilliCPUUsage: 1000}
	if err := WriteNodeMetric(m); err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
