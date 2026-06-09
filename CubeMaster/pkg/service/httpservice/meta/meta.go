// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package meta

import (
	"net/http"

	"github.com/gorilla/mux"
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
	nodeAction          = "/nodes/{node_id}"
	nodeStatusAction    = "/nodes/{node_id}/status"
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

func ReadyzHandler(w http.ResponseWriter, r *http.Request) {
	retCode := int(errorcode.ErrorCode_Success)
	retMsg := "ok"
	if !nodemeta.Ready() {
		retCode = int(errorcode.ErrorCode_MasterInternalError)
		retMsg = "metadata service not ready"
	}
	common.WriteResponse(w, http.StatusOK, &sandboxtypes.Res{
		Ret: &sandboxtypes.Ret{
			RetCode: retCode,
			RetMsg:  retMsg,
		},
	})
}

func RegisterNodeHandler(w http.ResponseWriter, r *http.Request) {
	req := &nodemeta.RegisterNodeRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	data, err := nodemeta.RegisterNode(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusOK, err)
		return
	}
	common.WriteResponse(w, http.StatusOK, &nodeResponse{
		RequestID: req.RequestID,
		Ret:       successRet(),
		Data:      data,
	})
}

func UpdateNodeStatusHandler(w http.ResponseWriter, r *http.Request) {
	req := &nodemeta.UpdateNodeStatusRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	nodeID := mux.Vars(r)["node_id"]
	data, err := nodemeta.UpdateNodeStatus(r.Context(), nodeID, req)
	if err != nil {
		writeErr(w, http.StatusOK, err)
		return
	}
	common.WriteResponse(w, http.StatusOK, &nodeResponse{
		RequestID: req.RequestID,
		Ret:       successRet(),
		Data:      data,
	})
}

func GetNodeHandler(w http.ResponseWriter, r *http.Request) {
	nodeID := mux.Vars(r)["node_id"]
	data, err := nodemeta.GetNode(r.Context(), nodeID)
	if err != nil {
		writeErr(w, http.StatusOK, err)
		return
	}
	common.WriteResponse(w, http.StatusOK, &nodeResponse{
		Ret:  successRet(),
		Data: data,
	})
}

func ListNodesHandler(w http.ResponseWriter, r *http.Request) {
	data, err := nodemeta.ListNodes(r.Context())
	if err != nil {
		writeErr(w, http.StatusOK, err)
		return
	}
	common.WriteResponse(w, http.StatusOK, &nodesResponse{
		Ret:  successRet(),
		Data: data,
	})
}

func VersionMatrixHandler(w http.ResponseWriter, r *http.Request) {
	data, err := nodemeta.GetVersionMatrix(r.Context())
	if err != nil {
		writeErr(w, http.StatusOK, err)
		return
	}
	common.WriteResponse(w, http.StatusOK, &versionMatrixResponse{
		Ret:  successRet(),
		Data: data,
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
