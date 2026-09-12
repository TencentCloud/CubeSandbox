// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"strings"
	"sync"
	"sync/atomic"
)

// nodeTemplateCreateCounters 记录本 CubeMaster 视角下每个 (node, template)
// 组合的在途创建数：创建路径选中节点后 +1、Cubelet 调用返回后 -1。
// 语义与 Node.LocalCreateNum 一致：本地内存计数、不做跨 Master 同步，
// 多 Master 部署时打分侧按 HealthyMasterNodes 折算全局估计。
var nodeTemplateCreateCounters sync.Map // key: nodeID + "\x00" + templateID -> *int64

func nodeTemplateCreateKey(nodeID, templateID string) string {
	return nodeID + "\x00" + templateID
}

// IncrNodeTemplateCreate 登记一次 (node, template) 在途创建
func IncrNodeTemplateCreate(nodeID, templateID string) {
	addNodeTemplateCreate(nodeID, templateID, 1)
}

// DecrNodeTemplateCreate 归还一次 (node, template) 在途创建
func DecrNodeTemplateCreate(nodeID, templateID string) {
	addNodeTemplateCreate(nodeID, templateID, -1)
}

func addNodeTemplateCreate(nodeID, templateID string, delta int64) {
	if nodeID == "" || templateID == "" {
		return
	}
	key := nodeTemplateCreateKey(nodeID, templateID)
	value, ok := nodeTemplateCreateCounters.Load(key)
	if !ok {
		value, _ = nodeTemplateCreateCounters.LoadOrStore(key, new(int64))
	}
	atomic.AddInt64(value.(*int64), delta)
}

// NodeTemplateCreateNum 返回本 Master 记录的 (node, template) 在途创建数
func NodeTemplateCreateNum(nodeID, templateID string) int64 {
	if nodeID == "" || templateID == "" {
		return 0
	}
	value, ok := nodeTemplateCreateCounters.Load(nodeTemplateCreateKey(nodeID, templateID))
	if !ok {
		return 0
	}
	num := atomic.LoadInt64(value.(*int64))
	if num < 0 {
		return 0
	}
	return num
}

// NodeTemplateCreateLoad 返回 (node, template) 在途创建的全局估计：
// 本地计数乘以健康 Master 数，折算口径与 getLocalCreateNumScore 一致
func NodeTemplateCreateLoad(nodeID, templateID string) int64 {
	return NodeTemplateCreateNum(nodeID, templateID) * HealthyMasterNodes()
}

// deleteNodeTemplateCreateCounters 节点下线时清理其全部 (node, template) 在途计数，
// 避免节点键随模板数量无界增长
func deleteNodeTemplateCreateCounters(nodeID string) {
	if nodeID == "" {
		return
	}
	prefix := nodeID + "\x00"
	nodeTemplateCreateCounters.Range(func(key, _ any) bool {
		if name, ok := key.(string); ok && strings.HasPrefix(name, prefix) {
			nodeTemplateCreateCounters.Delete(key)
		}
		return true
	})
}
