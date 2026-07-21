// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package meta

import "github.com/gin-gonic/gin"

// RegisterMetaRoutes wires all /internal/meta routes onto the given group.
// Route paths use gin param syntax (e.g. "/nodes/:node_id").
func RegisterMetaRoutes(g *gin.RouterGroup) {
	g.GET(readyzAction, readyzGinHandler)
	g.POST(registerNodeAction, registerNodeGinHandler)
	g.GET(nodesAction, listNodesGinHandler)
	g.GET(versionMatrixAction, versionMatrixGinHandler)
	g.GET(nodeAction, getNodeGinHandler)
	g.POST(nodeStatusAction, updateNodeStatusGinHandler)
	g.POST(nodeLabelsAction, updateNodeLabelsGinHandler)
	g.DELETE(nodeLabelsAction, deleteNodeLabelGinHandler)
	g.PUT(nodeIsolationAction, isolateNodeGinHandler)
	g.DELETE(nodeIsolationAction, unisolateNodeGinHandler)
}
