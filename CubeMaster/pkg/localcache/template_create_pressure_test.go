// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNodeTemplateCreateNumIncrDecr(t *testing.T) {
	key := nodeTemplateCreateKey("node-tpl-incr", "tpl-1")
	nodeTemplateCreateCounters.Delete(key)
	t.Cleanup(func() { nodeTemplateCreateCounters.Delete(key) })

	if got := NodeTemplateCreateNum("node-tpl-incr", "tpl-1"); got != 0 {
		t.Fatalf("initial count = %d, want 0", got)
	}

	IncrNodeTemplateCreate("node-tpl-incr", "tpl-1")
	IncrNodeTemplateCreate("node-tpl-incr", "tpl-1")
	if got := NodeTemplateCreateNum("node-tpl-incr", "tpl-1"); got != 2 {
		t.Fatalf("count after two incr = %d, want 2", got)
	}

	DecrNodeTemplateCreate("node-tpl-incr", "tpl-1")
	if got := NodeTemplateCreateNum("node-tpl-incr", "tpl-1"); got != 1 {
		t.Fatalf("count after decr = %d, want 1", got)
	}

	// 不同 (node, template) 组合之间互不影响
	if got := NodeTemplateCreateNum("node-tpl-incr", "tpl-2"); got != 0 {
		t.Fatalf("other template count = %d, want 0", got)
	}
	if got := NodeTemplateCreateNum("node-other", "tpl-1"); got != 0 {
		t.Fatalf("other node count = %d, want 0", got)
	}
}

func TestNodeTemplateCreateNumEmptyID(t *testing.T) {
	IncrNodeTemplateCreate("", "tpl-1")
	IncrNodeTemplateCreate("node-tpl-empty", "")
	if got := NodeTemplateCreateNum("", "tpl-1"); got != 0 {
		t.Fatalf("empty node count = %d, want 0", got)
	}
	if got := NodeTemplateCreateNum("node-tpl-empty", ""); got != 0 {
		t.Fatalf("empty template count = %d, want 0", got)
	}
}

func TestNodeTemplateCreateNumNeverNegative(t *testing.T) {
	key := nodeTemplateCreateKey("node-tpl-neg", "tpl-1")
	nodeTemplateCreateCounters.Delete(key)
	t.Cleanup(func() { nodeTemplateCreateCounters.Delete(key) })

	// 节点下线清理后迟到的 decr 会重建出负计数，读取侧按 0 兜底
	DecrNodeTemplateCreate("node-tpl-neg", "tpl-1")
	if got := NodeTemplateCreateNum("node-tpl-neg", "tpl-1"); got != 0 {
		t.Fatalf("count after lone decr = %d, want 0", got)
	}
}

func TestNodeTemplateCreateLoadScalesByMasterNodes(t *testing.T) {
	key := nodeTemplateCreateKey("node-tpl-load", "tpl-1")
	nodeTemplateCreateCounters.Delete(key)
	t.Cleanup(func() { nodeTemplateCreateCounters.Delete(key) })

	orig := atomic.LoadInt64(&l.totalSelfNodes)
	t.Cleanup(func() { atomic.StoreInt64(&l.totalSelfNodes, orig) })
	atomic.StoreInt64(&l.totalSelfNodes, 3)

	IncrNodeTemplateCreate("node-tpl-load", "tpl-1")
	IncrNodeTemplateCreate("node-tpl-load", "tpl-1")
	if got := NodeTemplateCreateLoad("node-tpl-load", "tpl-1"); got != 6 {
		t.Fatalf("load = %d, want 2*3=6", got)
	}
}

func TestNodeTemplateCreateConcurrent(t *testing.T) {
	nodeID := "node-tpl-conc"
	key := nodeTemplateCreateKey(nodeID, "tpl-1")
	nodeTemplateCreateCounters.Delete(key)
	t.Cleanup(func() { nodeTemplateCreateCounters.Delete(key) })

	const workers = 8
	const loops = 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			templateID := fmt.Sprintf("tpl-%d", w%3)
			for i := 0; i < loops; i++ {
				IncrNodeTemplateCreate(nodeID, templateID)
				DecrNodeTemplateCreate(nodeID, templateID)
			}
		}(w)
	}
	wg.Wait()

	for w := 0; w < 3; w++ {
		if got := NodeTemplateCreateNum(nodeID, fmt.Sprintf("tpl-%d", w)); got != 0 {
			t.Fatalf("count for tpl-%d = %d, want 0", w, got)
		}
	}
}

func TestDeleteNodeTemplateCreateCounters(t *testing.T) {
	nodeID := "node-tpl-del"
	t.Cleanup(func() { deleteNodeTemplateCreateCounters(nodeID) })

	IncrNodeTemplateCreate(nodeID, "tpl-1")
	IncrNodeTemplateCreate(nodeID, "tpl-2")
	IncrNodeTemplateCreate("node-tpl-keep", "tpl-1")
	defer DecrNodeTemplateCreate("node-tpl-keep", "tpl-1")

	deleteNodeTemplateCreateCounters(nodeID)

	if got := NodeTemplateCreateNum(nodeID, "tpl-1"); got != 0 {
		t.Fatalf("count after node cleanup = %d, want 0", got)
	}
	if got := NodeTemplateCreateNum("node-tpl-keep", "tpl-1"); got != 1 {
		t.Fatalf("other node count = %d, want 1", got)
	}
}
