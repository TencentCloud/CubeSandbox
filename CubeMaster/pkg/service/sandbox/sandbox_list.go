// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/api/resource"
)

func ListSandbox(ctx context.Context, req *types.ListCubeSandboxReq) (rsp *types.ListCubeSandboxRes) {
	return listSandbox(ctx, req, false)
}

// ListSandboxWithFailOnError surfaces cubelet list failures as a non-success
// ret_code instead of silently returning an empty list.
func ListSandboxWithFailOnError(ctx context.Context, req *types.ListCubeSandboxReq) (rsp *types.ListCubeSandboxRes) {
	return listSandbox(ctx, req, true)
}

func listSandbox(ctx context.Context, req *types.ListCubeSandboxReq, failOnCubeletError bool) (rsp *types.ListCubeSandboxRes) {
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}
	log.G(ctx).Infof("ListSandbox:%+v", utils.InterfaceToString(req))
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Debugf("ListSandbox_rsp:%+v", utils.InterfaceToString(rsp))
		} else if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("ListSandbox fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	rsp = &types.ListCubeSandboxRes{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},

		Total: localcache.GetHealthyNodesByInstanceType(-1, req.InstanceType).Len(),
	}
	// Inventory path: always emit data (even empty []) so callers can
	// distinguish "0 sandboxes" from "list did not complete".
	if failOnCubeletError {
		rsp.Data = []*types.SandboxBriefData{}
	}

	var nodeList []*node.Node
	if req.HostID != "" {
		tmpNode, ok := localcache.GetNode(req.HostID)
		if !ok {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_NotFound)
			rsp.Ret.RetMsg = errorcode.ErrorCode_NotFound.String()
			return
		}
		nodeList = append(nodeList, tmpNode)
	} else {
		if req.Size <= 0 || req.StartIdx < 0 {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = errorcode.ErrorCode_MasterParamsError.String()
			return
		}

		if req.StartIdx == 0 {
			req.StartIdx = 1
		}

		nodeList, rsp.EndIdx = localcache.RangeDBHost(req.StartIdx, req.Size, req.InstanceType)
	}

	if len(nodeList) == 0 {
		return
	}

	rsp.Size = len(nodeList)

	var resChan = make(chan *types.SandboxBriefData, 1000*len(nodeList))
	done := make(chan struct{})
	var listFailed atomic.Bool

	dealRspData(ctx, done, resChan, rsp)

	var wg sync.WaitGroup

	for _, tmpNode := range nodeList {
		tmpNode := tmpNode
		recov.GoWithWaitGroup(&wg, func() {
			doOneList(ctx, req, tmpNode, resChan, &listFailed)
		}, func(panicError interface{}) {
			log.G(ctx).Fatalf("panic:%v", string(debug.Stack()))
		})
	}
	wg.Wait()
	close(resChan)
	<-done

	if failOnCubeletError && listFailed.Load() {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = "list sandbox failed: cubelet unreachable"
	}

	// Results arrive over resChan in goroutine-completion order, which varies
	// between calls and across nodes, so rsp.Data would otherwise be reshuffled
	// on every list request. Sort by creation time descending (newest first),
	// falling back to SandboxID for a deterministic, stable order.
	sort.Slice(rsp.Data, func(i, j int) bool {
		if rsp.Data[i].CreateAt != rsp.Data[j].CreateAt {
			return rsp.Data[i].CreateAt > rsp.Data[j].CreateAt
		}
		return rsp.Data[i].SandboxID < rsp.Data[j].SandboxID
	})
	enrichSandboxListBackends(ctx, rsp.Data)
	mergePauseBindings(ctx, req, rsp)
	return
}

// mergePauseBindings adds Master's pause state to the node-scan result.
//
// A paused sandbox has no shim, so the node scan cannot be trusted to report
// it, and t_cube_pause_snapshot is the source of truth. Bindings are therefore
// read in full rather than per scanned node: the (StartIdx, Size) window only
// paginates node scanning. An explicit --hostid is still honoured, because it
// selects one node on purpose and `list --hostid X --quiet --delete` must not
// reach a sandbox parked on another node.
func mergePauseBindings(ctx context.Context, req *types.ListCubeSandboxReq, rsp *types.ListCubeSandboxRes) {
	records, err := pausesnap.List(ctx, pausesnap.ListOptions{
		HostID:       req.HostID,
		InstanceType: req.InstanceType,
	})
	if err != nil {
		if !errors.Is(err, pausesnap.ErrNotReady) {
			log.G(ctx).Warnf("ListSandbox: read pause bindings: %v", err)
		}
		return
	}
	rsp.Data = applyPauseBindings(rsp.Data, records, req.Filter != nil && len(req.Filter.LabelSelector) > 0)
}

func applyPauseBindings(items []*types.SandboxBriefData, records []*pausesnap.Record,
	labelFiltered bool) []*types.SandboxBriefData {
	known := make(map[string]*types.SandboxBriefData, len(items))
	for _, item := range items {
		if item != nil && item.SandboxID != "" {
			known[item.SandboxID] = item
		}
	}
	for _, rec := range records {
		if rec == nil || rec.SandboxID == "" {
			continue
		}
		if item, ok := known[rec.SandboxID]; ok {
			applyPauseBinding(item, rec)
			continue
		}
		// CREATING / FAILED leave the sandbox running, so the node scan owns
		// that row and its absence here means the sandbox is gone.
		// DELETE_FAILED has no shim either and its leftover package is exactly
		// what an operator needs to see, so it is rendered like READY.
		if !isShimlessPauseStatus(rec.Status) {
			continue
		}
		// Labels only exist on the node, so a label-filtered list cannot tell
		// whether this binding would have matched.
		if labelFiltered {
			continue
		}
		item := pauseBindingRow(rec)
		known[rec.SandboxID] = item
		items = append(items, item)
	}
	return items
}

func applyPauseBinding(item *types.SandboxBriefData, rec *pausesnap.Record) {
	if item == nil || rec == nil {
		return
	}
	item.PauseSnapshotID = rec.SnapshotID
	item.RemoteStatus = rec.RemoteStatus
	item.PauseStatus = strings.TrimSpace(rec.Status)
	if strings.TrimSpace(item.Backend) == "" {
		item.Backend = strings.TrimSpace(rec.Backend)
	}
}

// isShimlessPauseStatus reports whether the binding, not a node scan, is the
// only remaining evidence of the sandbox.
func isShimlessPauseStatus(status string) bool {
	status = strings.TrimSpace(status)
	return strings.EqualFold(status, pausesnap.StatusReady) ||
		strings.EqualFold(status, pausesnap.StatusDeleteFailed)
}

// pauseBindingRow renders a binding the node did not report. CreateAt stays
// empty: the binding only knows when the pause happened, not when the sandbox
// was created, and a wrong creation time is worse than none.
func pauseBindingRow(rec *pausesnap.Record) *types.SandboxBriefData {
	item := &types.SandboxBriefData{
		SandboxID:       rec.SandboxID,
		Status:          int32(cubebox.ContainerState_CONTAINER_PAUSED),
		HostID:          rec.NodeID,
		HostIP:          rec.NodeIP,
		Backend:         strings.TrimSpace(rec.Backend),
		PauseSnapshotID: rec.SnapshotID,
		RemoteStatus:    rec.RemoteStatus,
		PauseStatus:     strings.TrimSpace(rec.Status),
	}
	if !rec.UpdatedAt.IsZero() {
		item.PauseAt = rec.UpdatedAt.UnixNano()
	}
	return item
}

// enrichSandboxListBackends fills Backend from t_cube_sandbox_spec (DB source of
// truth for create-time xfs|s3). Missing specs leave Backend empty.
func enrichSandboxListBackends(ctx context.Context, items []*types.SandboxBriefData) {
	for _, item := range items {
		if item == nil || item.SandboxID == "" {
			continue
		}
		if b := strings.TrimSpace(item.Backend); b != "" {
			continue
		}
		if item.Annotations != nil {
			if b := strings.TrimSpace(item.Annotations[constants.CubeAnnotationStorageBackend]); b != "" {
				item.Backend = b
				continue
			}
		}
		spec, err := sandboxspec.Get(ctx, item.SandboxID)
		if err != nil || spec == nil {
			continue
		}
		item.Backend = strings.TrimSpace(spec.Backend)
	}
}

func dealRspData(ctx context.Context, done chan struct{}, resChan chan *types.SandboxBriefData,
	rsp *types.ListCubeSandboxRes) {
	recov.GoWithRecover(func() {
		defer close(done)
		for res := range resChan {
			select {
			case <-ctx.Done():
				return
			default:
			}
			rsp.Data = append(rsp.Data, res)
			if res.Status == int32(cubebox.ContainerState_CONTAINER_RUNNING) && !config.GetConfig().Common.EnabledListRunningSandboxCache {
				continue
			}
			localcache.SetSandboxCache(res.SandboxID, &localcache.SandboxCache{
				SandboxID: res.SandboxID,
				HostIP:    res.HostIP,
			})
		}
	}, func(panicError interface{}) {
		log.G(ctx).Fatalf("panic:%v", string(debug.Stack()))
	})
}

func doOneList(ctx context.Context, req *types.ListCubeSandboxReq, tmpNode *node.Node, resChan chan *types.SandboxBriefData, listFailed *atomic.Bool) {
	start := time.Now()
	rt := CubeLog.GetTraceInfo(ctx).DeepCopy()
	rt.Callee = constants.CubeLet
	rt.CalleeAction = "List"
	rt.RetCode = 200
	rt.CalleeEndpoint = cubelet.GetCubeletAddr(tmpNode.HostIP())
	defer func() {
		rt.Cost = time.Since(start)
		CubeLog.Trace(rt)
	}()

	cubeletReq := &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			LabelSelector: map[string]string{"io.kubernetes.cri.container-type": "sandbox"},
		},
	}

	if req.Filter != nil && req.Filter.LabelSelector != nil {
		for k, v := range req.Filter.LabelSelector {
			if k != "" && v != "" {
				cubeletReq.Filter.LabelSelector[k] = v
			}
		}
	}

	unlock := l.CubeletListLock.Lock(rt.CalleeEndpoint)
	defer unlock()

	cubeRsp, err := cubelet.List(ctx, rt.CalleeEndpoint, cubeletReq)
	if err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_ReqCubeAPIFailed)
		listFailed.Store(true)
		log.G(ctx).WithFields(map[string]interface{}{
			"CalleeEndpoint": rt.CalleeEndpoint,
		}).Errorf("List sandbox error:%v", err)
		return
	}

	for _, sandbox := range cubeRsp.GetItems() {
		sandboxLabels := cloneStringMap(sandbox.GetLabels())
		for _, container := range sandbox.GetContainers() {
			if container.GetType() == "sandbox" {
				if matchFilter(container.GetLabels()) {
					continue
				}
				labels := sandboxViewLabels(sandboxLabels, container.GetLabels())
				templateID := templateIDFromLabels(labels)
				select {
				case <-ctx.Done():
					return
				case resChan <- &types.SandboxBriefData{
					SandboxID:   sandbox.GetId(),
					HostID:      tmpNode.InsID,
					Status:      int32(container.GetState()),
					HostIP:      tmpNode.HostIP(),
					TemplateID:  templateID,
					CpuCount:    parseCPUCount(container.GetResources().GetCpu()),
					MemoryMB:    parseMemoryMB(container.GetResources().GetMem()),
					Annotations: buildAnnotationsFromLabels(labels),
					Labels:      labels,
					NameSpace:   sandbox.GetNamespace(),
					CreateAt:    sandbox.GetCreatedAt(),
					PauseAt:     container.GetPausedAt(),
					EndAt:       LookupSandboxEndAt(ctx, sandbox.GetId()),
					VolumeMounts: volumeMountsToContainerInfo(
						collectVolumeMountsFromContainers(sandbox.GetContainers())),
				}:
				}
				break
			}
		}
	}
}

func matchFilter(labels map[string]string) bool {
	tmpFilter := config.GetConfig().Common.ListFilterOutLables
	if len(tmpFilter) == 0 || len(labels) == 0 {
		return false
	}

	for k, v := range tmpFilter {
		if m, ok := labels[k]; ok && m == v {
			return true
		}
	}
	return false
}

func parseInt32(raw string) int32 {
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0
	}
	return int32(value)
}

func parseCPUCount(raw string) int32 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}
	if strings.HasSuffix(value, "m") {
		return parseInt32(strings.TrimSuffix(value, "m")) / 1000
	}
	return parseInt32(value)
}

func parseMemoryMB(raw string) int32 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}

	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	const maxInt32 = int64(1<<31 - 1)
	memoryMB := quantity.ScaledValue(resource.Mega)
	if memoryMB > maxInt32 {
		return int32(maxInt32)
	}
	return int32(memoryMB)
}
