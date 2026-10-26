// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Node struct {
	metav1.TypeMeta   `json:",inline,omitempty"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeSpec   `json:"spec,omitempty"`
	Status            NodeStatus `json:"status,omitempty"`
}

type NodeSpec struct {
	Unschedulable bool           `json:"unschedulable,omitempty"`
	Taints        []corev1.Taint `json:"taints,omitempty"`
	ProviderID    string         `json:"provider_id,omitempty"`
	InstanceType  string         `json:"instance_type,omitempty"`
}

type NodeStatus struct {
	Capacity      map[corev1.ResourceName]resource.Quantity `json:"capacity,omitempty"`
	Allocatable   map[corev1.ResourceName]resource.Quantity `json:"allocatable,omitempty"`
	Conditions    []corev1.NodeCondition                    `json:"conditions,omitempty"`
	Addresses     []corev1.NodeAddress                      `json:"addresses,omitempty"`
	NodeInfo      corev1.NodeSystemInfo                     `json:"node_info,omitempty"`
	CubeImages    []ContainerImage                          `json:"cube_images,omitempty"`
	CubeTemplates []LocalTemplate                           `json:"cube_templates,omitempty"`
}

type ContainerImage struct {
	Names     []string `json:"names,omitempty"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
}

type LocalTemplate struct {
	TemplateID string `json:"template_id,omitempty"`
	ID         string `json:"id,omitempty"`
	Media      string `json:"media,omitempty"`
	Path       string `json:"path,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
}

func (n *Node) DeepCopy() *Node {
	if n == nil {
		return nil
	}
	out := *n
	out.ObjectMeta = *n.ObjectMeta.DeepCopy()
	out.Spec = n.Spec.deepCopy()
	out.Status = *n.Status.DeepCopy()
	return &out
}

func (s NodeSpec) deepCopy() NodeSpec {
	out := s
	out.Taints = append([]corev1.Taint(nil), s.Taints...)
	return out
}

func (s *NodeStatus) DeepCopy() *NodeStatus {
	if s == nil {
		return nil
	}
	out := *s
	if s.Capacity != nil {
		out.Capacity = make(map[corev1.ResourceName]resource.Quantity, len(s.Capacity))
		for k, v := range s.Capacity {
			out.Capacity[k] = v.DeepCopy()
		}
	}
	if s.Allocatable != nil {
		out.Allocatable = make(map[corev1.ResourceName]resource.Quantity, len(s.Allocatable))
		for k, v := range s.Allocatable {
			out.Allocatable[k] = v.DeepCopy()
		}
	}
	out.Conditions = append([]corev1.NodeCondition(nil), s.Conditions...)
	out.Addresses = append([]corev1.NodeAddress(nil), s.Addresses...)
	out.CubeImages = append([]ContainerImage(nil), s.CubeImages...)
	out.CubeTemplates = append([]LocalTemplate(nil), s.CubeTemplates...)
	return &out
}

func GetNodeCondition(status *NodeStatus, conditionType corev1.NodeConditionType) (int, *corev1.NodeCondition) {
	if status == nil {
		return -1, nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			return i, &status.Conditions[i]
		}
	}
	return -1, nil
}
