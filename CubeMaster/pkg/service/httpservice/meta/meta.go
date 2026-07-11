// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package meta

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	metaURI             = "/internal/meta"
	readyzAction        = "/readyz"
	registerNodeAction  = "/nodes/register"
	nodesAction         = "/nodes"
	nodeAction          = "/nodes/:node_id"
	nodeStatusAction    = "/nodes/:node_id/status"
	nodeLabelsAction    = "/nodes/:node_id/labels"
	versionMatrixAction = "/version-matrix"
)

type nodesResponse struct {
	RequestID string                   `json:"requestID,omitempty"`
	Ret       *sandboxtypes.Ret        `json:"ret,omitempty"`
	Data      []*nodemeta.NodeSnapshot `json:"data,omitempty"`
}

type nodeResponse struct {
	RequestID string                 `json:"requestID,omitempty"`
	Ret       *sandboxtypes.Ret      `json:"ret,omitempty"`
	Data      *nodemeta.NodeSnapshot `json:"data,omitempty"`
}

type versionMatrixResponse struct {
	RequestID string                  `json:"requestID,omitempty"`
	Ret       *sandboxtypes.Ret       `json:"ret,omitempty"`
	Data      *nodemeta.VersionMatrix `json:"data,omitempty"`
}

func MetaURI() string {
	return metaURI
}

func ReadyzAction() string {
	return readyzAction
}

func RegisterNodeAction() string {
	return registerNodeAction
}

func NodesAction() string {
	return nodesAction
}

func NodeAction() string {
	return nodeAction
}

func NodeStatusAction() string {
	return nodeStatusAction
}

func VersionMatrixAction() string {
	return versionMatrixAction
}

func NodeLabelsAction() string {
	return nodeLabelsAction
}

func readyzGinHandler(c *gin.Context) {
	retCode := int(errorcode.ErrorCode_Success)
	retMsg := "ok"
	if !nodemeta.Ready() {
		retCode = int(errorcode.ErrorCode_MasterInternalError)
		retMsg = "metadata service not ready"
	}
	common.WriteResponse(c.Writer, http.StatusOK, &sandboxtypes.Res{
		Ret: &sandboxtypes.Ret{
			RetCode: retCode,
			RetMsg:  retMsg,
		},
	})
}

func registerNodeGinHandler(c *gin.Context) {
	req := &nodemeta.RegisterNodeRequest{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		writeErr(c.Writer, http.StatusBadRequest, err)
		return
	}
	data, err := nodemeta.RegisterNode(c.Request.Context(), req)
	if err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &nodeResponse{
		RequestID: req.RequestID,
		Ret:       successRet(),
		Data:      data,
	})
}

func updateNodeStatusGinHandler(c *gin.Context) {
	req := &nodemeta.UpdateNodeStatusRequest{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		writeErr(c.Writer, http.StatusBadRequest, err)
		return
	}
	nodeID := c.Param("node_id")
	data, err := nodemeta.UpdateNodeStatus(c.Request.Context(), nodeID, req)
	if err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &nodeResponse{
		RequestID: req.RequestID,
		Ret:       successRet(),
		Data:      data,
	})
}

func getNodeGinHandler(c *gin.Context) {
	nodeID := c.Param("node_id")
	data, err := nodemeta.GetNode(c.Request.Context(), nodeID)
	if err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &nodeResponse{
		Ret:  successRet(),
		Data: data,
	})
}

func listNodesGinHandler(c *gin.Context) {
	data, err := nodemeta.ListNodes(c.Request.Context())
	if err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &nodesResponse{
		Ret:  successRet(),
		Data: data,
	})
}

func versionMatrixGinHandler(c *gin.Context) {
	data, err := nodemeta.GetVersionMatrix(c.Request.Context())
	if err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &versionMatrixResponse{
		Ret:  successRet(),
		Data: data,
	})
}

func updateNodeLabelsGinHandler(c *gin.Context) {
	nodeID := c.Param("node_id")
	req := &nodemeta.UpdateNodeLabelsRequest{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		writeErr(c.Writer, http.StatusBadRequest, err)
		return
	}
	if err := nodemeta.UpdateNodeLabels(c.Request.Context(), nodeID, req.Labels); err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &sandboxtypes.Res{
		Ret: successRet(),
	})
}

func deleteNodeLabelGinHandler(c *gin.Context) {
	nodeID := c.Param("node_id")
	key := c.Query("key")
	if err := nodemeta.DeleteNodeLabel(c.Request.Context(), nodeID, key); err != nil {
		writeErr(c.Writer, http.StatusOK, err)
		return
	}
	common.WriteResponse(c.Writer, http.StatusOK, &sandboxtypes.Res{
		Ret: successRet(),
	})
}

func successRet() *sandboxtypes.Ret {
	return &sandboxtypes.Ret{
		RetCode: int(errorcode.ErrorCode_Success),
		RetMsg:  errorcode.ErrorCode_Success.String(),
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	retCode := int(errorcode.ErrorCode_MasterInternalError)
	if err == gorm.ErrRecordNotFound {
		retCode = int(errorcode.ErrorCode_NotFound)
	}
	common.WriteResponse(w, status, &sandboxtypes.Res{
		Ret: &sandboxtypes.Ret{
			RetCode: retCode,
			RetMsg:  err.Error(),
		},
	})
}
