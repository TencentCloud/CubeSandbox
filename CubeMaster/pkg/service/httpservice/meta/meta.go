// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package meta

import (
	"fmt"
	"net/http"
	"strings"

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
	nodeIsolationAction = "/nodes/{node_id}/isolation"
	versionMatrixAction = "/version-matrix"

	// maxIsolationReasonLen caps the operator-supplied reason to keep it from
	// bloating the registration row or polluting logs.
	maxIsolationReasonLen = 512
	// maxIsolationOperatorLen matches the isolated_by varchar(128) column.
	maxIsolationOperatorLen = 128
	// operatorHeader carries the authenticated principal forwarded by CubeAPI.
	// The isolation audit field is taken from here, never from the request body,
	// so callers cannot forge who performed the action.
	operatorHeader = "X-Operator"
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

func NodeIsolationAction() string {
	return nodeIsolationAction
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

type updateNodeIsolationRequest struct {
	RequestID string `json:"requestID,omitempty"`
	Isolated  bool   `json:"isolated"`
	Reason    string `json:"reason,omitempty"`
}

// UpdateNodeIsolationHandler applies or clears an administrative cordon on a
// node. The audit identity (isolated_by) is taken from the X-Operator header
// set by CubeAPI from the authenticated principal, never from the request body.
func UpdateNodeIsolationHandler(w http.ResponseWriter, r *http.Request) {
	req := &updateNodeIsolationRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	nodeID := mux.Vars(r)["node_id"]
	if err := validateNodeID(nodeID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) > maxIsolationReasonLen {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("reason exceeds %d characters", maxIsolationReasonLen))
		return
	}
	if strings.ContainsAny(reason, "\x00\r\n") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("reason contains control characters"))
		return
	}
	operator := strings.TrimSpace(r.Header.Get(operatorHeader))
	if operator == "" {
		operator = "unknown"
	}
	// isolated_by is persisted into a varchar(128) column; cap defensively so an
	// oversized (or hostile) X-Operator cannot fail the UPDATE or bloat the row.
	if len(operator) > maxIsolationOperatorLen {
		operator = operator[:maxIsolationOperatorLen]
	}
	data, err := nodemeta.SetNodeIsolated(r.Context(), nodeID, req.Isolated, operator, reason)
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

// validateNodeID rejects ids that could break path routing or be used for path
// traversal. Node ids in the open-source deployment are commonly IPv4 addresses
// (so '.' and ':' must be allowed), unlike validatePathSegment elsewhere.
func validateNodeID(id string) error {
	if id == "" {
		return fmt.Errorf("node_id is required")
	}
	if len(id) > 255 {
		return fmt.Errorf("node_id too long")
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("invalid node_id")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return fmt.Errorf("invalid node_id")
		}
	}
	return nil
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
