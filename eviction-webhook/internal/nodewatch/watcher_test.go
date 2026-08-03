// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodewatch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeWithConditions builds a *corev1.Node with the supplied conditions.
func nodeWithConditions(conditions []corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}
}

// TestHasMemoryPressure verifies that a node with MemoryPressure=True returns true.
func TestHasMemoryPressure(t *testing.T) {
	node := nodeWithConditions([]corev1.NodeCondition{
		{
			Type:   corev1.NodeMemoryPressure,
			Status: corev1.ConditionTrue,
		},
	})
	assert.True(t, HasMemoryPressure(node), "expected HasMemoryPressure to return true when MemoryPressure condition is True")
}

// TestHasMemoryPressureFalse verifies that a node without MemoryPressure returns false.
func TestHasMemoryPressureFalse(t *testing.T) {
	t.Run("no conditions at all", func(t *testing.T) {
		node := nodeWithConditions(nil)
		assert.False(t, HasMemoryPressure(node))
	})

	t.Run("MemoryPressure condition set to False", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{
				Type:   corev1.NodeMemoryPressure,
				Status: corev1.ConditionFalse,
			},
		})
		assert.False(t, HasMemoryPressure(node))
	})

	t.Run("only unrelated conditions present", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			},
		})
		assert.False(t, HasMemoryPressure(node))
	})
}

// TestHasResourcePressure verifies that a node with DiskPressure or PIDPressure returns true.
func TestHasResourcePressure(t *testing.T) {
	t.Run("DiskPressure is True", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{
				Type:   corev1.NodeDiskPressure,
				Status: corev1.ConditionTrue,
			},
		})
		assert.True(t, HasResourcePressure(node))
	})

	t.Run("PIDPressure is True", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{
				Type:   corev1.NodePIDPressure,
				Status: corev1.ConditionTrue,
			},
		})
		assert.True(t, HasResourcePressure(node))
	})

	t.Run("MemoryPressure is True", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{
				Type:   corev1.NodeMemoryPressure,
				Status: corev1.ConditionTrue,
			},
		})
		assert.True(t, HasResourcePressure(node), "MemoryPressure is also a resource pressure")
	})
}

// TestHasResourcePressureFalse verifies that a node with no pressure conditions returns false.
func TestHasResourcePressureFalse(t *testing.T) {
	t.Run("no conditions at all", func(t *testing.T) {
		node := nodeWithConditions(nil)
		assert.False(t, HasResourcePressure(node))
	})

	t.Run("all pressure conditions set to False", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
			{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse},
		})
		assert.False(t, HasResourcePressure(node))
	})

	t.Run("only NodeReady condition present", func(t *testing.T) {
		node := nodeWithConditions([]corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		})
		assert.False(t, HasResourcePressure(node))
	})
}
