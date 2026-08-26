// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

// ComponentVersion mirrors the cubelet-side masterclient.ComponentVersion.
// It carries the real version of one component installed on a node. Source is
// one of "manifest" | "binary" | "file" | "component-json".
// Alias of base/node.ComponentVersion so data flows CubeOps → localcache →
// templatecenter without type conversion.
type ComponentVersion = node.ComponentVersion

// declaredVersions maps component names to their release-declared versions,
// loaded once at startup from the on-disk manifest.
var (
	declaredVersionsMu sync.RWMutex
	declaredVersions   = map[string]string{}
)

// Init loads declared component versions and registers the CubeOps node
// loader into localcache.
func Init(ctx context.Context) error {
	_ = ctx
	info := loadDeclaredVersionInfo()
	declaredVersionsMu.Lock()
	declaredVersions = info.Primary
	declaredVersionsMu.Unlock()

	if addr := config.GetConfig().Common.CubeOpsAddr; addr != "" {
		common := config.GetConfig().Common
		log.G(ctx).Infof("nodemeta using CubeOps node loader addr=%s", addr)
		loader := NewCubeOpsLoader(addr).
			WithBootRetry(common.CubeOpsBootRetries, common.CubeOpsBootBackoff)
		// Fail fast: CubeOps is the authoritative node source. An unreachable
		// CubeOps at boot would serve an empty node list (no scheduling candidates).
		if _, err := loader.LoadNodes(ctx); err != nil {
			return fmt.Errorf("CubeOps unreachable at boot (addr=%s): %w; start CubeOps first, then CubeMaster", addr, err)
		}
		localcache.RegisterNodeLoader(cubeopsNodeLoader(loader))
	} else {
		log.G(ctx).Warnf("nodemeta: cube_ops_addr not configured; localcache will fall back to its own DB loader")
	}

	return nil
}

// DeclaredVersions returns a snapshot of the release-declared component versions.
func DeclaredVersions() map[string]string {
	declaredVersionsMu.RLock()
	defer declaredVersionsMu.RUnlock()
	out := make(map[string]string, len(declaredVersions))
	for k, v := range declaredVersions {
		out[k] = v
	}
	return out
}

func compatRelevantVersions(versions []ComponentVersion) map[string]string {
	out := map[string]string{
		"guest-image": "",
		"cube-agent":  "",
	}
	for _, v := range versions {
		switch v.Component {
		case "guest-image", "cube-agent":
			out[v.Component] = strings.TrimSpace(v.Version)
		}
	}
	return out
}

// GetNodeComponentVersions returns guest-image / cube-agent versions for a
// healthy node. Returns false when the node is unknown or unhealthy.
func GetNodeComponentVersions(ctx context.Context, nodeID string) (map[string]string, bool) {
	_ = ctx
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, false
	}
	n, ok := localcache.GetNode(nodeID)
	if !ok || n == nil {
		return nil, false
	}
	if !n.Healthy {
		return nil, false
	}
	return compatRelevantVersions(n.Versions), true
}

// HostFacts is an alias of base/node.HostFacts so data flows CubeOps →
// localcache → templatecenter without type conversion.
type HostFacts = node.HostFacts

// RestoreMatchFactsJSON freezes only the two fields used for cross-node
// restore matching: cpuid_hash and host_kernel_release. Vendor/model/KVM
// extras stay on the live node heartbeat and are not written to snapshot
// or pause-snapshot rows.
func RestoreMatchFactsJSON(facts *HostFacts) string {
	if facts == nil {
		return ""
	}
	slim := struct {
		CPUIDHash         string `json:"cpuid_hash,omitempty"`
		HostKernelRelease string `json:"host_kernel_release,omitempty"`
	}{
		CPUIDHash:         strings.TrimSpace(facts.CPUIDHash),
		HostKernelRelease: strings.TrimSpace(facts.HostKernelRelease),
	}
	if slim.CPUIDHash == "" && slim.HostKernelRelease == "" {
		return ""
	}
	raw, err := json.Marshal(slim)
	if err != nil {
		return ""
	}
	return string(raw)
}

// CandidateNode is a node that matches a HostFacts query.
type CandidateNode struct {
	NodeID    string
	HostIP    string
	HostFacts *HostFacts
}

// GetNodeHostFacts returns the trusted host facts for a healthy node.
// Returns false when the node is unknown, unhealthy, or no facts were reported.
func GetNodeHostFacts(ctx context.Context, nodeID string) (*HostFacts, bool) {
	_ = ctx
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, false
	}
	n, ok := localcache.GetNode(nodeID)
	if !ok || n == nil {
		return nil, false
	}
	if !n.Healthy {
		return nil, false
	}
	if n.HostFacts == nil || n.HostFacts.IsZero() {
		return nil, false
	}
	return n.HostFacts, true
}

// GetPersistedNodeHostFacts returns the host facts for a node ignoring live
// health. Since CubeOps already persists facts and syncs to localcache, this
// is the same as GetNodeHostFacts but without the healthy gate.
func GetPersistedNodeHostFacts(ctx context.Context, nodeID string) (*HostFacts, bool) {
	_ = ctx
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, false
	}
	n, ok := localcache.GetNode(nodeID)
	if !ok || n == nil {
		return nil, false
	}
	if n.HostFacts == nil || n.HostFacts.IsZero() {
		return nil, false
	}
	return n.HostFacts, true
}

// QueryHostFactCandidates returns all healthy nodes with host facts, optionally
// filtered by CPUID hash and kernel release.
func QueryHostFactCandidates(ctx context.Context, requiredCPUIDHash, requiredKernelRelease string, matchAll bool) ([]*CandidateNode, error) {
	_ = ctx
	return filterHostFactCandidates(localcache.GetHealthyNodes(-1), requiredCPUIDHash, requiredKernelRelease, matchAll), nil
}

// filterHostFactCandidates applies the HostFacts filter to a node list.
// Extracted from QueryHostFactCandidates for unit testing without localcache.
func filterHostFactCandidates(nodes node.NodeList, requiredCPUIDHash, requiredKernelRelease string, matchAll bool) []*CandidateNode {
	out := make([]*CandidateNode, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || !n.Healthy {
			continue
		}
		if n.HostFacts == nil || n.HostFacts.IsZero() {
			continue
		}
		if !matchAll {
			if n.HostFacts.CPUIDHash != requiredCPUIDHash {
				continue
			}
			if n.HostFacts.HostKernelRelease != requiredKernelRelease {
				continue
			}
		}
		out = append(out, &CandidateNode{
			NodeID:    n.InsID,
			HostIP:    n.IP,
			HostFacts: n.HostFacts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}
