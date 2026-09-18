// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

const (
	nodeSnapshotKeyPrefix = "cube:v1:cubeops:node:snapshot:"
	nodeSnapshotTTLSec    = 30
)

func nodeSnapshotKey(nodeID string) string {
	return nodeSnapshotKeyPrefix + nodeID
}

// WriteNodeSnapshot stores the node snapshot as a short-TTL JSON blob. It
// returns false when a newer snapshot already won the Redis ordering check.
func WriteNodeSnapshot(snap *model.NodeSnapshot) bool {
	if snap == nil || snap.NodeID == "" || pool == nil {
		return false
	}
	data, err := json.Marshal(snap)
	if err != nil {
		logging.G(context.Background()).Warnf("nodesnapshot: marshal failed: node=%s: %v", snap.NodeID, err)
		return false
	}
	conn := pool.Get()
	defer conn.Close()
	const setIfNewer = `
local current = redis.call('GET', KEYS[1])
if current then
  local ok, decoded = pcall(cjson.decode, current)
  if ok then
    local current_order = decoded.heartbeat_order_unix_milli or 0
    local incoming_order = tonumber(ARGV[2]) or 0
    if current_order > incoming_order or (incoming_order == 0 and current_order > 0) then
      return 0
    end
  end
end
redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[3])
return 1`
	applied, err := redis.Int(conn.Do("EVAL", setIfNewer, 1, nodeSnapshotKey(snap.NodeID), data, snap.HeartbeatOrderUnixMilli, nodeSnapshotTTLSec))
	if err != nil {
		logging.G(context.Background()).Warnf("nodesnapshot: redis conditional SET failed: node=%s: %v", snap.NodeID, err)
		return false
	}
	return applied == 1
}

// ReadNodeSnapshot fetches a node snapshot from Redis. Returns (nil, nil) on miss.
func ReadNodeSnapshot(nodeID string) (*model.NodeSnapshot, error) {
	if nodeID == "" || pool == nil {
		return nil, nil
	}
	conn := pool.Get()
	defer conn.Close()
	data, err := redis.Bytes(conn.Do("GET", nodeSnapshotKey(nodeID)))
	if err == redis.ErrNil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nodesnapshot: redis GET: %w", err)
	}
	var snap model.NodeSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("nodesnapshot: unmarshal: %w", err)
	}
	return &snap, nil
}

// scanNodeSnapshotsHook is set by tests to intercept Redis snapshot scans.
var scanNodeSnapshotsHook func() ([]*model.NodeSnapshot, error)

// SetScanNodeSnapshotsHook registers a test hook; cleanup restores the prior one.
func SetScanNodeSnapshotsHook(hook func() ([]*model.NodeSnapshot, error)) func() {
	prev := scanNodeSnapshotsHook
	scanNodeSnapshotsHook = hook
	return func() { scanNodeSnapshotsHook = prev }
}

// ScanNodeSnapshots returns all node snapshots in Redis, iterating SCAN to completion.
func ScanNodeSnapshots() ([]*model.NodeSnapshot, error) {
	if scanNodeSnapshotsHook != nil {
		return scanNodeSnapshotsHook()
	}
	if pool == nil {
		return nil, nil
	}
	conn := pool.Get()
	defer conn.Close()

	var keys []string
	cursor := 0
	for {
		// SCAN replies [cursor, [key ...]]; unpack the outer tuple first.
		reply, err := redis.Values(conn.Do("SCAN", cursor, "MATCH", nodeSnapshotKeyPrefix+"*", "COUNT", 200))
		if err != nil {
			return nil, fmt.Errorf("nodesnapshot: redis SCAN: %w", err)
		}
		if len(reply) != 2 {
			return nil, fmt.Errorf("nodesnapshot: redis SCAN: unexpected reply shape: %v", reply)
		}
		next, err := redis.Int(reply[0], nil)
		if err != nil {
			return nil, fmt.Errorf("nodesnapshot: redis SCAN: parse cursor: %w", err)
		}
		keysInBatch, err := redis.Strings(reply[1], nil)
		if err != nil {
			return nil, fmt.Errorf("nodesnapshot: redis SCAN: parse keys: %w", err)
		}
		keys = append(keys, keysInBatch...)
		if next == 0 {
			break
		}
		cursor = next
	}

	if len(keys) == 0 {
		return nil, nil
	}
	values, err := redis.ByteSlices(conn.Do("MGET", redis.Args{}.AddFlat(keys)...))
	if err != nil {
		return nil, fmt.Errorf("nodesnapshot: redis MGET: %w", err)
	}
	out := make([]*model.NodeSnapshot, 0, len(values))
	for _, data := range values {
		if len(data) == 0 {
			continue
		}
		var snap model.NodeSnapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			continue
		}
		out = append(out, &snap)
	}
	return out, nil
}

// DeleteNodeSnapshot removes a node snapshot from Redis.
func DeleteNodeSnapshot(nodeID string) {
	if nodeID == "" || pool == nil {
		return
	}
	conn := pool.Get()
	defer conn.Close()
	if _, err := conn.Do("DEL", nodeSnapshotKey(nodeID)); err != nil {
		logging.G(context.Background()).Warnf("nodesnapshot: redis DEL failed: node=%s: %v", nodeID, err)
	}
}

// SnapshotKeyForTest exposes the key prefix for tests.
func SnapshotKeyForTest(nodeID string) string {
	return nodeSnapshotKey(nodeID)
}
