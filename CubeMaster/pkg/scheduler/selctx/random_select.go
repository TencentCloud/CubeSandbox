// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package selctx

import "golang.org/x/exp/rand"

// randomSelect 简单随机选择器：所有元素等概率被选中，权重参数被忽略
type randomSelect struct {
	items []interface{}
	r     *rand.Rand
}

// Next 从元素列表中随机返回一个元素
func (r *randomSelect) Next() (item interface{}) {
	return r.items[r.r.Intn(len(r.items))]
}

// Add 追加一个元素（weighted.W 接口要求实现，随机选择忽略权重）
func (r *randomSelect) Add(item interface{}, weight int) {
	r.items = append(r.items, item)
}

// All 随机选择器不维护权重表，返回 nil
func (r *randomSelect) All() map[interface{}]int {
	return nil
}

// RemoveAll 清空已累积的元素，避免复用选择器时使用过期候选。
func (r *randomSelect) RemoveAll() {
	r.items = nil
}

// Reset 随机选择器无均衡状态需要重置，空实现
func (r *randomSelect) Reset() {}
