// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestCreateSnapshotSuccessResponse(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot host resolution context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName, backend string, _ snapshotmode.Mode) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{
			JobID:        "op-1",
			TemplateID:   "snap-1",
			RequestID:    requestID,
			SandboxID:    sandboxID,
			ResourceType: "snapshot",
			ResourceID:   "snap-1",
			Operation:    "SNAPSHOT_CREATE",
			Status:       "READY",
			Phase:        "REGISTERING",
		}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{
			SnapshotID:      snapshotID,
			Status:          "READY",
			OriginSandboxID: knownSandboxTestID,
			StorageBackend:  "cubecow",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"requestID":"req-1",
		"sandbox_id":"`+knownSandboxTestID+`",
		"display_name":"my-snapshot"
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)

	got, ok := resp.(*snapshotResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	if assert.NotNil(t, got.Snapshot) {
		assert.Equal(t, "snap-1", got.Snapshot.SnapshotID)
		assert.Equal(t, knownSandboxTestID, got.Snapshot.OriginSandboxID)
	}
	if assert.NotNil(t, got.Operation) {
		assert.Equal(t, "op-1", got.Operation.OperationID)
		assert.Equal(t, "snap-1", got.Operation.SnapshotID)
		assert.Equal(t, "READY", got.Operation.Status)
	}
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestCreateSnapshotRejectsReservedPrefixNameAsParamsError(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})
	resolveSnapshotHostFn = func(context.Context, string, string) (string, string, error) {
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName, backend string, _ snapshotmode.Mode) (*types.TemplateImageJobInfo, error) {
		return nil, fmt.Errorf("%w: %q", templatecenter.ErrInvalidSnapshotName, displayName)
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"requestID":"req-1",
		"sandbox_id":"`+knownSandboxTestID+`",
		"display_name":"snap-backup"
	}`))
	resp := createSnapshot(req, &CubeLog.RequestTrace{})

	got, ok := resp.(*snapshotResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
}

// Business failures return HTTP 200, so the route span must carry the real ret_code.
func TestCreateSnapshotHandlerReportsRouteSpanBusinessStatus(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		stub     func(*gomonkey.Patches)
		wantCode int
	}{
		{
			name:     "missing sandbox id",
			body:     `{"request_id":"req-1"}`,
			stub:     func(*gomonkey.Patches) {},
			wantCode: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name: "sandbox not found",
			body: `{"request_id":"req-1","sandbox_id":"sb-missing"}`,
			stub: func(p *gomonkey.Patches) {
				p.ApplyFunc(sandbox.ResolveSandboxID, func(context.Context, string) (string, error) {
					return "", sandboxid.ErrNotFound
				})
			},
			wantCode: int(errorcode.ErrorCode_NotFound),
		},
	}

	const unknownSandboxRequestID = "req-1"
	const unknownSandboxID = "sb-missing"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := setupSpanRecorder(t)
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			tc.stub(patches)

			rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
			req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req.WithContext(rootCtx)
			createSnapshotGinHandler(c)
			rootSpan.End()
			flush()

			var got snapshotResponse
			require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
			require.NotNil(t, got.Res)
			require.NotNil(t, got.Res.Ret)
			assert.Equal(t, tc.wantCode, got.Res.Ret.RetCode)

			routes := rec.named("test.root")
			require.Len(t, routes, 1)
			if routes[0].Status().Code != codes.Error {
				t.Errorf("route span status = %v, want Error on a business failure", routes[0].Status().Code)
			}
			if got := attrInt(t, routes[0].Attributes(), telemetry.AttrRetCode); got != int64(tc.wantCode) {
				t.Errorf("route span %s = %d, want %d", telemetry.AttrRetCode, got, tc.wantCode)
			}
			if tc.name == "sandbox not found" {
				attrs := routes[0].Attributes()
				assert.Equal(t, unknownSandboxRequestID, attrString(attrs, telemetry.AttrRequestID))
				assert.Equal(t, unknownSandboxID, attrString(attrs, telemetry.AttrSandboxID))
				assert.Empty(t, rec.named(telemetry.SpanSnapshotCreateCapture))
			}
		})
	}
}

func TestCreateSnapshotHandlerPopulatesRouteSpanIdentifiers(t *testing.T) {
	registerKnownSandboxTestID(t)
	rec, flush := setupSpanRecorder(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName, backend string, _ snapshotmode.Mode) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{JobID: "op-1", TemplateID: "snap-1", RequestID: requestID, Status: "READY"}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{SnapshotID: snapshotID, Status: "READY"}, nil
	}

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	body := `{"request_id":"req-span","sandbox_id":"` + knownSandboxTestID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(body))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(CubeLog.WithRequestTrace(rootCtx, &CubeLog.RequestTrace{}))
	createSnapshotGinHandler(c)
	rootSpan.End()
	flush()

	routes := rec.named("test.root")
	require.Len(t, routes, 1)
	attrs := routes[0].Attributes()
	assert.Equal(t, "req-span", attrString(attrs, telemetry.AttrRequestID))
	assert.Equal(t, knownSandboxTestID, attrString(attrs, telemetry.AttrSandboxID))
	assert.Equal(t, "snap-1", attrString(attrs, telemetry.AttrSnapshotID))
	assert.Equal(t, "op-1", attrString(attrs, telemetry.AttrJobID))
}

func TestSnapshotErrorCodeMapsMySQLLockErrorsToDBError(t *testing.T) {
	for _, err := range []error{
		&mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"},
		&mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"},
	} {
		assert.Equal(t, int(errorcode.ErrorCode_DBError), snapshotErrorCode(err))
	}
}

// Cubelet wraps the stable identifier in a sentence and Master stores that text
// in the job's error_message, so the status must be derived from the identifier
// by containment.
func TestSnapshotErrorCodeClassifiesStableIdentifiers(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{
			err:  fmt.Errorf("failed to capture sandbox snapshot: %s: guest agent too old", snapshotmode.ErrFilesystemOnlyFreezeUnsupported),
			want: int(errorcode.ErrorCode_Conflict),
		},
		{
			err:  fmt.Errorf("failed to capture sandbox snapshot: %s: not a blk-cube disk", snapshotmode.ErrFilesystemOnlyRootfsUnsupported),
			want: int(errorcode.ErrorCode_Conflict),
		},
		{
			err:  fmt.Errorf("%s: snap-1", snapshotmode.ErrFilesystemOnlyRollback),
			want: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			err:  fmt.Errorf("%s: node heartbeat missing", snapshotmode.ErrFilesystemOnlyNodeUnknown),
			want: int(errorcode.ErrorCode_MasterUnavailable),
		},
		{
			// Server-side failure: no client action, keeps the existing default.
			err:  fmt.Errorf("%s: FIFREEZE failed", snapshotmode.ErrFilesystemOnlyFreezeFailed),
			want: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			// Not-found messages quote the requested id, so an id that looks
			// like an identifier must not turn a 404 into a 400.
			err:  fmt.Errorf("%w: snapshot %s", templatecenter.ErrSnapshotNotFound, snapshotmode.ErrSnapshotModeInvalid),
			want: int(errorcode.ErrorCode_NotFound),
		},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, snapshotErrorCode(tc.err), tc.err.Error())
	}
}

func TestCreateSnapshotAcceptsSnakeCaseRequestID(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName, backend string, _ snapshotmode.Mode) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{JobID: "op-2", TemplateID: "snap-2", RequestID: requestID}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{SnapshotID: snapshotID}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"request_id":"req-snake",
		"sandbox_id":"`+knownSandboxTestID+`"
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)

	got := resp.(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-snake", got.RequestID)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestCreateSnapshotDetachesExecutionFromCanceledRequest(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot host resolution context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName, backend string, _ snapshotmode.Mode) (*types.TemplateImageJobInfo, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot execution context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return &types.TemplateImageJobInfo{
			JobID:      "op-detached",
			TemplateID: "snap-detached",
			RequestID:  requestID,
			Status:     "READY",
		}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot info lookup context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return &templatecenter.SnapshotInfo{SnapshotID: snapshotID, Status: "READY"}, nil
	}

	baseReq := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"request_id":"req-detached",
		"sandbox_id":"`+knownSandboxTestID+`"
	}`))
	canceledCtx, cancel := context.WithCancel(baseReq.Context())
	cancel()
	req := baseReq.WithContext(canceledCtx)

	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)
	got := resp.(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
}

func TestSnapshotExecutionContextDetachesFromParentCancellation(t *testing.T) {
	setupSpanRecorder(t)

	parentBase, cancelParent := context.WithCancel(context.Background())
	parentCtx, parentSpan := telemetry.Start(parentBase, "test.parent")
	defer parentSpan.End()
	cancelParent()

	before := time.Now()
	ctx, cancel := snapshotExecutionContext(parentCtx, map[string]any{"RequestId": "req-ctx"})
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("snapshot HTTP execution context should not inherit parent cancellation: %v", ctx.Err())
	default:
	}

	sc := trace.SpanContextFromContext(ctx)
	if sc.TraceID() != parentSpan.SpanContext().TraceID() {
		t.Errorf("detached trace id = %s, want the request trace %s", sc.TraceID(), parentSpan.SpanContext().TraceID())
	}
	if sc.SpanID() != parentSpan.SpanContext().SpanID() {
		t.Errorf("detached parent span id = %s, want the request span %s", sc.SpanID(), parentSpan.SpanContext().SpanID())
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("detached context must carry the independent snapshot operation deadline")
	}
	want := before.Add(templatecenter.SnapshotOperationTimeout())
	if deadline.Before(want.Add(-time.Second)) || deadline.After(want.Add(time.Second)) {
		t.Errorf("detached deadline = %s, want ~%s (independent operation budget)", deadline, want)
	}
}

func TestHandleSnapshotOperationMapsNotFound(t *testing.T) {
	origGetSnapshotOperationFn := getSnapshotOperationFn
	t.Cleanup(func() {
		getSnapshotOperationFn = origGetSnapshotOperationFn
	})
	getSnapshotOperationFn = func(ctx context.Context, operationID string) (*templatecenter.SnapshotOperationInfo, error) {
		return nil, templatecenter.ErrSnapshotOperationNotFound
	}

	rt := &CubeLog.RequestTrace{}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = httptest.NewRequest(http.MethodGet, "/cube/operation/op-missing", nil).WithContext(ctx)
	gc.Params = gin.Params{{Key: "operation_id", Value: "op-missing"}}
	handleSnapshotOperationAction(gc)

	var got operationResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_NotFound), rt.RetCode)
}

func TestGetSnapshotListSupportsFiltersAndPagination(t *testing.T) {
	origListSnapshotsFn := listSnapshotsFn
	t.Cleanup(func() {
		listSnapshotsFn = origListSnapshotsFn
	})
	listSnapshotsFn = func(ctx context.Context, opts *templatecenter.ListSnapshotsOptions) ([]templatecenter.SnapshotInfo, string, error) {
		assert.Equal(t, "snap-1", opts.SnapshotID)
		assert.Equal(t, "sb-1", opts.SandboxID)
		assert.Equal(t, "prod", opts.Name)
		assert.Equal(t, "READY", opts.Status)
		assert.Equal(t, 1, opts.Limit)
		assert.Equal(t, "1", opts.NextToken)
		return []templatecenter.SnapshotInfo{{SnapshotID: "snap-1", OriginSandboxID: "sb-1", Status: "READY"}}, "2", nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot?snapshot_id=snap-1&sandbox_id=sb-1&name=prod&status=READY&limit=1&next_token=1&request_id=req-list", nil)
	rt := &CubeLog.RequestTrace{}
	resp := getSnapshot(req, rt, "")

	got := resp.(*snapshotListResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-list", got.RequestID)
	assert.Equal(t, "2", got.NextToken)
	if assert.Len(t, got.Data, 1) {
		assert.Equal(t, "snap-1", got.Data[0].SnapshotID)
	}
}

func TestSnapshotResourceHidesInternalNetworkQosAnnotation(t *testing.T) {
	annotations := map[string]string{
		constants.CubeAnnotationsNetWork:              `{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}}}`,
		constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
	}
	resource := snapshotResourceFromInfo(&templatecenter.SnapshotInfo{
		SnapshotID: "snap-1",
		CreateRequest: &types.CreateCubeSandboxReq{
			Annotations: annotations,
		},
	})

	if assert.NotNil(t, resource.CreateRequest) {
		assert.NotContains(t, resource.CreateRequest.Annotations, constants.CubeAnnotationsNetWork)
		assert.Equal(t, "tpl-1", resource.CreateRequest.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	assert.Contains(t, annotations, constants.CubeAnnotationsNetWork)
}

func TestHandleSandboxRollbackActionUsesPathSandboxID(t *testing.T) {
	registerKnownSandboxTestID(t)

	origRollbackSnapshotFn := rollbackSnapshotFn
	t.Cleanup(func() {
		rollbackSnapshotFn = origRollbackSnapshotFn
	})
	rollbackSnapshotFn = func(ctx context.Context, requestID, sandboxID, snapshotID, instanceType, backend string) (*types.TemplateImageJobInfo, error) {
		assert.Equal(t, "req-rb", requestID)
		assert.Equal(t, knownSandboxTestID, sandboxID)
		assert.Equal(t, "snap-1", snapshotID)
		return &types.TemplateImageJobInfo{
			JobID:      "op-rb",
			RequestID:  requestID,
			SandboxID:  sandboxID,
			ResourceID: snapshotID,
			Status:     "READY",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/"+knownSandboxTestID+"/rollback", strings.NewReader(`{
		"request_id":"req-rb",
		"snapshot_id":"snap-1"
	}`))
	rt := &CubeLog.RequestTrace{}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = req.WithContext(ctx)
	gc.Params = gin.Params{{Key: "sandbox_id", Value: knownSandboxTestID}}
	handleSandboxRollbackAction(gc)

	var got operationResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-rb", got.RequestID)
	assert.Equal(t, "op-rb", got.Operation.OperationID)
	assert.Equal(t, "READY", got.Operation.Status)
}

func TestHandleSandboxRollbackActionPopulatesRouteSpanIdentifiers(t *testing.T) {
	registerKnownSandboxTestID(t)
	rec, flush := setupSpanRecorder(t)

	origRollbackSnapshotFn := rollbackSnapshotFn
	t.Cleanup(func() {
		rollbackSnapshotFn = origRollbackSnapshotFn
	})
	rollbackSnapshotFn = func(ctx context.Context, requestID, sandboxID, snapshotID, instanceType, backend string) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{
			JobID:      "op-rb",
			RequestID:  requestID,
			SandboxID:  sandboxID,
			ResourceID: snapshotID,
			Status:     "READY",
		}, nil
	}

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/"+knownSandboxTestID+"/rollback", strings.NewReader(`{
		"request_id":"req-rb",
		"snapshot_id":"snap-1"
	}`))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(CubeLog.WithRequestTrace(rootCtx, &CubeLog.RequestTrace{}))
	c.Params = gin.Params{{Key: "sandbox_id", Value: knownSandboxTestID}}
	handleSandboxRollbackAction(c)
	rootSpan.End()
	flush()

	routes := rec.named("test.root")
	require.Len(t, routes, 1)
	attrs := routes[0].Attributes()
	assert.Equal(t, "req-rb", attrString(attrs, telemetry.AttrRequestID))
	assert.Equal(t, knownSandboxTestID, attrString(attrs, telemetry.AttrSandboxID))
	assert.Equal(t, "snap-1", attrString(attrs, telemetry.AttrSnapshotID))
	assert.Equal(t, "op-rb", attrString(attrs, telemetry.AttrJobID))
}

func TestHandleSandboxRollbackActionReportsBusinessStatus(t *testing.T) {
	registerKnownSandboxTestID(t)
	rec, flush := setupSpanRecorder(t)

	origRollbackSnapshotFn := rollbackSnapshotFn
	t.Cleanup(func() {
		rollbackSnapshotFn = origRollbackSnapshotFn
	})
	rollbackSnapshotFn = func(context.Context, string, string, string, string, string) (*types.TemplateImageJobInfo, error) {
		return nil, errors.New("rollback boom")
	}

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/"+knownSandboxTestID+"/rollback", strings.NewReader(`{
		"request_id":"req-rb",
		"snapshot_id":"snap-1"
	}`))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(CubeLog.WithRequestTrace(rootCtx, &CubeLog.RequestTrace{}))
	c.Params = gin.Params{{Key: "sandbox_id", Value: knownSandboxTestID}}
	handleSandboxRollbackAction(c)
	rootSpan.End()
	flush()

	var got operationResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	require.NotNil(t, got.Res)
	require.NotNil(t, got.Res.Ret)
	require.NotEqual(t, int(errorcode.ErrorCode_Success), got.Res.Ret.RetCode)

	routes := rec.named("test.root")
	require.Len(t, routes, 1)
	if routes[0].Status().Code != codes.Error {
		t.Errorf("route span status = %v, want Error on a business failure", routes[0].Status().Code)
	}
	if got := attrInt(t, routes[0].Attributes(), telemetry.AttrRetCode); got == 0 {
		t.Error("rollback route span must carry the business ret_code")
	}
}

func TestHandleSandboxRollbackActionReportsRouteSpanRejections(t *testing.T) {
	const rollbackPath = "/cube/sandbox/:sandbox_id/rollback"
	const legacyPath = "/cube/sandbox/rollback"
	const otherSandboxID = "ffffffffffffffffffffffffffffffff"

	cases := []struct {
		name     string
		route    string
		reqPath  string
		body     string
		stub     func(*gomonkey.Patches)
		wantCode int
	}{
		{
			name:     "malformed body",
			route:    rollbackPath,
			reqPath:  "/cube/sandbox/" + knownSandboxTestID + "/rollback",
			body:     `{"request_id":`,
			wantCode: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name:     "missing request id",
			route:    rollbackPath,
			reqPath:  "/cube/sandbox/" + knownSandboxTestID + "/rollback",
			body:     `{"sandbox_id":"` + knownSandboxTestID + `","snapshot_id":"snap-1"}`,
			wantCode: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name:     "missing snapshot id",
			route:    rollbackPath,
			reqPath:  "/cube/sandbox/" + knownSandboxTestID + "/rollback",
			body:     `{"request_id":"req-1","sandbox_id":"` + knownSandboxTestID + `"}`,
			wantCode: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name:     "missing sandbox id",
			route:    legacyPath,
			reqPath:  legacyPath,
			body:     `{"request_id":"req-1","snapshot_id":"snap-1"}`,
			wantCode: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name:    "unresolvable sandbox id",
			route:   rollbackPath,
			reqPath: "/cube/sandbox/sb-missing/rollback",
			body:    `{"request_id":"req-1","sandbox_id":"sb-missing","snapshot_id":"snap-1"}`,
			stub: func(p *gomonkey.Patches) {
				p.ApplyFunc(sandbox.ResolveSandboxID, func(context.Context, string) (string, error) {
					return "", sandboxid.ErrNotFound
				})
			},
			wantCode: int(errorcode.ErrorCode_NotFound),
		},
		{
			name:    "path body mismatch",
			route:   rollbackPath,
			reqPath: "/cube/sandbox/" + knownSandboxTestID + "/rollback",
			body:    `{"request_id":"req-1","sandbox_id":"` + otherSandboxID + `","snapshot_id":"snap-1"}`,
			stub: func(p *gomonkey.Patches) {
				p.ApplyFunc(sandbox.ResolveSandboxID, func(_ context.Context, id string) (string, error) {
					return id, nil
				})
			},
			wantCode: int(errorcode.ErrorCode_MasterParamsError),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := setupSpanRecorder(t)
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			if tc.stub != nil {
				tc.stub(patches)
			}

			router := gin.New()
			router.Use(telemetry.GinMiddleware())
			router.POST(legacyPath, handleSandboxRollbackAction)
			router.POST(rollbackPath, handleSandboxRollbackAction)

			req := httptest.NewRequest(http.MethodPost, tc.reqPath, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			flush()

			var got operationResponse
			require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
			require.NotNil(t, got.Res)
			require.NotNil(t, got.Res.Ret)
			assert.Equal(t, tc.wantCode, got.Res.Ret.RetCode)

			spans := rec.named("POST " + tc.route)
			require.Len(t, spans, 1)
			if spans[0].Status().Code != codes.Error {
				t.Errorf("route span status = %v, want Error on a rejected rollback", spans[0].Status().Code)
			}
			if got := attrInt(t, spans[0].Attributes(), telemetry.AttrRetCode); got != int64(tc.wantCode) {
				t.Errorf("route span %s = %d, want %d", telemetry.AttrRetCode, got, tc.wantCode)
			}
		})
	}
}

func TestConstrainSnapshotCreateScopeIntersectsRequestedScope(t *testing.T) {
	origResolveSnapshotReadyNodeScopeFn := resolveSnapshotReadyNodeScopeFn
	t.Cleanup(func() {
		resolveSnapshotReadyNodeScopeFn = origResolveSnapshotReadyNodeScopeFn
	})
	resolveSnapshotReadyNodeScopeFn = func(ctx context.Context, snapshotID string) ([]string, error) {
		return []string{"node-a", "node-b"}, nil
	}

	req := &types.CreateCubeSandboxReq{
		DistributionScope: []string{"node-b", "node-c"},
	}
	err := constrainSnapshotCreateScope(context.Background(), "snap-1", req)
	if err != nil {
		t.Fatalf("constrainSnapshotCreateScope failed: %v", err)
	}
	assert.Equal(t, []string{"node-b"}, req.DistributionScope)
}

func stubSnapshotReadyForNewUse(t *testing.T) {
	t.Helper()
	orig := ensureSnapshotReadyForNewUseFn
	ensureSnapshotReadyForNewUseFn = func(ctx context.Context, snapshotID string) error { return nil }
	t.Cleanup(func() { ensureSnapshotReadyForNewUseFn = orig })
}

// TestBindSnapshotCreateReplicaInjectsRuntimeAnnotations verifies the v4
// contract: master sets only the logical snapshot id + attached_at
// annotations, never the physical memory_vol/memory_dev. Any stale physical
// annotation present in the caller-supplied request must be stripped so it
// cannot reach the cubelet.
func TestBindSnapshotCreateReplicaInjectsRuntimeAnnotations(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origResolveSnapshotReadyNodeScopeFn := resolveSnapshotReadyNodeScopeFn
	origResolveSnapshotReadyReplicaFn := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		resolveSnapshotReadyNodeScopeFn = origResolveSnapshotReadyNodeScopeFn
		resolveSnapshotReadyReplicaFn = origResolveSnapshotReadyReplicaFn
	})
	resolveSnapshotReadyNodeScopeFn = func(ctx context.Context, snapshotID string) ([]string, error) {
		return []string{"node-a", "node-b"}, nil
	}
	resolveSnapshotReadyReplicaFn = func(ctx context.Context, snapshotID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		assert.Equal(t, "snap-1", snapshotID)
		assert.Equal(t, "node-a", preferredNodeID)
		// v5: ReplicaStatus is now thin — no physical fields available to
		// accidentally propagate.
		return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
	}

	req := &types.CreateCubeSandboxReq{
		Annotations:       map[string]string{},
		DistributionScope: []string{"node-a", "node-b"},
	}
	err := bindSnapshotCreateReplica(context.Background(), "snap-1", req)
	if err != nil {
		t.Fatalf("bindSnapshotCreateReplica failed: %v", err)
	}
	assert.Equal(t, []string{"node-a"}, req.DistributionScope)
	assert.Equal(t, "snap-1", req.Annotations[constants.CubeAnnotationRuntimeSnapshotID])
	assert.NotEmpty(t, req.Annotations[constants.CubeAnnotationRuntimeSnapshotAttachedAt])
}

func TestBindSnapshotCreateReplicaCrossNodeWhenOriginCannotSchedule(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origDecide := decideRestorePlacementFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		decideRestorePlacementFn = origDecide
	})
	getSnapshotRestoreSourceFn = func(ctx context.Context, snapshotID string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{
			SnapshotID:          snapshotID,
			Backend:             constants.SnapshotBackendS3,
			RemoteStatus:        constants.RemoteStatusReady,
			OriginNodeID:        "node-a",
			OriginNodeIP:        "10.0.0.1",
			OriginHostFactsJSON: `{"cpuid_hash":"sha256:cpu","host_kernel_release":"5.15.0"}`,
			InstanceType:        "cubebox",
			ExportUUIDs:         `{"rootfs":"r1","memory":"m1"}`,
		}, nil
	}
	decideRestorePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		assert.Equal(t, "snap-1", in.SnapshotID)
		assert.Equal(t, constants.SnapshotBackendS3, in.Backend)
		assert.Equal(t, constants.RemoteStatusReady, in.RemoteStatus)
		return &restoreplace.Placement{NodeID: "node-b", NodeIP: "10.0.0.2", CrossNode: true}, nil
	}

	req := &types.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Annotations:  map[string]string{constants.CubeAnnotationComponentCubeShimVersion: "caller-shim"},
	}
	err := bindSnapshotCreateReplica(context.Background(), "snap-1", req)
	if err != nil {
		t.Fatalf("bindSnapshotCreateReplica failed: %v", err)
	}
	assert.NotContains(t, req.Annotations, constants.CubeAnnotationComponentCubeShimVersion)
	assert.Equal(t, []string{"node-b"}, req.DistributionScope)
	assert.Equal(t, "true", req.Annotations[constants.CubeAnnotationSnapshotAllowNonLocal])
	assert.Equal(t, constants.SnapshotBackendS3, req.Annotations[constants.CubeAnnotationStorageBackend])
	assert.Equal(t, `{"rootfs":"r1","memory":"m1"}`, req.Annotations[constants.CubeAnnotationSnapshotRemoteUUIDs])
	assert.Equal(t, "snap-1", req.Annotations[constants.CubeAnnotationRuntimeSnapshotID])
}

func TestBindSnapshotCreateReplicaPinsRawHostMountToOrigin(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origDecide := decideRestorePlacementFn
	origReplica := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		decideRestorePlacementFn = origDecide
		resolveSnapshotReadyReplicaFn = origReplica
	})
	getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{
			SnapshotID: "snap-1", Backend: constants.SnapshotBackendS3,
			RemoteStatus: constants.RemoteStatusReady, OriginNodeID: "node-a", OriginNodeIP: "10.0.0.1",
		}, nil
	}
	decideRestorePlacementFn = func(_ context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		assert.True(t, in.PinToOrigin)
		return &restoreplace.Placement{NodeID: "node-a", NodeIP: "10.0.0.1"}, nil
	}
	resolveSnapshotReadyReplicaFn = func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
		return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
	}
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		sandbox.AnnotationHostDirMount: `[{"hostPath":"/data/shared","mountPath":"/mnt"}]`,
	}}

	require.NoError(t, bindSnapshotCreateReplica(context.Background(), "snap-1", req))
	assert.Equal(t, []string{"node-a"}, req.DistributionScope)
	assert.Empty(t, req.Annotations[constants.CubeAnnotationSnapshotAllowNonLocal])
}

func TestBindSnapshotCreateReplicaAllowsPluginVolumeCrossNode(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origDecide := decideRestorePlacementFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		decideRestorePlacementFn = origDecide
	})
	getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{
			SnapshotID: "snap-1", Backend: constants.SnapshotBackendS3,
			RemoteStatus: constants.RemoteStatusReady, OriginNodeID: "node-a", OriginNodeIP: "10.0.0.1",
		}, nil
	}
	decideRestorePlacementFn = func(_ context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		assert.False(t, in.PinToOrigin)
		return &restoreplace.Placement{NodeID: "node-b", NodeIP: "10.0.0.2", CrossNode: true}, nil
	}
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			sandbox.AnnotationPluginVolumeMounts: `[{"name":"data","container_path":"/mnt/data"}]`,
		},
		Volumes: []*types.Volume{{Name: "data"}},
	}

	require.NoError(t, bindSnapshotCreateReplica(context.Background(), "snap-1", req))
	assert.Equal(t, []string{"node-b"}, req.DistributionScope)
	assert.Equal(t, "true", req.Annotations[constants.CubeAnnotationSnapshotAllowNonLocal])
	assert.Equal(t, "true", req.Annotations[constants.CubeAnnotationSnapshotCrossNode])
}

// A filesystem-only package carries no external mount and cold boot mounts none,
// so the restore has no origin dependency: placement must not be pinned even
// when the snapshot itself was taken from a sandbox with a raw host mount.
func TestBindFilesystemOnlySnapshotDoesNotPinToOrigin(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origDecide := decideRestorePlacementFn
	origReplica := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		decideRestorePlacementFn = origDecide
		resolveSnapshotReadyReplicaFn = origReplica
	})
	getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{
			SnapshotID: "snap-1", Backend: constants.SnapshotBackendS3,
			RemoteStatus: constants.RemoteStatusReady, OriginNodeID: "node-a", OriginNodeIP: "10.0.0.1",
			Mode: snapshotmode.FsOnly,
		}, nil
	}
	decideRestorePlacementFn = func(_ context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		assert.False(t, in.PinToOrigin)
		assert.Equal(t, restoreplace.FilesystemColdBoot, in.Kind)
		return &restoreplace.Placement{NodeID: "node-b", NodeIP: "10.0.0.2", CrossNode: true}, nil
	}
	resolveSnapshotReadyReplicaFn = func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
		return templatecenter.ReplicaStatus{NodeID: "node-b"}, nil
	}

	require.NoError(t, bindSnapshotCreateReplica(context.Background(), "snap-1", &types.CreateCubeSandboxReq{
		Annotations: map[string]string{},
	}))
}

// A cold boot mounts only what the caller declares, and never pins the child to
// the snapshot origin.
func TestBindFilesystemOnlySnapshotAcceptsCallerExternalMounts(t *testing.T) {
	cases := map[string]*types.CreateCubeSandboxReq{
		"host mount": {
			Annotations: map[string]string{
				sandbox.AnnotationHostDirMount: `[{"hostPath":"/data/shared","mountPath":"/mnt/shared"}]`,
			},
		},
		"plugin volume": {
			Annotations: map[string]string{
				sandbox.AnnotationPluginVolumeMounts: `[{"name":"data","container_path":"/mnt/data"}]`,
			},
			Volumes: []*types.Volume{{Name: "data"}},
		},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			stubSnapshotReadyForNewUse(t)
			origSource := getSnapshotRestoreSourceFn
			origDecide := decideRestorePlacementFn
			origReplica := resolveSnapshotReadyReplicaFn
			t.Cleanup(func() {
				getSnapshotRestoreSourceFn = origSource
				decideRestorePlacementFn = origDecide
				resolveSnapshotReadyReplicaFn = origReplica
			})
			getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
				return &templatecenter.RestoreSource{
					SnapshotID: "snap-1", Backend: constants.SnapshotBackendS3,
					RemoteStatus: constants.RemoteStatusReady, OriginNodeID: "node-a", OriginNodeIP: "10.0.0.1",
					Mode: snapshotmode.FsOnly,
				}, nil
			}
			placed := false
			decideRestorePlacementFn = func(_ context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
				placed = true
				assert.False(t, in.PinToOrigin)
				assert.Equal(t, restoreplace.FilesystemColdBoot, in.Kind)
				return &restoreplace.Placement{NodeID: "node-b", NodeIP: "10.0.0.2", CrossNode: true}, nil
			}
			resolveSnapshotReadyReplicaFn = func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
				return templatecenter.ReplicaStatus{NodeID: "node-b"}, nil
			}

			require.NoError(t, bindSnapshotCreateReplica(context.Background(), "snap-1", req))
			assert.True(t, placed)
		})
	}
}

func TestSnapshotRestorePinsOnlyRawHostMountFromStoredTemplate(t *testing.T) {
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}}
	templateReq := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		sandbox.AnnotationHostDirMount: `[{"hostPath":"/data/shared","mountPath":"/mnt"}]`,
	}}

	assert.True(t, snapshotRestoreHasRawHostMount(req, templateReq))
	templateReq.Annotations = map[string]string{
		sandbox.AnnotationPluginVolumeMounts: `[{"name":"data","container_path":"/mnt"}]`,
	}
	assert.False(t, snapshotRestoreHasRawHostMount(req, templateReq))
}

func TestReconcileSnapshotPluginVolumes(t *testing.T) {
	const snapshotMounts = `[{"name":"data","container_path":"/mnt/data"}]`
	tests := []struct {
		name           string
		snapshotMounts string
		requestMounts  string
		requestVolumes []*types.Volume
		wantErr        bool
		wantVolumes    []string
		wantAnnotation bool
	}{
		{
			name:           "identical mounts are dropped from request",
			snapshotMounts: snapshotMounts,
			requestMounts:  `[{"name":"data","container_path":"/mnt/data"}]`,
			requestVolumes: []*types.Volume{{Name: "data"}, {Name: "scratch"}},
			wantVolumes:    []string{"scratch"},
		},
		{
			name:           "equivalent paths after clean are identical",
			snapshotMounts: snapshotMounts,
			requestMounts:  `[{"name":" data ","container_path":"/mnt/data/"}]`,
			requestVolumes: []*types.Volume{{Name: "data"}},
			wantVolumes:    []string{},
		},
		{
			name:           "request without mounts is unchanged",
			snapshotMounts: snapshotMounts,
			requestVolumes: []*types.Volume{{Name: "scratch"}},
			wantVolumes:    []string{"scratch"},
		},
		{
			name:           "snapshot without plugin mounts keeps request mounts",
			requestMounts:  `[{"name":"data","container_path":"/mnt/data"}]`,
			requestVolumes: []*types.Volume{{Name: "data"}},
			wantVolumes:    []string{"data"},
			wantAnnotation: true,
		},
		{
			name:           "different path is rejected",
			snapshotMounts: snapshotMounts,
			requestMounts:  `[{"name":"data","container_path":"/mnt/other"}]`,
			requestVolumes: []*types.Volume{{Name: "data"}},
			wantErr:        true,
		},
		{
			name:           "different readonly is rejected",
			snapshotMounts: snapshotMounts,
			requestMounts:  `[{"name":"data","container_path":"/mnt/data","readonly":true}]`,
			requestVolumes: []*types.Volume{{Name: "data"}},
			wantErr:        true,
		},
		{
			name:           "extra volume is rejected",
			snapshotMounts: snapshotMounts,
			requestMounts:  `[{"name":"data","container_path":"/mnt/data"},{"name":"more","container_path":"/mnt/more"}]`,
			requestVolumes: []*types.Volume{{Name: "data"}, {Name: "more"}},
			wantErr:        true,
		},
		{
			name:           "missing volume is rejected",
			snapshotMounts: `[{"name":"data","container_path":"/mnt/data"},{"name":"more","container_path":"/mnt/more"}]`,
			requestMounts:  `[{"name":"data","container_path":"/mnt/data"}]`,
			requestVolumes: []*types.Volume{{Name: "data"}},
			wantErr:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateReq := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{},
				Volumes:     []*types.Volume{{Name: "data"}},
			}
			if tt.snapshotMounts != "" {
				templateReq.Annotations[sandbox.AnnotationPluginVolumeMounts] = tt.snapshotMounts
			}
			req := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{},
				Volumes:     tt.requestVolumes,
			}
			if tt.requestMounts != "" {
				req.Annotations[sandbox.AnnotationPluginVolumeMounts] = tt.requestMounts
			}

			err := reconcileSnapshotPluginVolumes("snap-1", req, templateReq)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "must match the snapshot's volume mounts")
				return
			}
			require.NoError(t, err)
			names := make([]string, 0, len(req.Volumes))
			for _, volume := range req.Volumes {
				names = append(names, volume.Name)
			}
			assert.Equal(t, tt.wantVolumes, names)
			_, hasAnnotation := req.Annotations[sandbox.AnnotationPluginVolumeMounts]
			assert.Equal(t, tt.wantAnnotation, hasAnnotation)
		})
	}
}

// emptyDirVolume builds a sandbox-local volume, the kind a filesystem-only
// template keeps: it is created per sandbox and carries no node dependency.
func emptyDirVolume(name string) *types.Volume {
	return &types.Volume{
		Name:         name,
		VolumeSource: &types.VolumeSource{EmptyDir: &types.EmptyDirVolumeSource{}},
	}
}

func hostDirVolume(name, hostPath string) *types.Volume {
	return &types.Volume{
		Name: name,
		VolumeSource: &types.VolumeSource{
			HostDirVolumeSources: &types.HostDirVolumeSources{
				VolumeSources: []*types.HostDirSource{{Name: name, HostPath: hostPath}},
			},
		},
	}
}

func mountNames(ctr *types.Container) []string {
	names := make([]string, 0, len(ctr.VolumeMounts))
	for _, mount := range ctr.VolumeMounts {
		names = append(names, mount.GetName())
	}
	return names
}

// A filesystem-only snapshot carries the rootfs only, so every external mount
// the origin sandbox recorded must be dropped: the volumes, the container
// mounts that reference them and the annotations that would re-inject them.
// Dropping only some of the three leaves the request with dangling references
// or a mount the cold-booted guest never gets.
func TestStripSnapshotExternalMountsStripsOnlyTheTemplateSide(t *testing.T) {
	templateReq := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			sandbox.AnnotationPluginVolumeMounts:  `[{"name":"data","container_path":"/mnt/data"},{"name":"ghost","container_path":"/mnt/ghost"}]`,
			sandbox.AnnotationPluginVolumeSources: `[{"name":"data","plugin":"p1"}]`,
			sandbox.AnnotationHostDirMount:        `[{"hostPath":"/data/shared","mountPath":"/mnt/shared"}]`,
		},
		Volumes: []*types.Volume{
			{Name: "data"},   // declared by the annotation
			{Name: "legacy"}, // no source at all: a plugin volume too
			hostDirVolume("hostdir-0", "/data/shared"),
			emptyDirVolume("scratch"),
		},
		Containers: []*types.Container{{
			VolumeMounts: []*cubeboxv1.VolumeMounts{
				{Name: "data", ContainerPath: "/mnt/data"},
				{Name: "hostdir-0", ContainerPath: "/mnt/shared"},
				{Name: "ghost", ContainerPath: "/mnt/ghost"}, // declared, volume absent
				{Name: "scratch", ContainerPath: "/scratch"},
			},
		}},
	}
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			sandbox.AnnotationPluginVolumeMounts: `[{"name":"mine","container_path":"/mnt/mine"}]`,
		},
		Volumes: []*types.Volume{{Name: "mine"}},
	}

	stripSnapshotExternalMounts(templateReq)

	assert.Equal(t, []string{"scratch"}, volumeNames(templateReq.Volumes))
	assert.Equal(t, []string{"scratch"}, mountNames(templateReq.Containers[0]),
		"container mounts of stripped volumes must not survive as dangling references")
	for _, key := range []string{
		sandbox.AnnotationPluginVolumeMounts,
		sandbox.AnnotationPluginVolumeSources,
		sandbox.AnnotationHostDirMount,
	} {
		_, ok := templateReq.Annotations[key]
		assert.False(t, ok, "%s must be stripped from the template request", key)
	}
	// The caller's own mounts are untouched: that verdict belongs to the
	// cold-boot check, not to the template strip.
	assert.Equal(t, []string{"mine"}, volumeNames(req.Volumes))
	assert.Contains(t, req.Annotations, sandbox.AnnotationPluginVolumeMounts)
}

func volumeNames(volumes []*types.Volume) []string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	return names
}

func TestBindSnapshotCreateReplicaHostMountFailsWithoutOriginMetadata(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origReplica := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		resolveSnapshotReadyReplicaFn = origReplica
	})
	getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
		return nil, templatecenter.ErrTemplateStoreNotInitialized
	}
	resolveSnapshotReadyReplicaFn = func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
		t.Fatal("host-mount restore must not use an unpinned legacy replica")
		return templatecenter.ReplicaStatus{}, nil
	}
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		sandbox.AnnotationHostDirMount: `[{"hostPath":"/data/shared","mountPath":"/mnt"}]`,
	}}

	err := bindSnapshotCreateReplica(context.Background(), "snap-1", req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "with host mount requires origin restore metadata")
}

func TestBindSnapshotCreateReplicaKeepsOriginWhenPlacementSaysOrigin(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origDecide := decideRestorePlacementFn
	origReplica := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		decideRestorePlacementFn = origDecide
		resolveSnapshotReadyReplicaFn = origReplica
	})
	getSnapshotRestoreSourceFn = func(ctx context.Context, snapshotID string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{
			SnapshotID:   snapshotID,
			Backend:      constants.SnapshotBackendS3,
			RemoteStatus: constants.RemoteStatusReady,
			OriginNodeID: "node-a",
			OriginNodeIP: "10.0.0.1",
			ExportUUIDs:  `{"rootfs":"r1"}`,
		}, nil
	}
	decideRestorePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		return &restoreplace.Placement{NodeID: "node-a", NodeIP: "10.0.0.1", CrossNode: false}, nil
	}
	resolveSnapshotReadyReplicaFn = func(ctx context.Context, snapshotID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		assert.Equal(t, "node-a", preferredNodeID)
		return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
	}

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}}
	err := bindSnapshotCreateReplica(context.Background(), "snap-1", req)
	if err != nil {
		t.Fatalf("bindSnapshotCreateReplica failed: %v", err)
	}
	assert.Equal(t, []string{"node-a"}, req.DistributionScope)
	assert.Empty(t, req.Annotations[constants.CubeAnnotationSnapshotAllowNonLocal])
	assert.Equal(t, `{"rootfs":"r1"}`, req.Annotations[constants.CubeAnnotationSnapshotRemoteUUIDs])
}

func TestBindSnapshotCreateReplicaXFSIgnoresExportUUIDs(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	origSource := getSnapshotRestoreSourceFn
	origDecide := decideRestorePlacementFn
	origReplica := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		getSnapshotRestoreSourceFn = origSource
		decideRestorePlacementFn = origDecide
		resolveSnapshotReadyReplicaFn = origReplica
	})
	getSnapshotRestoreSourceFn = func(ctx context.Context, snapshotID string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{
			SnapshotID:   snapshotID,
			Backend:      constants.SnapshotBackendXFS,
			OriginNodeID: "node-a",
			OriginNodeIP: "10.0.0.1",
			ExportUUIDs:  `{"rootfs":"should-not-propagate"}`,
		}, nil
	}
	decideRestorePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		t.Fatal("XFS FromSnap must not call Decide")
		return nil, fmt.Errorf("Decide must not run")
	}
	resolveSnapshotReadyReplicaFn = func(ctx context.Context, snapshotID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
	}

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}}
	if err := bindSnapshotCreateReplica(context.Background(), "snap-1", req); err != nil {
		t.Fatalf("bindSnapshotCreateReplica failed: %v", err)
	}
	assert.Equal(t, []string{"node-a"}, req.DistributionScope)
	assert.Empty(t, req.Annotations[constants.CubeAnnotationSnapshotRemoteUUIDs])
	assert.Empty(t, req.Annotations[constants.CubeAnnotationSnapshotAllowNonLocal])
}

func TestBindSnapshotCreateReplicaRejectsTombstone(t *testing.T) {
	orig := ensureSnapshotReadyForNewUseFn
	t.Cleanup(func() { ensureSnapshotReadyForNewUseFn = orig })
	ensureSnapshotReadyForNewUseFn = func(ctx context.Context, snapshotID string) error {
		return fmt.Errorf("%w: %s", templatecenter.ErrSnapshotNotFound, snapshotID)
	}

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}}
	err := bindSnapshotCreateReplica(context.Background(), "snap-deleted", req)
	if !errors.Is(err, templatecenter.ErrSnapshotNotFound) {
		t.Fatalf("error = %v, want ErrSnapshotNotFound", err)
	}
}

// TestBindAppSnapshotTemplateReplicaRequiresReadyReplica verifies that even
// though no physical refs are written, the replica resolution still runs as
// a fail-fast gate (no ready replica -> error).
func TestBindAppSnapshotTemplateReplicaRequiresReadyReplica(t *testing.T) {
	origResolveTemplateReadyReplicaFn := resolveTemplateReadyReplicaFn
	t.Cleanup(func() {
		resolveTemplateReadyReplicaFn = origResolveTemplateReadyReplicaFn
	})
	resolveTemplateReadyReplicaFn = func(ctx context.Context, templateID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		return templatecenter.ReplicaStatus{}, assert.AnError
	}

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}}
	err := bindAppSnapshotTemplateReplica(context.Background(), "tpl-missing", req, false)
	assert.Error(t, err)
}

// A shared jfs template has one replica, on whichever node built it; the
// create may land on any node, scoped or not.
func TestBindAppSnapshotTemplateReplicaSharedTemplateIgnoresReplicaNode(t *testing.T) {
	origResolveTemplateReadyReplicaFn := resolveTemplateReadyReplicaFn
	t.Cleanup(func() {
		resolveTemplateReadyReplicaFn = origResolveTemplateReadyReplicaFn
	})
	var preferred []string
	resolveTemplateReadyReplicaFn = func(ctx context.Context, templateID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		preferred = append(preferred, preferredNodeID)
		return templatecenter.ReplicaStatus{NodeID: "builder", AgentVersion: "v1"}, nil
	}

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}, DistributionScope: []string{"node-c"}}
	assert.NoError(t, bindAppSnapshotTemplateReplica(context.Background(), "tpl-1", req, true))
	assert.Equal(t, []string{""}, preferred)
	assert.Equal(t, "true", req.Annotations[constants.CubeAnnotationSnapshotAllowNonLocal])
	assert.Equal(t, "v1", req.Annotations[constants.CubeAnnotationComponentCubeAgentVersion])

	assert.True(t, sharedJFSTemplate(&types.CreateCubeSandboxReq{Backend: "jfs"}, templatecenter.TemplateKindTemplate))
	assert.False(t, sharedJFSTemplate(&types.CreateCubeSandboxReq{Backend: "jfs"}, templatecenter.TemplateKindSnapshot))
	assert.False(t, sharedJFSTemplate(&types.CreateCubeSandboxReq{Backend: "xfs"}, templatecenter.TemplateKindTemplate))
}

func TestCompatibleNodesBySnapshot(t *testing.T) {
	origFn := listCompatibleNodesForSnapshotFn
	t.Cleanup(func() { listCompatibleNodesForSnapshotFn = origFn })
	listCompatibleNodesForSnapshotFn = func(ctx context.Context, snapshotID string, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		assert.Equal(t, "snap-1", snapshotID)
		assert.False(t, includeAll)
		return &templatecenter.CompatibleNodesResult{
			SnapshotID: snapshotID,
			OriginNode: "node-a",
			Nodes:      []templatecenter.CompatibleNode{{NodeID: "node-b", Compatible: true}},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/snap-1/compatible-nodes?request_id=req-1", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "snap-1").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "snap-1", got.SnapshotID)
	assert.Equal(t, "node-a", got.OriginNode)
	if assert.Len(t, got.Nodes, 1) {
		assert.Equal(t, "node-b", got.Nodes[0].NodeID)
	}
}

func TestCompatibleNodesByFactors(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		assert.Equal(t, "sha256:cpu", factors.CPUIDHash)
		assert.Equal(t, "5.15.0", factors.HostKernelRelease)
		assert.True(t, includeAll)
		return &templatecenter.CompatibleNodesResult{
			Nodes: []templatecenter.CompatibleNode{{NodeID: "node-b", Compatible: true}},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/ignored/compatible-nodes?cpuid_hash=sha256:cpu&host_kernel_release=5.15.0&include_all=true", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "ignored").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Len(t, got.Nodes, 1)
}

// The dedicated /compatible-nodes-by-factors route serves the bare-factor mode
// without a dummy snapshot_id segment.
func TestCompatibleNodesByFactorsDedicatedRoute(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		assert.Equal(t, "sha256:cpu", factors.CPUIDHash)
		assert.Equal(t, "5.15.0", factors.HostKernelRelease)
		return &templatecenter.CompatibleNodesResult{
			Nodes: []templatecenter.CompatibleNode{{NodeID: "node-b", Compatible: true}},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/compatible-nodes-by-factors?cpuid_hash=sha256:cpu&host_kernel_release=5.15.0", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodesByFactors(req, rt).(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Len(t, got.Nodes, 1)
}

// The dedicated route rejects an under-specified query the same way the shared
// mode does, without reaching the listing fn.
func TestCompatibleNodesByFactorsDedicatedRouteUnderspecified(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	called := false
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		called = true
		return &templatecenter.CompatibleNodesResult{}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/compatible-nodes-by-factors?cpuid_hash=sha256:cpu", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodesByFactors(req, rt).(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.False(t, called, "factors fn must not be called for under-specified query")
}

func TestCompatibleNodesMissingParams(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot//compatible-nodes", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestCompatibleNodesFactorsErrorMapsToParamsError(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		return nil, templatecenter.ErrRestoreCompatNoFactors
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/ignored/compatible-nodes?cpuid_hash=x&host_kernel_release=5.15.0", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "ignored").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
}

// A bare-factor query supplying only one of the two required factors must be
// rejected with a params error rather than silently returning an empty list
// (the missing required dimension would otherwise fail closed against every
// candidate). The factors fn must never be reached.
func TestCompatibleNodesBareFactorUnderspecified(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	called := false
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		called = true
		return &templatecenter.CompatibleNodesResult{}, nil
	}

	for _, q := range []string{"cpuid_hash=sha256:cpu", "host_kernel_release=5.15.0"} {
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/ignored/compatible-nodes?"+q, nil)
		rt := &CubeLog.RequestTrace{}
		got := compatibleNodes(req, rt, "ignored").(*compatibleNodesResponse)

		assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
		assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
	}
	assert.False(t, called, "factors fn must not be called for under-specified query")
}

func TestSnapshotColdReplicaDropsCallerShimOverride(t *testing.T) {
	stubSnapshotReadyForNewUse(t)
	previousSource, previousReplica := getSnapshotRestoreSourceFn, resolveSnapshotReadyReplicaFn
	t.Cleanup(func() { getSnapshotRestoreSourceFn = previousSource; resolveSnapshotReadyReplicaFn = previousReplica })
	getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
		return &templatecenter.RestoreSource{SnapshotID: "snap-cold", Backend: constants.SnapshotBackendXFS, OriginNodeID: "node-a", OriginNodeIP: "10.0.0.1"}, nil
	}
	resolveSnapshotReadyReplicaFn = func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
		return templatecenter.ReplicaStatus{}, errors.New("cold replica cache")
	}
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{constants.CubeAnnotationComponentCubeShimVersion: "caller-shim"}}
	require.NoError(t, bindSnapshotCreateReplica(context.Background(), "snap-cold", req))
	assert.NotContains(t, req.Annotations, constants.CubeAnnotationComponentCubeShimVersion)
	assert.Equal(t, "snap-cold", req.Annotations[constants.CubeAnnotationRuntimeSnapshotID])
}

// CubeAPI reads the ret envelope from the top level of the body, exactly like
// every other /cube route. A nested {"res":{...}} would hide a non-success ret
// and be read as "no modes supported" instead of as an error.
func TestCapabilitiesResponseKeepsTheFlatEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/cube/capabilities?request_id=req-1", nil)
	ctx.Request = ctx.Request.WithContext(
		CubeLog.WithRequestTrace(ctx.Request.Context(), &CubeLog.RequestTrace{}),
	)

	handleCapabilitiesAction(ctx)

	body := recorder.Body.String()
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, body, `"ret"`, "ret must be at the top level")
	assert.NotContains(t, body, `"res"`, "the envelope must not be nested under res")
	assert.Contains(t, body, `"snapshot_modes":["full","fs-only"]`,
		"the contract is the set of modes this build accepts")
	assert.NotContains(t, body, `"filesystem_only_snapshot"`,
		"a feature switch is not part of the contract; the build supports both modes")
	assert.Contains(t, body, `"requestID":"req-1"`)
}
