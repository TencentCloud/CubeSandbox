// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxspec

import (
	"container/list"
	"sync"
	"time"
)

// createdAtCacheSize bounds the memo. One entry holds a UUID-sized key, a
// timestamp and the list bookkeeping — well under 200 bytes — so the cap
// keeps the whole map in the low megabytes even for a host that pauses tens
// of thousands of sandboxes.
const createdAtCacheSize = 8192

// createdAtMemo memoizes the immutable creation time of a sandbox spec.
//
// Only reads populate it, so every value is a copy of the durable column and
// cannot disagree with the database. The timestamp never changes after the
// first insert (doPut omits created_at from the upsert's update columns), so
// entries need no revalidation; the LRU bound and delete keep the footprint
// finite when a destroy or an out-of-process purge never reaches this map.
type createdAtMemo struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type createdAtEntry struct {
	sandboxID string
	createdAt time.Time
}

func newCreatedAtMemo(max int) *createdAtMemo {
	if max <= 0 {
		max = createdAtCacheSize
	}
	return &createdAtMemo{
		max:   max,
		order: list.New(),
		items: make(map[string]*list.Element, max),
	}
}

// load returns the memoized time and marks the entry as most recently used.
func (m *createdAtMemo) load(sandboxID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[sandboxID]
	if !ok {
		return time.Time{}, false
	}
	m.order.MoveToFront(el)
	return el.Value.(*createdAtEntry).createdAt, true
}

// store inserts or refreshes an entry, evicting the least recently used entry
// once the cap is reached.
func (m *createdAtMemo) store(sandboxID string, createdAt time.Time) {
	if sandboxID == "" || createdAt.IsZero() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[sandboxID]; ok {
		el.Value.(*createdAtEntry).createdAt = createdAt
		m.order.MoveToFront(el)
		return
	}
	el := m.order.PushFront(&createdAtEntry{sandboxID: sandboxID, createdAt: createdAt})
	m.items[sandboxID] = el
	for m.order.Len() > m.max {
		oldest := m.order.Back()
		if oldest == nil {
			break
		}
		m.order.Remove(oldest)
		delete(m.items, oldest.Value.(*createdAtEntry).sandboxID)
	}
}

// delete drops the entry for sandboxID, if any.
func (m *createdAtMemo) delete(sandboxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[sandboxID]
	if !ok {
		return
	}
	m.order.Remove(el)
	delete(m.items, sandboxID)
}
