// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// newMetaEngine wires the real /internal/meta routes onto a gin engine exactly
// as production (RegisterMetaRoutes) and injects a request trace the way the
// request middleware does, so every handler runs with a non-nil trace — no DB,
// no config, no redis.
func newMetaEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			CubeLog.WithRequestTrace(c.Request.Context(), &CubeLog.RequestTrace{}))
		c.Next()
	})
	RegisterMetaRoutes(r.Group(MetaURI()))
	return r
}

// retEnvelope decodes only the ret block so tests can assert on ret_code
// without depending on each response's data shape.
type retEnvelope struct {
	Ret *struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
}

func decodeRetCode(t *testing.T, body []byte) int {
	t.Helper()
	var env retEnvelope
	require.NoError(t, common.FastestJsoniter.Unmarshal(body, &env))
	require.NotNil(t, env.Ret, "response must carry a ret envelope: %s", string(body))
	return env.Ret.RetCode
}

// TestGetNodeExtractsNodeIDFromPath pins gin's c.Param("node_id") wiring after
// the migration: the path segment reaches nodemeta.GetNode and the success
// envelope is HTTP 200 / ret_code 200.
func TestGetNodeExtractsNodeIDFromPath(t *testing.T) {
	var gotNodeID string
	patch := gomonkey.ApplyFunc(nodemeta.GetNode,
		func(_ context.Context, nodeID string) (*nodemeta.NodeSnapshot, error) {
			gotNodeID = nodeID
			return &nodemeta.NodeSnapshot{NodeID: nodeID}, nil
		})
	defer patch.Reset()

	r := newMetaEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, MetaURI()+"/nodes/node-42", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "node-42", gotNodeID, "node_id must be extracted from the path param")
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
}

// TestDeleteNodeLabelExtractsNodeIDAndKeyQuery proves both extraction axes:
// node_id from the path AND key from c.Query("key").
func TestDeleteNodeLabelExtractsNodeIDAndKeyQuery(t *testing.T) {
	var gotNodeID, gotKey string
	patch := gomonkey.ApplyFunc(nodemeta.DeleteNodeLabel,
		func(_ context.Context, nodeID, key string) error {
			gotNodeID = nodeID
			gotKey = key
			return nil
		})
	defer patch.Reset()

	r := newMetaEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete,
		MetaURI()+"/nodes/node-7/labels?key=zone", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "node-7", gotNodeID, "node_id must come from the path")
	assert.Equal(t, "zone", gotKey, `label key must come from c.Query("key")`)
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
}

// TestIsolateNodeSetsSchedulingDisabledTrue verifies PUT .../isolation forwards
// disabled=true to the business func.
func TestIsolateNodeSetsSchedulingDisabledTrue(t *testing.T) {
	var gotNodeID string
	var gotDisabled bool
	patch := gomonkey.ApplyFunc(nodemeta.SetNodeSchedulingDisabled,
		func(_ context.Context, nodeID string, disabled bool) (*nodemeta.NodeSnapshot, error) {
			gotNodeID = nodeID
			gotDisabled = disabled
			return &nodemeta.NodeSnapshot{NodeID: nodeID}, nil
		})
	defer patch.Reset()

	r := newMetaEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut,
		MetaURI()+"/nodes/node-1/isolation", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "node-1", gotNodeID)
	assert.True(t, gotDisabled, "PUT isolation must pass disabled=true")
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
}

// TestUnisolateNodeSetsSchedulingDisabledFalse verifies DELETE .../isolation
// forwards disabled=false to the business func.
func TestUnisolateNodeSetsSchedulingDisabledFalse(t *testing.T) {
	var gotDisabled bool
	patch := gomonkey.ApplyFunc(nodemeta.SetNodeSchedulingDisabled,
		func(_ context.Context, nodeID string, disabled bool) (*nodemeta.NodeSnapshot, error) {
			gotDisabled = disabled
			return &nodemeta.NodeSnapshot{NodeID: nodeID}, nil
		})
	defer patch.Reset()

	r := newMetaEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete,
		MetaURI()+"/nodes/node-1/isolation", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, gotDisabled, "DELETE isolation must pass disabled=false")
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
}

// TestRegisterNodeRejectsMalformedBodyWith400 locks the parse-time guard: a
// malformed body short-circuits to HTTP 400 before any business call, so no
// patch is required.
func TestRegisterNodeRejectsMalformedBodyWith400(t *testing.T) {
	r := newMetaEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		MetaURI()+"/nodes/register", strings.NewReader("{not json")))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	// writeErr maps an unrecognized parse error to MasterInternalError.
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), decodeRetCode(t, w.Body.Bytes()))
}

// TestRegisterNodeParsesValidBodyAndCallsRegisterNode proves a valid body is
// parsed and forwarded to nodemeta.RegisterNode, returning the success envelope.
func TestRegisterNodeParsesValidBodyAndCallsRegisterNode(t *testing.T) {
	var gotReq *nodemeta.RegisterNodeRequest
	patch := gomonkey.ApplyFunc(nodemeta.RegisterNode,
		func(_ context.Context, req *nodemeta.RegisterNodeRequest) (*nodemeta.NodeSnapshot, error) {
			gotReq = req
			return &nodemeta.NodeSnapshot{NodeID: req.NodeID}, nil
		})
	defer patch.Reset()

	body := `{"requestID":"req-1","node_id":"node-9","host_ip":"10.0.0.9"}`
	r := newMetaEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		MetaURI()+"/nodes/register", strings.NewReader(body)))

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, gotReq, "RegisterNode must be invoked with the parsed body")
	assert.Equal(t, "req-1", gotReq.RequestID)
	assert.Equal(t, "node-9", gotReq.NodeID)
	assert.Equal(t, "10.0.0.9", gotReq.HostIP)
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
	assert.Contains(t, w.Body.String(), `"node_id":"node-9"`)
}
