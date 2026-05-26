// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"sort"
	"strings"
	"time"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

type SnapshotStorageStatus struct {
	NodeID        string `json:"node_id,omitempty"`
	NodeIP        string `json:"node_ip,omitempty"`
	UsagePct      uint64 `json:"usage_pct,omitempty"`
	Mode          string `json:"mode,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUpdatedAt int64  `json:"last_updated_at,omitempty"`
}

func ListSnapshotStorageStatus(ctx context.Context, refresh bool) ([]SnapshotStorageStatus, error) {
	nodes := localcache.GetHealthyNodesByInstanceType(-1, cubeboxv1.InstanceType_cubebox.String())
	out := make([]SnapshotStorageStatus, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	var listErr error

	for i := range nodes {
		nodeID := strings.TrimSpace(nodes[i].ID())
		nodeIP := strings.TrimSpace(nodes[i].HostIP())
		key := firstNonEmpty(nodeID, nodeIP)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		state, err := snapshotStorageStateForView(ctx, nodeID, nodeIP, refresh)
		if err != nil && listErr == nil {
			listErr = err
		}
		out = append(out, snapshotStorageStatusFromState(nodeID, nodeIP, state))
	}

	sort.Slice(out, func(i, j int) bool {
		left := firstNonEmpty(out[i].NodeID, out[i].NodeIP)
		right := firstNonEmpty(out[j].NodeID, out[j].NodeIP)
		return left < right
	})
	return out, listErr
}

func snapshotStorageStateForView(ctx context.Context, nodeID, nodeIP string, refresh bool) (snapshotStorageState, error) {
	if refresh {
		return refreshSingleSnapshotStorageState(ctx, nodeID, nodeIP)
	}
	if cached, ok := localcache.GetSnapshotStorageState(nodeID, nodeIP); ok {
		// Match getOrRefreshSnapshotStorageState: unknown mode bypasses TTL,
		// triggering a synchronous refresh so cold-start failures self-heal quickly.
		cachedMode := snapshotStorageMode(cached.Mode)
		if cachedMode != snapshotStorageModeUnknown && cached.LastUpdatedAt > 0 &&
			time.Since(time.Unix(cached.LastUpdatedAt, 0)) <= snapshotMetricsTTL {
			return snapshotStorageState{
				NodeID:        firstNonEmpty(cached.NodeID, nodeID),
				NodeIP:        firstNonEmpty(cached.NodeIP, nodeIP),
				UsagePct:      cached.UsagePct,
				Mode:          cachedMode,
				LastError:     cached.LastError,
				LastUpdatedAt: time.Unix(cached.LastUpdatedAt, 0),
			}, nil
		}
	}
	return refreshSingleSnapshotStorageState(ctx, nodeID, nodeIP)
}

func snapshotStorageStatusFromState(nodeID, nodeIP string, state snapshotStorageState) SnapshotStorageStatus {
	return SnapshotStorageStatus{
		NodeID:        firstNonEmpty(state.NodeID, nodeID),
		NodeIP:        firstNonEmpty(state.NodeIP, nodeIP),
		UsagePct:      state.UsagePct,
		Mode:          string(state.Mode),
		LastError:     state.LastError,
		LastUpdatedAt: state.LastUpdatedAt.Unix(),
	}
}
