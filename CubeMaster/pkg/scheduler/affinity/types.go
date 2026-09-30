// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package affinity

// NodeSelectorOperator 节点选择器操作符类型
type NodeSelectorOperator string

const (
	NodeSelectorOpIn           NodeSelectorOperator = "In"           // 标签值在给定集合中
	NodeSelectorOpNotIn        NodeSelectorOperator = "NotIn"        // 标签值不在给定集合中
	NodeSelectorOpExists       NodeSelectorOperator = "Exists"       // 标签存在
	NodeSelectorOpDoesNotExist NodeSelectorOperator = "DoesNotExist" // 标签不存在
	NodeSelectorOpGt           NodeSelectorOperator = "Gt"           // 标签资源量大于阈值
	NodeSelectorOpLt           NodeSelectorOperator = "Lt"           // 标签资源量小于阈值
)

// NodeSelectorRequirement 单条节点选择需求：标签键 + 操作符 + 候选值集合
type NodeSelectorRequirement struct {
	Key string `json:"key" protobuf:"bytes,1,opt,name=key"`

	Operator NodeSelectorOperator `json:"operator" protobuf:"bytes,2,opt,name=operator,casttype=NodeSelectorOperator"`

	Values map[string]any `json:"values,omitempty" protobuf:"bytes,3,rep,name=values"`
}

// NodeSelectorTerm 一组节点选择条款：包含按标签表达式匹配与按字段匹配两类需求
type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty" protobuf:"bytes,1,rep,name=matchExpressions"`

	MatchFields []NodeSelectorRequirement `json:"matchFields,omitempty" protobuf:"bytes,2,rep,name=matchFields"`
}
