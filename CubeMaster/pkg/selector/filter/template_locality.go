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

// templateLocalityFilter 模板本地化过滤插件：
// 保证带有模板的请求只调度到已缓存该模板镜像、且在允许调度范围内的节点
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

// Select 过滤规则（仅当请求携带 TemplateID 时生效）：
// 1. 节点必须在模板允许的调度范围内（TemplateNodeScope）；
// 2. 节点本地必须已缓存模板镜像；
// 3. 若请求强制要求快照存储，节点还需支持快照存储写入
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
	for i := range inList {
		if !templateNodeAllowed(reqRes, inList[i]) {
			log.G(selCtx.Ctx).Warnf("%v select:%v template=%s not in template scope", l.ID(), inList[i].ID(), reqRes.TemplateID)
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
				log.G(selCtx.Ctx).Warnf("%v select:%v template=%s not local", l.ID(), inList[i].ID(), reqRes.TemplateID)
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
					log.G(selCtx.Ctx).Warnf("%v select:%v template=%s snapshot storage unavailable", l.ID(), inList[i].ID(), reqRes.TemplateID)
					continue
				}
			}
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

// templateNodeAllowed 判断节点是否在模板允许的调度范围内；
// 未配置范围时所有节点均允许
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

// snapshotStorageNodeAllowed 判断节点是否允许快照存储写入
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
