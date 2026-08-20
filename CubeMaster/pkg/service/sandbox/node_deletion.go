// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"fmt"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
)

var ErrNodeHasSandboxes = errors.New("node still has sandboxes")

// DeleteNode verifies the node is empty through Cubelet before removing its
// control-plane metadata. force bypasses the inventory check but never the
// nodemeta isolation requirement.
func DeleteNode(ctx context.Context, nodeID string, force bool) error {
	if force {
		log.G(ctx).Warnf("force deleting node without sandbox inventory verification node_id=%s", nodeID)
		return nodemeta.DeleteNode(ctx, nodeID)
	}
	// Known race: the inventory check and metadata deletion are not atomic with
	// sandbox creation. A create that already passed the final scheduling
	// admission, or one admitted by another CubeMaster that has not observed the
	// isolation yet, may still reach Cubelet after this check. LocalCreateNum is
	// also not consulted here. The current operational contract mitigates this by
	// requiring operators to isolate the node, wait for in-flight creates and
	// replica propagation, and remove all sandboxes before deletion. We currently
	// accept this residual window instead of adding a cross-replica create lease
	// or a lifecycle lock shared with the create path.
	count, err := countNodeSandboxes(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("verify node sandboxes: %w; restore Cubelet connectivity and retry, or retry with force=true", err)
	}
	if count != 0 {
		return fmt.Errorf("%w: %d found; remove them first and retry, or use --force (CLI) to bypass the inventory check", ErrNodeHasSandboxes, count)
	}
	return nodemeta.DeleteNode(ctx, nodeID)
}

// countNodeSandboxes asks the target Cubelet directly. Any inventory error is
// returned so normal deletion fails closed; only an explicit force request may
// bypass the check.
func countNodeSandboxes(ctx context.Context, nodeID string) (int, error) {
	n, ok := localcache.GetNode(nodeID)
	if !ok || n == nil {
		if _, err := nodemeta.GetNode(ctx, nodeID); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("node %s exists in metadata but is not present in the scheduler cache", nodeID)
	}
	endpoint := cubelet.GetCubeletAddr(n.HostIP())
	unlock := l.CubeletListLock.Lock(endpoint)
	defer unlock()
	rsp, err := cubelet.List(ctx, endpoint, &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			LabelSelector: map[string]string{"io.kubernetes.cri.container-type": "sandbox"},
		},
	})
	if err != nil {
		return 0, err
	}
	return len(rsp.GetItems()), nil
}
