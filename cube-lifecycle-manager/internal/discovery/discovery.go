// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package discovery tracks the set of live CubeProxy replicas. It reads the
// registration Hash + heartbeat Sorted Set that each CubeProxy publishes into
// Redis, and exposes a Snapshot() to the rest of cube-lifecycle-manager.
//
// Redis schema (owner: CubeProxy init_worker timer):
//
//	HSET cube:v1:shared:cube_proxy:registry <proxy_id> <JSON Endpoint>
//	ZADD cube:v1:shared:cube_proxy:heartbeat <unix_ms> <proxy_id>
//
// A CubeProxy is considered live when its heartbeat score is within the
// configured TTL. Expired members are pruned by the same refresh loop so
// stale rows don't leak.
package discovery

import (
	"encoding/json"
	"sync"
	"time"
)

// Redis key names. Kept as package-level vars so tests can point them at a
// scratch namespace if needed.
var (
	RegistryKey  = "cube:v1:shared:cube_proxy:registry"
	HeartbeatKey = "cube:v1:shared:cube_proxy:heartbeat"
)

// Endpoint is one live CubeProxy replica. The JSON tags mirror what
// CubeProxy's init_worker timer writes into the registry Hash.
type Endpoint struct {
	ProxyID   string `json:"proxy_id"`
	AdminURL  string `json:"admin_url"`  // e.g. http://10.0.0.2:8082
	ResumeURL string `json:"resume_url"` // reserved; today == AdminURL host + :8083 not used
	NodeIP    string `json:"node_ip,omitempty"`
	StartedAt int64  `json:"started_at,omitempty"` // unix ms
	Version   string `json:"version,omitempty"`
}

// Fleet is the read-only view exposed to consumers (proxypush, main).
// Every call returns a fresh slice so callers may sort / filter safely.
type Fleet interface {
	Snapshot() []Endpoint
}

// Static is a Fleet backed by a fixed list, used for tests and for the
// CUBE_LCM_PROXY_ADMIN_URLS override path (single-host dev).
type Static struct {
	endpoints []Endpoint
}

// NewStatic returns a Fleet built from raw admin URLs. Every URL becomes an
// Endpoint whose ProxyID equals the URL (uniqueness is by construction).
func NewStatic(adminURLs []string) *Static {
	eps := make([]Endpoint, 0, len(adminURLs))
	for _, u := range adminURLs {
		eps = append(eps, Endpoint{ProxyID: u, AdminURL: u})
	}
	return &Static{endpoints: eps}
}

// Snapshot returns a defensive copy.
func (s *Static) Snapshot() []Endpoint {
	out := make([]Endpoint, len(s.endpoints))
	copy(out, s.endpoints)
	return out
}

// decodeEndpoint parses a registry-Hash value. Unrecognized JSON is returned
// as (Endpoint{}, err); the caller decides whether to skip or log.
func decodeEndpoint(raw string) (Endpoint, error) {
	var ep Endpoint
	if err := json.Unmarshal([]byte(raw), &ep); err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

// live wraps an Endpoint with the timestamp we saw it at, used internally by
// RedisDiscovery to decide join/leave transitions.
type live struct {
	ep       Endpoint
	lastSeen time.Time
}

// clone helps write concurrency-safe Snapshot() implementations.
func cloneEndpoints(src map[string]*live) []Endpoint {
	out := make([]Endpoint, 0, len(src))
	for _, l := range src {
		out = append(out, l.ep)
	}
	return out
}

// diffJoins compares "previous" and "current" ProxyID sets, returning the IDs
// that appear only in "current" (joins) and only in "previous" (leaves).
func diffJoins(prev, cur map[string]*live) (joins, leaves []string) {
	for id := range cur {
		if _, ok := prev[id]; !ok {
			joins = append(joins, id)
		}
	}
	for id := range prev {
		if _, ok := cur[id]; !ok {
			leaves = append(leaves, id)
		}
	}
	return
}

// snapshotMap is a copy-on-write helper used by RedisDiscovery.Snapshot; it
// lives here so tests can exercise the cloning path without touching Redis.
func snapshotMap(m map[string]*live, mu *sync.RWMutex) []Endpoint {
	mu.RLock()
	defer mu.RUnlock()
	return cloneEndpoints(m)
}
