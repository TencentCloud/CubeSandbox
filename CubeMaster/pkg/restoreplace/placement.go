// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package restoreplace implements the CubeMaster §3.1 restore placement tree
// shared by Pause Resume and from-snapshot Create: origin node first; leave
// the origin only when staying would fail and the snapshot is S3
// remote_ready. Cross-node candidates must match the origin cpuid_hash and
// host_kernel_release.
package restoreplace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// Input is the snapshot／pause-snap identity needed to run §3.1.
type Input struct {
	SnapshotID          string
	Backend             string
	RemoteStatus        string
	OriginNodeID        string
	OriginNodeIP        string
	OriginHostFactsJSON string
	InstanceType        string
	ReqRes              *selctx.RequestResource
	RequestedScope      []string
	// PinToOrigin forbids cross-node restore even when the snapshot is S3
	// remote_ready (e.g. host-mount is bound to the origin host path).
	PinToOrigin bool
}

// Placement is the chosen host. CrossNode is true only after origin failed
// and a compatible peer passed scheduler.Select.
type Placement struct {
	NodeID    string
	NodeIP    string
	CrossNode bool
}

type compatibleCandidate struct {
	NodeID string
	NodeIP string
}

var (
	lookupNodeFn              = defaultLookupNode
	selectNodeFn              = defaultSelectNode
	listCompatibleFn          = defaultListCompatible
	queryHostFactCandidatesFn = nodemeta.QueryHostFactCandidates
)

func crossNodeBlockedReason(in Input) string {
	if in.PinToOrigin {
		return "host-mount"
	}
	if !CanCrossNode(in.Backend, in.RemoteStatus) {
		return fmt.Sprintf("backend=%s remote_status=%s",
			strings.TrimSpace(in.Backend), strings.TrimSpace(in.RemoteStatus))
	}
	return ""
}

// CanCrossNode is §3.1 steps 1–3: only S3 snapshots whose sync finished
// (remote_status=ready) may leave the origin.
func CanCrossNode(backend, remoteStatus string) bool {
	if !constants.IsS3Backend(backend) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(remoteStatus), constants.RemoteStatusReady)
}

// Decide runs the §3.1 tree. Origin is returned whenever it can still take
// the job (including when cross-node is allowed). Cross-node Select runs only
// when origin cannot, PinToOrigin is false, and CanCrossNode is true.
func Decide(ctx context.Context, in Input) (*Placement, error) {
	origin := lookupNodeFn(in.OriginNodeID, in.OriginNodeIP)
	if origin != nil && nodeInRequestedScope(origin, in.RequestedScope) && originSchedulable(ctx, origin, in) {
		return placementFromNode(origin, false), nil
	}

	originDesc := strings.TrimSpace(in.OriginNodeID)
	if originDesc == "" {
		originDesc = strings.TrimSpace(in.OriginNodeIP)
	}
	if originDesc == "" {
		originDesc = "unknown"
	}

	if reason := crossNodeBlockedReason(in); reason != "" {
		if origin == nil {
			return nil, fmt.Errorf("origin node unavailable for %s and snapshot cannot restore cross-node (%s)",
				strings.TrimSpace(in.SnapshotID), reason)
		}
		return nil, fmt.Errorf("origin node %s cannot schedule %s and snapshot cannot restore cross-node (%s)",
			originDesc, strings.TrimSpace(in.SnapshotID), reason)
	}

	facts := parseOriginHostFacts(in.OriginHostFactsJSON)
	if facts == nil || strings.TrimSpace(facts.CPUIDHash) == "" || strings.TrimSpace(facts.HostKernelRelease) == "" {
		return nil, fmt.Errorf("cannot cross-node restore %s: origin cpuid_hash/host_kernel_release unknown",
			strings.TrimSpace(in.SnapshotID))
	}

	candidates, err := listCompatibleFn(ctx, facts)
	if err != nil {
		return nil, fmt.Errorf("list compatible nodes for %s: %w", strings.TrimSpace(in.SnapshotID), err)
	}

	excludeID := ""
	excludeIP := ""
	if origin != nil {
		excludeID = origin.ID()
		excludeIP = origin.HostIP()
	} else {
		excludeID = strings.TrimSpace(in.OriginNodeID)
		excludeIP = strings.TrimSpace(in.OriginNodeIP)
	}

	scope := make([]string, 0, len(candidates)*2)
	seen := map[string]struct{}{}
	for _, c := range candidates {
		if c.NodeID == "" && c.NodeIP == "" {
			continue
		}
		if sameNode(c.NodeID, c.NodeIP, excludeID, excludeIP) {
			continue
		}
		if !scopeAllows(c.NodeID, c.NodeIP, in.RequestedScope) {
			continue
		}
		for _, key := range []string{c.NodeID, c.NodeIP} {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			scope = append(scope, key)
		}
	}
	if len(scope) == 0 {
		return nil, fmt.Errorf("no compatible node for cross-node restore of %s (cpuid_hash/kernel match)",
			strings.TrimSpace(in.SnapshotID))
	}

	selected, err := selectNodeFn(ctx, in.InstanceType, cloneReqResForCrossNode(in.ReqRes, scope), scope)
	if err != nil {
		return nil, fmt.Errorf("cross-node schedule failed for %s: %w", strings.TrimSpace(in.SnapshotID), err)
	}
	if selected == nil {
		return nil, fmt.Errorf("cross-node schedule failed for %s: no node selected", strings.TrimSpace(in.SnapshotID))
	}
	if !scopeAllows(selected.ID(), selected.HostIP(), scope) {
		return nil, fmt.Errorf("cross-node schedule picked %s/%s outside compatible scope for %s",
			selected.ID(), selected.HostIP(), strings.TrimSpace(in.SnapshotID))
	}
	log.G(ctx).Infof("restoreplace: cross-node snapshot=%s origin=%s target=%s/%s",
		strings.TrimSpace(in.SnapshotID), originDesc, selected.ID(), selected.HostIP())
	return placementFromNode(selected, true), nil
}

func originSchedulable(ctx context.Context, origin *node.Node, in Input) bool {
	if origin == nil {
		return false
	}
	if !origin.Healthy || !origin.SchedulingAllowed() {
		return false
	}
	scope := []string{origin.ID()}
	if ip := strings.TrimSpace(origin.HostIP()); ip != "" {
		scope = append(scope, ip)
	}
	selected, err := selectNodeFn(ctx, in.InstanceType, cloneReqResForOrigin(in.ReqRes, scope), scope)
	if err != nil || selected == nil {
		return false
	}
	return selected.ID() == origin.ID() || (origin.HostIP() != "" && selected.HostIP() == origin.HostIP())
}

func cloneReqResForOrigin(src *selctx.RequestResource, scope []string) *selctx.RequestResource {
	out := emptyReqRes()
	if src != nil {
		copied := *src
		out = &copied
	}
	// Origin health is load／cordon／cpu／mem — not locality cache presence.
	out.TemplateID = ""
	out.AllowNonLocalTemplate = true
	out.EnforceSnapshotStorage = false
	out.TemplateNodeScope = append([]string(nil), scope...)
	return out
}

func cloneReqResForCrossNode(src *selctx.RequestResource, scope []string) *selctx.RequestResource {
	out := emptyReqRes()
	if src != nil {
		copied := *src
		out = &copied
	}
	out.TemplateID = ""
	out.AllowNonLocalTemplate = true
	out.EnforceSnapshotStorage = false
	out.TemplateNodeScope = append([]string(nil), scope...)
	return out
}

func emptyReqRes() *selctx.RequestResource {
	return &selctx.RequestResource{}
}

func placementFromNode(n *node.Node, cross bool) *Placement {
	if n == nil {
		return &Placement{CrossNode: cross}
	}
	return &Placement{NodeID: n.ID(), NodeIP: n.HostIP(), CrossNode: cross}
}

func defaultLookupNode(nodeID, nodeIP string) *node.Node {
	nodeID = strings.TrimSpace(nodeID)
	nodeIP = strings.TrimSpace(nodeIP)
	if nodeID != "" {
		if n, ok := localcache.GetNode(nodeID); ok && n != nil {
			return n
		}
	}
	if nodeIP != "" {
		if n, ok := localcache.GetNodesByIp(nodeIP); ok && n != nil {
			return n
		}
	}
	return nil
}

func defaultSelectNode(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, _ []string) (*node.Node, error) {
	name := "random"
	if cfg := config.GetConfig(); cfg != nil && cfg.Scheduler != nil {
		name = cfg.Scheduler.LeastSelectName
	}
	sel := selctx.New(name)
	sel.Ctx = ctx
	sel.InstanceType = instanceType
	if reqRes == nil {
		reqRes = emptyReqRes()
	}
	sel.ReqRes = reqRes
	if v := constants.GetNodeSelector(ctx); v != nil {
		if ns, ok := v.(affinity.NodeSelector); ok {
			sel.Affinity.NodeSelector = ns
		}
	}
	if v := constants.GetPreferredSchedulingTerms(ctx); v != nil {
		if np, ok := v.(affinity.PreferredSchedulingTerms); ok {
			sel.Affinity.NodePrefererd = np
		}
	}
	return scheduler.Select(sel)
}

func defaultListCompatible(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
	if origin == nil {
		return nil, nil
	}
	rows, err := queryHostFactCandidatesFn(ctx, origin.CPUIDHash, origin.HostKernelRelease, false)
	if err != nil {
		return nil, err
	}
	out := make([]compatibleCandidate, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.HostFacts == nil {
			continue
		}
		out = append(out, compatibleCandidate{NodeID: row.NodeID, NodeIP: row.HostIP})
	}
	return out, nil
}

func parseOriginHostFacts(raw string) *nodemeta.HostFacts {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var facts nodemeta.HostFacts
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return nil
	}
	if strings.TrimSpace(facts.CPUIDHash) == "" && strings.TrimSpace(facts.HostKernelRelease) == "" {
		return nil
	}
	return &facts
}

func nodeInRequestedScope(n *node.Node, scope []string) bool {
	if n == nil {
		return false
	}
	return scopeAllows(n.ID(), n.HostIP(), scope)
}

func scopeAllows(nodeID, nodeIP string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, item := range scope {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == strings.TrimSpace(nodeID) || item == strings.TrimSpace(nodeIP) {
			return true
		}
	}
	return false
}

func sameNode(idA, ipA, idB, ipB string) bool {
	idA, ipA, idB, ipB = strings.TrimSpace(idA), strings.TrimSpace(ipA), strings.TrimSpace(idB), strings.TrimSpace(ipB)
	if idA != "" && idB != "" && idA == idB {
		return true
	}
	if ipA != "" && ipB != "" && ipA == ipB {
		return true
	}
	return false
}
