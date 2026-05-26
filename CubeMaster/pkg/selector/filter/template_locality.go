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

func (l *templateLocalityFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	inList := selCtx.Nodes()
	reqRes := selCtx.GetReqRes()
	if reqRes == nil || reqRes.TemplateID == "" {
		return inList, nil
	}

	nodes := make(node.NodeList, 0, inList.Len())
	for i := range inList {
		if !templateNodeAllowed(reqRes, inList[i]) {
			log.G(selCtx.Ctx).Warnf("%v select:%v template=%s not in template scope", l.ID(), inList[i].ID(), reqRes.TemplateID)
			continue
		}
		if localcache.GetImageStateByNode(reqRes.TemplateID, inList[i].ID()) == nil {
			log.G(selCtx.Ctx).Warnf("%v select:%v template=%s not local", l.ID(), inList[i].ID(), reqRes.TemplateID)
			continue
		}
		if reqRes.EnforceSnapshotStorage && !snapshotStorageNodeAllowed(inList[i]) {
			log.G(selCtx.Ctx).Warnf("%v select:%v template=%s snapshot storage unavailable", l.ID(), inList[i].ID(), reqRes.TemplateID)
			continue
		}
		nodes.Append(inList[i])
	}
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	} else {
		log.G(selCtx.Ctx).Infof("%v select_size:%v", l.ID(), nodes.Len())
	}
	return nodes, nil
}

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
