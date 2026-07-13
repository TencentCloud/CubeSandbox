// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package discovery

import (
	"sort"
	"sync"
	"testing"
)

func TestStatic_SnapshotIsDefensiveCopy(t *testing.T) {
	s := NewStatic([]string{"http://a", "http://b"})
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(snap))
	}
	// Mutating the returned slice must not affect subsequent Snapshot calls.
	snap[0].AdminURL = "tampered"
	snap2 := s.Snapshot()
	if snap2[0].AdminURL != "http://a" {
		t.Fatalf("Static.Snapshot returned aliased slice; got %q", snap2[0].AdminURL)
	}
}

func TestDecodeEndpoint(t *testing.T) {
	ep, err := decodeEndpoint(`{"proxy_id":"10.0.0.1:8082","admin_url":"http://10.0.0.1:8082","node_ip":"10.0.0.1","started_at":123,"version":"v1"}`)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if ep.ProxyID != "10.0.0.1:8082" || ep.AdminURL != "http://10.0.0.1:8082" ||
		ep.NodeIP != "10.0.0.1" || ep.StartedAt != 123 || ep.Version != "v1" {
		t.Fatalf("unexpected decode: %+v", ep)
	}

	if _, err := decodeEndpoint("not-json"); err == nil {
		t.Fatal("decodeEndpoint accepted non-JSON")
	}
}

func TestDiffJoins(t *testing.T) {
	prev := map[string]*live{
		"a": {ep: Endpoint{ProxyID: "a"}},
		"b": {ep: Endpoint{ProxyID: "b"}},
	}
	cur := map[string]*live{
		"b": {ep: Endpoint{ProxyID: "b"}},
		"c": {ep: Endpoint{ProxyID: "c"}},
	}
	joins, leaves := diffJoins(prev, cur)
	sort.Strings(joins)
	sort.Strings(leaves)
	if len(joins) != 1 || joins[0] != "c" {
		t.Fatalf("joins wrong: %v", joins)
	}
	if len(leaves) != 1 || leaves[0] != "a" {
		t.Fatalf("leaves wrong: %v", leaves)
	}
}

func TestSnapshotMap_ConcurrentReads(t *testing.T) {
	var mu sync.RWMutex
	m := map[string]*live{
		"a": {ep: Endpoint{ProxyID: "a", AdminURL: "http://a"}},
		"b": {ep: Endpoint{ProxyID: "b", AdminURL: "http://b"}},
	}
	// Snapshot should be safe for parallel callers even while writers hold
	// the mutex in short bursts.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				snap := snapshotMap(m, &mu)
				if len(snap) != 2 {
					t.Errorf("expected 2, got %d", len(snap))
					return
				}
			}
		}()
	}
	wg.Wait()
}
