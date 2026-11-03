// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package selctx

import "golang.org/x/exp/rand"

// randomSelect picks every element with equal probability; the weighted.W
// interface's weight parameter is ignored. Call RemoveAll before reuse to
// clear stale candidates.
type randomSelect struct {
	items []interface{}
	r     *rand.Rand
}

func (r *randomSelect) Next() (item interface{}) {
	return r.items[r.r.Intn(len(r.items))]
}

func (r *randomSelect) Add(item interface{}, weight int) {
	r.items = append(r.items, item)
}

func (r *randomSelect) All() map[interface{}]int {
	return nil
}

func (r *randomSelect) RemoveAll() {
	r.items = nil
}

func (r *randomSelect) Reset() {}
