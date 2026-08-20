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

// RemoveAll 随机选择器无需清空权重表，空实现
func (r *randomSelect) RemoveAll() {}

// Reset 随机选择器无需重置状态，空实现
func (r *randomSelect) Reset() {}
