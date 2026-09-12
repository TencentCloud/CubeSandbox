// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Reservation management for the scheduler.
//
// Between "the scheduler picked a node" and "the Cubelet actually created
// the sandbox and its metrics flowed back", the node cache still shows the
// old allocation, so concurrent creates keep landing on the same node
// (herding). This file closes that window: after selection, CubeMaster
// re-reads the node under a lock, re-checks the resource predicates with
// already-reserved amounts added, and records a reservation covering CPU
// quota, memory quota, one MVM slot, and one create-concurrency slot. The
// caller is expected to re-schedule (bounded) on ErrNodeReservationConflict
// and to Release the reservation once the Cubelet create returns — on
// success the next Cubelet metric report takes the accounting over.
//
// Single-CubeMaster deployments are fully covered by the in-process
// registry. With multiple CubeMaster replicas the reservation is also
// accumulated in a per-node Redis Hash (atomic check-and-increment via
// Lua), so replicas see each other's in-flight pressure. When Redis is not
// configured or errors, the code degrades to local-only reservations and
// relies on the realtime_create_num guard as the safety net, matching the
// pre-reservation behavior.
package localcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

// ErrNodeReservationConflict means the selected node cannot take the request
// right now once in-flight reservations are accounted for. It is a normal,
// retryable scheduling outcome: the caller should mark the node bad for this
// attempt and re-schedule.
var ErrNodeReservationConflict = errors.New("node reservation conflict")

// NodeReservation is one held reservation. Release is idempotent and safe to
// call from any goroutine.
type NodeReservation struct {
	NodeID   string
	CpuMilli int64
	MemMB    int64

	redisBacked bool
	released    atomic.Bool
}

// nodeReservationAmount is the per-node aggregate of what one CubeMaster
// replica has reserved but not yet released.
type nodeReservationAmount struct {
	cpuMilli int64
	memMB    int64
	mvm      int64
	creating int64
}

func (a *nodeReservationAmount) empty() bool {
	return a.cpuMilli == 0 && a.memMB == 0 && a.mvm == 0 && a.creating == 0
}

// reservationRegistry is the authoritative local accounting. node.ReservedNum
// is only a best-effort visibility mirror for snapshots (CEL / gRPC plugins).
var reservationRegistry = struct {
	sync.Mutex
	nodes map[string]*nodeReservationAmount
}{nodes: make(map[string]*nodeReservationAmount)}

const (
	// reservationAcquireScript atomically adds the request to the per-node
	// reservation Hash and rolls back if any dimension exceeds the headroom
	// computed by the caller from its freshest node view:
	//
	//	KEYS[1] = reservation hash key
	//	ARGV[1] = safety TTL (seconds)
	//	ARGV[2..5] = deltas: cpu_milli, mem_mb, mvm, creating
	//	ARGV[6..9] = maxima: cpu_milli, mem_mb, mvm, creating
	//
	// Returns 1 on success, 0 on conflict.
	reservationAcquireScript = `
local cpu = redis.call('HINCRBY', KEYS[1], 'cpu_milli', ARGV[2])
local mem = redis.call('HINCRBY', KEYS[1], 'mem_mb', ARGV[3])
local mvm = redis.call('HINCRBY', KEYS[1], 'mvm', ARGV[4])
local creating = redis.call('HINCRBY', KEYS[1], 'creating', ARGV[5])
redis.call('EXPIRE', KEYS[1], ARGV[1])
if cpu > tonumber(ARGV[6]) or mem > tonumber(ARGV[7]) or mvm > tonumber(ARGV[8]) or creating > tonumber(ARGV[9]) then
  redis.call('HINCRBY', KEYS[1], 'cpu_milli', -1 * tonumber(ARGV[2]))
  redis.call('HINCRBY', KEYS[1], 'mem_mb', -1 * tonumber(ARGV[3]))
  redis.call('HINCRBY', KEYS[1], 'mvm', -1 * tonumber(ARGV[4]))
  redis.call('HINCRBY', KEYS[1], 'creating', -1 * tonumber(ARGV[5]))
  return 0
end
return 1
`
	// reservationReleaseScript subtracts the reservation again, clamping each
	// field at zero so a release after a TTL expiry cannot drive the Hash
	// negative.
	//
	//	KEYS[1] = reservation hash key
	//	ARGV[1] = safety TTL (seconds)
	//	ARGV[2..5] = deltas: cpu_milli, mem_mb, mvm, creating
	reservationReleaseScript = `
local fields = {'cpu_milli', 'mem_mb', 'mvm', 'creating'}
for i, field in ipairs(fields) do
  local v = redis.call('HINCRBY', KEYS[1], field, -1 * tonumber(ARGV[i + 1]))
  if v < 0 then
    redis.call('HSET', KEYS[1], field, 0)
  end
end
redis.call('EXPIRE', KEYS[1], ARGV[1])
return 1
`
)

// reservationTTLSec bounds how long a reservation can leak in Redis if the
// holding CubeMaster crashes: the create timeout plus one minute of slack.
func reservationTTLSec() int64 {
	ttl := int64(config.GetConfig().CubeletConf.CreateTimeoutInsec) + 60
	if ttl <= 0 {
		ttl = 660
	}
	return ttl
}

// TryReserveNode re-reads the node from the cache and, under the registry
// lock, re-checks the CPU / memory quota, MVM limit, and create-concurrency
// predicates with this replica's outstanding reservations already charged.
// On success the reservation is recorded locally and then pushed to Redis
// for cross-replica visibility; a Redis conflict rolls the local record back
// and returns ErrNodeReservationConflict, while a Redis error degrades to a
// local-only reservation (warn-logged) instead of failing the create.
func TryReserveNode(ctx context.Context, nodeID string, cpuMilli, memMB int64) (*NodeReservation, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("%w: empty node id", ErrNodeReservationConflict)
	}
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return nil, errors.New("TryReserveNode: scheduler config is nil")
	}

	reservationRegistry.Lock()
	current, ok := GetNode(nodeID)
	if !ok {
		reservationRegistry.Unlock()
		return nil, fmt.Errorf("%w: node %s missing from cache", ErrNodeReservationConflict, nodeID)
	}
	local := reservationRegistry.nodes[nodeID]
	if local == nil {
		local = &nodeReservationAmount{}
	}
	if err := checkReservationCapacity(&sconf.SchedulerConf, current, local, cpuMilli, memMB); err != nil {
		reservationRegistry.Unlock()
		return nil, err
	}
	local.cpuMilli += cpuMilli
	local.memMB += memMB
	local.mvm++
	local.creating++
	reservationRegistry.nodes[nodeID] = local
	bumpReservedNum(nodeID, 1)
	reservationRegistry.Unlock()

	reservation := &NodeReservation{NodeID: nodeID, CpuMilli: cpuMilli, MemMB: memMB}
	if config.GetConfig().RedisConf == nil {
		// No Redis configured: local-only by design (single-replica
		// deployments never coordinate through Redis).
		return reservation, nil
	}
	acquired, err := redisAcquireReservation(ctx, current, cpuMilli, memMB)
	switch {
	case err != nil:
		// Redis down: keep the local reservation and let the
		// realtime_create_num guard be the backstop, as before.
		log.G(ctx).Warnf("reservation redis acquire degraded to local-only, node=%s err=%v", nodeID, err)
	case !acquired:
		reservation.rollbackLocal()
		return nil, fmt.Errorf("%w: node %s over committed across masters", ErrNodeReservationConflict, nodeID)
	default:
		reservation.redisBacked = true
	}
	return reservation, nil
}

// checkReservationCapacity mirrors the mandatory guard predicates, charging
// this replica's outstanding reservations on top of the last reported usage.
// The comparisons stay one request stricter than the filters (free must
// exceed the request, matching cpufilter/memfilter).
func checkReservationCapacity(sconf *config.SchedulerConf, n *node.Node, local *nodeReservationAmount, cpuMilli, memMB int64) error {
	cpuFree := n.QuotaCpu -
		sconf.EffectiveAllocated(n.QuotaCpuUsage) - local.cpuMilli
	if cpuFree <= cpuMilli {
		return fmt.Errorf("%w: node %s cpu free %d, want > %d", ErrNodeReservationConflict, n.ID(), cpuFree, cpuMilli)
	}
	memFree := n.QuotaMem -
		sconf.EffectiveAllocated(n.QuotaMemUsage) - local.memMB
	if memFree <= memMB {
		return fmt.Errorf("%w: node %s mem free %d, want > %d", ErrNodeReservationConflict, n.ID(), memFree, memMB)
	}
	if mvmLimit := RealMaxMvmLimit(n); n.MvmNum+local.mvm >= mvmLimit {
		return fmt.Errorf("%w: node %s mvm %d reserved %d, limit %d",
			ErrNodeReservationConflict, n.ID(), n.MvmNum, local.mvm, mvmLimit)
	}
	if createLimit := CreateConcurrentLimit(n); n.RealTimeCreateNum+local.creating >= createLimit {
		return fmt.Errorf("%w: node %s creating %d reserved %d, limit %d",
			ErrNodeReservationConflict, n.ID(), n.RealTimeCreateNum, local.creating, createLimit)
	}
	return nil
}

// Release returns the reservation. On success paths this is where the
// accounting hands over to the next Cubelet metric report; on failure or
// cancel paths it simply frees the slots.
func (r *NodeReservation) Release(ctx context.Context) {
	if r == nil || !r.released.CompareAndSwap(false, true) {
		return
	}
	r.rollbackLocal()
	if r.redisBacked {
		if err := redisReleaseReservation(ctx, r.NodeID, r.CpuMilli, r.MemMB); err != nil {
			// The safety TTL reaps whatever is left behind.
			log.G(ctx).Warnf("reservation redis release failed, node=%s err=%v", r.NodeID, err)
		}
	}
}

// rollbackLocal undoes the local record. Caller must not hold the registry
// lock; the function takes it itself.
func (r *NodeReservation) rollbackLocal() {
	reservationRegistry.Lock()
	defer reservationRegistry.Unlock()
	local := reservationRegistry.nodes[r.NodeID]
	if local != nil {
		local.cpuMilli -= r.CpuMilli
		local.memMB -= r.MemMB
		local.mvm--
		local.creating--
		if local.empty() {
			delete(reservationRegistry.nodes, r.NodeID)
		}
	}
	bumpReservedNum(r.NodeID, -1)
}

// bumpReservedNum mirrors the reservation count onto the cached node so
// frozen snapshots expose it as SnapshotNode.reserved. Best-effort: if the
// node was re-registered and the cache entry replaced, the mirror restarts
// from zero while the registry keeps the authoritative counts.
func bumpReservedNum(nodeID string, delta int64) {
	if elem, ok := l.cache.Get(nodeID); ok {
		if cached, ok := elem.(*node.Node); ok && cached != nil {
			cached.ReservedNumIncrBy(delta)
		}
	}
}

// reservationRedisConn is indirected so unit tests can point reservations at
// a miniredis instance without touching the process-wide pool cache.
var reservationRedisConn = wrapredis.GetRedis

// redisAcquireReservation pushes the reservation into the per-node Redis
// Hash. The headroom maxima are computed from the caller's fresh node view so
// the Lua check is atomic across replicas: every replica charges into the
// same Hash and the script rejects whatever would overshoot capacity.
func redisAcquireReservation(ctx context.Context, n *node.Node, cpuMilli, memMB int64) (bool, error) {
	sconf := &config.GetConfig().Scheduler.SchedulerConf
	maxCpu := n.QuotaCpu - sconf.EffectiveAllocated(n.QuotaCpuUsage)
	maxMem := n.QuotaMem - sconf.EffectiveAllocated(n.QuotaMemUsage)
	maxMvm := RealMaxMvmLimit(n) - n.MvmNum
	maxCreating := CreateConcurrentLimit(n) - n.RealTimeCreateNum
	result, err := redis.Int(reservationRedisConn().Do("EVAL", reservationAcquireScript, 1,
		rediskey.NodeReservation(n.ID()), reservationTTLSec(),
		cpuMilli, memMB, 1, 1, maxCpu, maxMem, maxMvm, maxCreating))
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func redisReleaseReservation(ctx context.Context, nodeID string, cpuMilli, memMB int64) error {
	if config.GetConfig().RedisConf == nil {
		return nil
	}
	_, err := reservationRedisConn().Do("EVAL", reservationReleaseScript, 1,
		rediskey.NodeReservation(nodeID), reservationTTLSec(), cpuMilli, memMB, 1, 1)
	return err
}
