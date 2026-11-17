// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// templateLocalityFilter keeps template-bound requests on nodes that have
// cached the template image and are within the allowed scheduling scope.
type templateLocalityFilter struct{}

func NewTemplateLocalityFilter() *templateLocalityFilter {
	return &templateLocalityFilter{}
}

func (l *templateLocalityFilter) ID() string {
	return constants.SelectorFilterID + "/" + "template_locality"
}

func (l *templateLocalityFilter) String() string {
	return l.ID()
}

// Select only takes effect when the request carries a TemplateID (or a
// template scheduling scope is configured).
func (l *templateLocalityFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	inList := selCtx.Nodes()
	reqRes := selCtx.GetReqRes()
	if reqRes == nil {
		return inList, nil
	}
	if reqRes.TemplateID == "" && len(reqRes.TemplateNodeScope) == 0 {
		return inList, nil
	}

	nodes := make(node.NodeList, 0, inList.Len())
	rejects := newRejectLog(selCtx)
	for i := range inList {
		if !templateNodeAllowed(reqRes, inList[i]) {
			rejects.warnf("%v select:%v template=%s not in template scope", l.ID(), inList[i].ID(), reqRes.TemplateID)
			continue
		}
		if reqRes.TemplateID != "" && !reqRes.AllowNonLocalTemplate {
			facts, frozen := selCtx.SnapshotFacts(inList[i].ID())
			templateLocal := false
			if frozen && facts.TemplateLocalKnown {
				templateLocal = facts.TemplateLocal
			} else {
				templateLocal = localcache.GetImageStateByNode(reqRes.TemplateID, inList[i].ID()) != nil
			}
			if !templateLocal {
				rejects.warnf("%v select:%v template=%s not local", l.ID(), inList[i].ID(), reqRes.TemplateID)
				continue
			}
			if reqRes.EnforceSnapshotStorage {
				storageAllowed := false
				if frozen && facts.SnapshotStorageKnown {
					storageAllowed = facts.SnapshotStorageAllowed
				} else {
					storageAllowed = snapshotStorageNodeAllowed(inList[i])
				}
				if !storageAllowed {
					rejects.warnf("%v select:%v template=%s snapshot storage unavailable", l.ID(), inList[i].ID(), reqRes.TemplateID)
					continue
				}
			}
		}
		nodes.Append(inList[i])
	}
	rejects.flush(l.ID())
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	}
	return nodes, nil
}

// templateNodeAllowed reports whether the node is within the template's
// scheduling scope; all nodes are allowed when no scope is configured.
func templateNodeAllowed(reqRes *selctx.RequestResource, n *node.Node) bool {
	if reqRes == nil || len(reqRes.TemplateNodeScope) == 0 || n == nil {
		return true
	}
	for _, item := range reqRes.TemplateNodeScope {
		scope := strings.TrimSpace(item)
		if scope == "" {
			continue
		}
		if scope == n.ID() || scope == n.HostIP() {
			return true
		}
	}
	return false
}

func snapshotStorageNodeAllowed(n *node.Node) bool {
	if n == nil {
		return false
	}
	state, ok := localcache.GetSnapshotStorageState(n.ID(), n.HostIP())
	if !ok {
		return false
	}
	return localcache.IsSnapshotStorageWriteAllowed(state.Mode)
}
