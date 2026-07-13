// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	createSnapshotFn       = templatecenter.SubmitSandboxSnapshot
	getSnapshotInfoFn      = templatecenter.GetSnapshotInfo
	listSnapshotsFn        = templatecenter.ListSnapshots
	listSnapshotStorageFn  = templatecenter.ListSnapshotStorageStatus
	deleteSnapshotFn       = templatecenter.DeleteSnapshot
	rollbackSnapshotFn     = templatecenter.RollbackSandboxToSnapshot
	getSnapshotOperationFn = templatecenter.GetSnapshotOperation
	resolveSnapshotHostFn  = resolveSandboxHost
)

const snapshotResponseWriteDeadlineBuffer = 30 * time.Second

// snapshotCreateRequest is the public HTTP body for "snapshot a running
// sandbox". After the Phase 4 hard break the caller only supplies identifiers
// and the optional display name; the canonical create-time spec is loaded
// internally from sandboxspec (Phase 1).
type snapshotCreateRequest struct {
	RequestID       string `json:"request_id,omitempty"`
	LegacyRequestID string `json:"requestID,omitempty"`
	SandboxID       string `json:"sandbox_id,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
}

type snapshotRollbackRequest struct {
	RequestID       string `json:"request_id,omitempty"`
	LegacyRequestID string `json:"requestID,omitempty"`
	SandboxID       string `json:"sandbox_id,omitempty"`
	SnapshotID      string `json:"snapshot_id,omitempty"`
	InstanceType    string `json:"instance_type,omitempty"`
}

type snapshotDeleteRequest struct {
	RequestID       string `json:"request_id,omitempty"`
	LegacyRequestID string `json:"requestID,omitempty"`
	InstanceType    string `json:"instance_type,omitempty"`
}

type snapshotResponse struct {
	*types.Res
	Snapshot  *snapshotResource  `json:"snapshot,omitempty"`
	Operation *operationResource `json:"operation,omitempty"`
}

type snapshotListResponse struct {
	*types.Res
	Data      []*snapshotResource `json:"data,omitempty"`
	NextToken string              `json:"next_token,omitempty"`
}

type snapshotStorageResponse struct {
	*types.Res
	Data []*snapshotStorageResource `json:"data,omitempty"`
}

type operationResponse struct {
	*types.Res
	Operation *operationResource `json:"operation,omitempty"`
}

type snapshotResource struct {
	SnapshotID                string                         `json:"snapshot_id,omitempty"`
	InstanceType              string                         `json:"instance_type,omitempty"`
	Version                   string                         `json:"version,omitempty"`
	Status                    string                         `json:"status,omitempty"`
	DisplayName               string                         `json:"display_name,omitempty"`
	OriginSandboxID           string                         `json:"origin_sandbox_id,omitempty"`
	OriginNodeID              string                         `json:"origin_node_id,omitempty"`
	StorageBackend            string                         `json:"storage_backend,omitempty"`
	Retain                    bool                           `json:"retain,omitempty"`
	RootfsSizeBytesAtSnapshot uint64                         `json:"rootfs_size_bytes_at_snapshot,omitempty"`
	LastError                 string                         `json:"last_error,omitempty"`
	CreatedAt                 string                         `json:"created_at,omitempty"`
	RuntimeRefCount           int64                          `json:"runtime_ref_count,omitempty"`
	RuntimeRefSandboxes       []string                       `json:"runtime_ref_sandboxes,omitempty"`
	Replicas                  []templatecenter.ReplicaStatus `json:"replicas,omitempty"`
	CreateRequest             *types.CreateCubeSandboxReq    `json:"create_request,omitempty"`
}

type operationResource struct {
	OperationID  string `json:"operation_id,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	SandboxID    string `json:"sandbox_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Status       string `json:"status,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Progress     int32  `json:"progress,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	AttemptNo    int32  `json:"attempt_no,omitempty"`
	RetryOfJobID string `json:"retry_of_job_id,omitempty"`
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
}

type snapshotStorageResource struct {
	NodeID        string `json:"node_id,omitempty"`
	NodeIP        string `json:"node_ip,omitempty"`
	UsagePct      uint64 `json:"usage_pct,omitempty"`
	Mode          string `json:"mode,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUpdatedAt int64  `json:"last_updated_at,omitempty"`
}

func handleSnapshotAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	switch r.Method {
	case http.MethodPost:
		extendSnapshotWriteDeadline(w)
		return createSnapshot(r, rt)
	case http.MethodGet:
		return getSnapshot(r, rt)
	case http.MethodDelete:
		extendSnapshotWriteDeadline(w)
		return deleteSnapshot(r, rt)
	default:
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}
}

func handleSnapshotStorageAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	if r.Method != http.MethodGet {
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}
	refresh := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("refresh")), "true") ||
		strings.TrimSpace(r.URL.Query().Get("refresh")) == "1"
	data, err := listSnapshotStorageFn(r.Context(), refresh)
	response := &snapshotStorageResponse{
		Res: &types.Res{
			RequestID: requestIDFromQuery(r),
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
		Data: make([]*snapshotStorageResource, 0, len(data)),
	}
	for i := range data {
		response.Data = append(response.Data, snapshotStorageResourceFromStatus(data[i]))
	}
	if err != nil {
		response.Res.Ret.RetMsg = err.Error()
	}
	rt.RequestID = response.Res.RequestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return response
}

func handleSandboxRollbackAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	extendSnapshotWriteDeadline(w)
	req := &snapshotRollbackRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		return &operationResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: err.Error()}},
		}
	}
	requestID := firstNonEmptyTrimmed(req.RequestID, req.LegacyRequestID)
	pathSandboxID := sandboxIDFromRollbackPath(r.URL.Path)
	if req.SandboxID == "" {
		req.SandboxID = pathSandboxID
	}
	if pathSandboxID != "" && strings.TrimSpace(req.SandboxID) != pathSandboxID {
		return &operationResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "sandbox_id in path does not match request body",
			}},
		}
	}
	if requestID == "" || strings.TrimSpace(req.SandboxID) == "" || strings.TrimSpace(req.SnapshotID) == "" {
		return &operationResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "request_id, sandbox_id and snapshot_id are required",
			}},
		}
	}
	ctx, cancel := snapshotExecutionContext(r.Context(), map[string]any{
		"RequestId":  requestID,
		"Action":     "RollbackSnapshot",
		"SnapshotID": req.SnapshotID,
		"SandboxID":  req.SandboxID,
	})
	defer cancel()
	info, err := rollbackSnapshotFn(ctx, requestID, req.SandboxID, req.SnapshotID, req.InstanceType)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &operationResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &operationResponse{
		Res: &types.Res{
			RequestID: requestID,
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
		Operation: operationResourceFromInfo(snapshotOperationInfoFromJob(info)),
	}
}

func extendSnapshotWriteDeadline(w http.ResponseWriter) {
	deadline := time.Now().Add(templatecenter.SnapshotOperationTimeout() + snapshotResponseWriteDeadlineBuffer)
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.G(context.Background()).Warnf("set snapshot response write deadline failed: %v", err)
	}
}

func snapshotExecutionContext(parent context.Context, fields map[string]any) (context.Context, context.CancelFunc) {
	base := context.Background()
	if rt := CubeLog.GetTraceInfo(parent); rt != nil {
		base = CubeLog.WithRequestTrace(base, rt.DeepCopy())
	}
	return context.WithTimeout(log.WithLogger(base, log.G(parent).WithFields(fields)), templatecenter.SnapshotOperationTimeout())
}

func handleSnapshotOperationAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	if r.Method != http.MethodGet {
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}
	operationID := resourceIDFromPath(r.URL.Path, actionURI(OperationAction))
	requestID := requestIDFromQuery(r)
	if operationID == "" {
		return &operationResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "operation_id is required",
			}},
		}
	}
	info, err := getSnapshotOperationFn(r.Context(), operationID)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &operationResponse{
			Res: &types.Res{RequestID: requestID, Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
		}
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &operationResponse{
		Res:       &types.Res{RequestID: requestID, Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"}},
		Operation: operationResourceFromInfo(info),
	}
}

func createSnapshot(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req := &snapshotCreateRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: err.Error()}},
		}
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "sandbox_id is required",
			}},
		}
	}
	requestID := firstNonEmptyTrimmed(req.RequestID, req.LegacyRequestID)
	if requestID == "" {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "request_id is required",
			}},
		}
	}
	ctx, cancel := snapshotExecutionContext(r.Context(), map[string]any{
		"RequestId": requestID,
		"Action":    "CreateSnapshot",
		"SandboxID": req.SandboxID,
	})
	defer cancel()
	hostID, hostIP, err := resolveSnapshotHostFn(ctx, requestID, req.SandboxID)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &snapshotResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]any{
		"RequestId":   requestID,
		"SandboxHost": hostIP,
	}))
	info, err := createSnapshotFn(ctx, requestID, req.SandboxID, hostID, hostIP, req.DisplayName)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &snapshotResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	snapshotInfo, err := getSnapshotInfoFn(ctx, info.TemplateID, false)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &snapshotResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &snapshotResponse{
		Res: &types.Res{
			RequestID: requestID,
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
		Snapshot:  snapshotResourceFromInfo(snapshotInfo),
		Operation: operationResourceFromInfo(snapshotOperationInfoFromJob(info)),
	}
}

func getSnapshot(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	snapshotID := resourceIDFromPath(r.URL.Path, actionURI(SnapshotAction))
	requestID := requestIDFromQuery(r)
	if snapshotID == "" {
		infos, nextToken, err := listSnapshotsFn(r.Context(), &templatecenter.ListSnapshotsOptions{
			SnapshotID: strings.TrimSpace(r.URL.Query().Get("snapshot_id")),
			SandboxID:  strings.TrimSpace(r.URL.Query().Get("sandbox_id")),
			Name:       strings.TrimSpace(r.URL.Query().Get("name")),
			Status:     strings.TrimSpace(r.URL.Query().Get("status")),
			Limit:      parsePositiveIntQuery(r, "limit"),
			NextToken:  strings.TrimSpace(r.URL.Query().Get("next_token")),
		})
		if err != nil {
			code := snapshotErrorCode(err)
			rt.RetCode = int64(code)
			return &snapshotListResponse{
				Res: &types.Res{RequestID: requestID, Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
			}
		}
		rsp := &snapshotListResponse{
			Res:       &types.Res{RequestID: requestID, Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"}},
			Data:      make([]*snapshotResource, 0, len(infos)),
			NextToken: nextToken,
		}
		for i := range infos {
			item := infos[i]
			rsp.Data = append(rsp.Data, snapshotResourceFromInfo(&item))
		}
		rt.RequestID = requestID
		rt.RetCode = int64(errorcode.ErrorCode_Success)
		return rsp
	}
	includeRequest := r.URL.Query().Get("include_request") == "true" || r.URL.Query().Get("include_request") == "1"
	info, err := getSnapshotInfoFn(r.Context(), snapshotID, includeRequest)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &snapshotResponse{
			Res: &types.Res{RequestID: requestID, Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
		}
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &snapshotResponse{
		Res:      &types.Res{RequestID: requestID, Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"}},
		Snapshot: snapshotResourceFromInfo(info),
	}
}

// deleteSnapshot fronts `DELETE /cube/snapshot/{snapshot_id}` and is part of
// the *synchronous* snapshot contract: the HTTP response is not produced
// until the underlying snapshot-delete job has reached a terminal status
// (READY on success, FAILED on error).  Specifically:
//
//   - Inputs are validated, then `templatecenter.DeleteSnapshot` runs the
//     full delete pipeline (replica cleanup via cubelet, metadata removal,
//     cache invalidation) inside a context capped at
//     `templatecenter.SnapshotOperationTimeout()` (15 min).
//   - `extendSnapshotWriteDeadline(w)` widens the HTTP write deadline to
//     `SnapshotOperationTimeout + 30s` so the server-side write does not
//     fire before the synchronous wait completes.
//   - `executeSnapshotDeleteJob` -> `finalizeSynchronousSnapshotJob` only
//     returns a non-error `info` when the job row is `Ready`; any other
//     terminal state (`Failed`, missing) becomes an error and is mapped to
//     a 4xx/5xx ret code.  Pending/Running can never escape this handler.
//
// Callers may therefore treat a successful response as "snapshot definitely
// removed" and a non-success response as "the operation either ran to
// failure or was rejected before starting" — there is no middle "still
// running, please poll" outcome.  `GET /cube/operation/{operation_id}` is
// kept around purely for human audit; programmatic clients have no reason
// to poll.  The snapshot API is synchronous — CubeAPI waits for a
// terminal state and does not expose a polling interface to callers.
func deleteSnapshot(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	snapshotID := resourceIDFromPath(r.URL.Path, actionURI(SnapshotAction))
	if snapshotID == "" {
		return &operationResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "snapshot_id is required",
			}},
		}
	}
	deleteReq := &snapshotDeleteRequest{}
	if r.ContentLength > 0 {
		if err := common.GetBodyReq(r, deleteReq); err != nil {
			return &operationResponse{
				Res: &types.Res{Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				}},
			}
		}
	}
	requestID := firstNonEmptyTrimmed(deleteReq.RequestID, deleteReq.LegacyRequestID, requestIDFromQuery(r))
	if requestID == "" {
		return &operationResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "request_id is required",
			}},
		}
	}
	instanceType := firstNonEmptyTrimmed(deleteReq.InstanceType, r.URL.Query().Get("instance_type"))
	ctx, cancel := snapshotExecutionContext(r.Context(), map[string]any{
		"RequestId":  requestID,
		"Action":     "DeleteSnapshot",
		"SnapshotID": snapshotID,
	})
	defer cancel()
	info, err := deleteSnapshotFn(ctx, requestID, snapshotID, instanceType)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &operationResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &operationResponse{
		Res: &types.Res{
			RequestID: requestID,
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
		Operation: operationResourceFromInfo(snapshotOperationInfoFromJob(info)),
	}
}

func resolveSandboxHost(ctx context.Context, requestID, sandboxID string) (string, string, error) {
	hostIP := ""
	if cache := localcache.GetSandboxCache(sandboxID); cache != nil {
		hostIP = cache.HostIP
	}
	hostID := ""
	if hostIP == "" {
		infoRsp := sandbox.SandboxInfo(ctx, &types.GetCubeSandboxReq{
			RequestID: requestID,
			SandboxID: sandboxID,
		})
		if infoRsp == nil || infoRsp.Ret == nil || infoRsp.Ret.RetCode != int(errorcode.ErrorCode_Success) || len(infoRsp.Data) == 0 {
			msg := "sandbox not found"
			if infoRsp != nil && infoRsp.Ret != nil && infoRsp.Ret.RetMsg != "" {
				msg = infoRsp.Ret.RetMsg
			}
			return "", "", errors.New(msg)
		}
		hostIP = infoRsp.Data[0].HostIP
		hostID = infoRsp.Data[0].HostID
	}
	if hostID == "" && hostIP != "" {
		if n, ok := localcache.GetNodesByIp(hostIP); ok && n != nil {
			hostID = n.ID()
		}
	}
	if hostIP == "" || hostID == "" {
		return "", "", errors.New("unable to resolve sandbox host")
	}
	return hostID, hostIP, nil
}

func snapshotErrorCode(err error) int {
	switch {
	case err == nil:
		return int(errorcode.ErrorCode_Success)
	case isSnapshotConflictError(err):
		return int(errorcode.ErrorCode_Conflict)
	case errors.Is(err, templatecenter.ErrSnapshotNotFound),
		errors.Is(err, templatecenter.ErrSnapshotOperationNotFound),
		errors.Is(err, templatecenter.ErrTemplateNotFound):
		return int(errorcode.ErrorCode_NotFound)
	case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
		return int(errorcode.ErrorCode_DBError)
	case isMySQLLockError(err):
		return int(errorcode.ErrorCode_DBError)
	default:
		return int(errorcode.ErrorCode_MasterParamsError)
	}
}

func isMySQLLockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1205, 1213:
			return true
		}
	}
	message := err.Error()
	return strings.Contains(message, "Error 1213") ||
		strings.Contains(message, "Error 1205") ||
		strings.Contains(message, "Deadlock found when trying to get lock") ||
		strings.Contains(message, "Lock wait timeout exceeded")
}

func snapshotOperationInfoFromJob(info *types.TemplateImageJobInfo) *templatecenter.SnapshotOperationInfo {
	if info == nil {
		return nil
	}
	return &templatecenter.SnapshotOperationInfo{
		OperationID:  info.JobID,
		SnapshotID:   info.ResourceID,
		SandboxID:    info.SandboxID,
		RequestID:    info.RequestID,
		Operation:    info.Operation,
		Status:       info.Status,
		Phase:        info.Phase,
		Progress:     info.Progress,
		ErrorMessage: info.ErrorMessage,
		AttemptNo:    info.AttemptNo,
		RetryOfJobID: info.RetryOfJobID,
		ResourceType: info.ResourceType,
		ResourceID:   info.ResourceID,
	}
}

func snapshotResourceFromInfo(info *templatecenter.SnapshotInfo) *snapshotResource {
	if info == nil {
		return nil
	}
	return &snapshotResource{
		SnapshotID:                info.SnapshotID,
		InstanceType:              info.InstanceType,
		Version:                   info.Version,
		Status:                    info.Status,
		DisplayName:               info.DisplayName,
		OriginSandboxID:           info.OriginSandboxID,
		OriginNodeID:              info.OriginNodeID,
		StorageBackend:            info.StorageBackend,
		Retain:                    info.Retain,
		RootfsSizeBytesAtSnapshot: info.RootfsSizeBytesAtSnapshot,
		LastError:                 info.LastError,
		CreatedAt:                 info.CreatedAt,
		RuntimeRefCount:           info.RuntimeRefCount,
		RuntimeRefSandboxes:       append([]string(nil), info.RuntimeRefSandboxes...),
		Replicas:                  append([]templatecenter.ReplicaStatus(nil), info.Replicas...),
		CreateRequest:             info.CreateRequest,
	}
}

func operationResourceFromInfo(info *templatecenter.SnapshotOperationInfo) *operationResource {
	if info == nil {
		return nil
	}
	return &operationResource{
		OperationID:  info.OperationID,
		SnapshotID:   info.SnapshotID,
		SandboxID:    info.SandboxID,
		RequestID:    info.RequestID,
		Operation:    info.Operation,
		Status:       info.Status,
		Phase:        info.Phase,
		Progress:     info.Progress,
		ErrorMessage: info.ErrorMessage,
		AttemptNo:    info.AttemptNo,
		RetryOfJobID: info.RetryOfJobID,
		ResourceType: info.ResourceType,
		ResourceID:   info.ResourceID,
	}
}

func snapshotStorageResourceFromStatus(info templatecenter.SnapshotStorageStatus) *snapshotStorageResource {
	return &snapshotStorageResource{
		NodeID:        info.NodeID,
		NodeIP:        info.NodeIP,
		UsagePct:      info.UsagePct,
		Mode:          info.Mode,
		LastError:     info.LastError,
		LastUpdatedAt: info.LastUpdatedAt,
	}
}

func resourceIDFromPath(path, prefix string) string {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, prefix+"/") {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(path, prefix+"/"), "/")
}

func sandboxIDFromRollbackPath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 4 {
		return ""
	}
	if parts[0] != strings.Trim(CubeURI(), "/") || parts[1] != strings.Trim(SandboxAction, "/") || parts[3] != "rollback" {
		return ""
	}
	return strings.TrimSpace(parts[2])
}

func requestIDFromQuery(r *http.Request) string {
	if r == nil {
		return ""
	}
	return firstNonEmptyTrimmed(r.URL.Query().Get("request_id"), r.URL.Query().Get("requestID"))
}

func requestIDFromNestedRequest(req *types.Request) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(req.RequestID)
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parsePositiveIntQuery(r *http.Request, key string) int {
	if r == nil {
		return 0
	}
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

func isSnapshotConflictError(err error) bool {
	switch {
	case errors.Is(err, templatecenter.ErrTemplateAttemptInProgress),
		errors.Is(err, templatecenter.ErrTemplateInUse),
		errors.Is(err, templatecenter.ErrTemplateHasNoReadyReplica),
		errors.Is(err, templatecenter.ErrSnapshotReplicaMetadataIncomplete),
		errors.Is(err, templatecenter.ErrDuplicateTemplate):
		return true
	default:
		return false
	}
}
