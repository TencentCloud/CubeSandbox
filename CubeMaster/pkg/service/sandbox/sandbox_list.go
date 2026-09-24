// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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

	rsp.Size = len(nodeList)
	if len(nodeList) > 0 {
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
	}

	enrichSandboxListBackends(ctx, rsp.Data)
	mergePauseBindings(ctx, req, rsp)
	enrichSandboxListEndAts(ctx, rsp.Data)
	types.SortSandboxList(rsp.Data)
	return
}

// mergePauseBindings adds Master's pause state to the node-scan result.
//
// A paused sandbox has no shim, so the node scan cannot be trusted to report
// it, and t_cube_pause_snapshot is the source of truth for pause state.
// Scanned rows are always enriched. Shimless READY／DELETE_FAILED rows that
// the scan missed are appended only on the last node page (or when --hostid
// selects one node). Their identity comes from the persisted create spec;
// a label selector is matched against those labels.
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
	opts := pauseBindingMerge{appendMissing: shouldAppendShimlessPauseRows(req, rsp)}
	if req != nil && req.Filter != nil {
		opts.labelSelector = req.Filter.LabelSelector
	}
	if opts.appendMissing {
		if ids := missingShimlessBindingIDs(rsp.Data, records); len(ids) > 0 {
			specs, specErr := sandboxspec.GetMany(ctx, ids)
			if specErr != nil && !errors.Is(specErr, sandboxspec.ErrSandboxSpecStoreNotReady) {
				log.G(ctx).Warnf("ListSandbox: read sandbox specs: %v", specErr)
			}
			opts.specs = specs
		}
	}
	rsp.Data = applyPauseBindings(ctx, rsp.Data, records, opts)
}

// shouldAppendShimlessPauseRows is true for a single-host list and for the
// last node-window page (EndIdx >= Total, or no healthy nodes). Intermediate
// pages — including an empty window past the last node — only decorate rows
// the scan already returned.
func shouldAppendShimlessPauseRows(req *types.ListCubeSandboxReq, rsp *types.ListCubeSandboxRes) bool {
	if req != nil && strings.TrimSpace(req.HostID) != "" {
		return true
	}
	if rsp == nil {
		return false
	}
	if rsp.Total <= 0 {
		return true
	}
	return rsp.EndIdx >= rsp.Total
}

// pauseBindingMerge is the extra input applyPauseBindings needs beyond the
// node scan: specs for shimless rows, and whether this page may append them.
type pauseBindingMerge struct {
	specs         map[string]*types.CreateCubeSandboxReq
	labelSelector map[string]string
	appendMissing bool
}

func applyPauseBindings(ctx context.Context, items []*types.SandboxBriefData, records []*pausesnap.Record,
	opts pauseBindingMerge) []*types.SandboxBriefData {
	known := indexBriefs(items)
	for _, rec := range records {
		if rec == nil || rec.SandboxID == "" {
			continue
		}
		if item, ok := known[rec.SandboxID]; ok {
			// Pause fields are filled even when the node says RUNNING. PauseStatus
			// stays READY so an operator can see the stale binding; Status itself
			// stays RUNNING. Info has no PauseStatus field and does not add a
			// pause annotation in that case.
			applyPauseBinding(item, rec)
			view := decidePauseView(rec, item.Status, true)
			switch {
			case view.stale:
				recordStalePauseBinding(ctx, "list", rec.SandboxID, rec)
			case view.override:
				item.Status = view.status
				item.Annotations = overlayPauseAnnotations(item.Annotations, rec, view.pauseErr)
			default:
				// Node status stands, but the binding still names the pause snapshot.
				item.Annotations = overlayPauseAnnotations(item.Annotations, rec, "")
			}
			continue
		}
		// CREATING / FAILED leave the shim running, so a row the scan missed
		// is a sandbox that is gone. DELETE_FAILED has no shim either.
		if !opts.appendMissing || !isShimlessPauseStatus(rec.Status) {
			continue
		}
		spec := opts.specs[rec.SandboxID]
		item := briefFromPauseBinding(rec, spec)
		if item == nil || !labelsMatchSelector(item.Labels, opts.labelSelector) || matchFilter(item.Labels) {
			continue
		}
		item.Annotations = overlayPauseAnnotations(item.Annotations, rec, "")
		known[rec.SandboxID] = item
		items = append(items, item)
	}
	return items
}

func indexBriefs(items []*types.SandboxBriefData) map[string]*types.SandboxBriefData {
	known := make(map[string]*types.SandboxBriefData, len(items))
	for _, item := range items {
		if item != nil && item.SandboxID != "" {
			known[item.SandboxID] = item
		}
	}
	return known
}

// labelsMatchSelector mirrors how doOneList forwards the selector to Cubelet:
// pairs with an empty key or value are ignored.
func labelsMatchSelector(labels, selector map[string]string) bool {
	for k, v := range selector {
		if k == "" || v == "" {
			continue
		}
		if labels[k] != v {
			return false
		}
	}
	return true
}

func missingShimlessBindingIDs(items []*types.SandboxBriefData, records []*pausesnap.Record) []string {
	known := indexBriefs(items)
	var ids []string
	for _, rec := range records {
		if rec == nil || rec.SandboxID == "" || !isShimlessPauseStatus(rec.Status) {
			continue
		}
		if _, ok := known[rec.SandboxID]; !ok {
			ids = append(ids, rec.SandboxID)
		}
	}
	return ids
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

func enrichSandboxListEndAts(ctx context.Context, items []*types.SandboxBriefData) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil && item.SandboxID != "" {
			ids = append(ids, item.SandboxID)
		}
	}
	endAts := lookupSandboxEndAts(ctx, ids)
	for _, item := range items {
		if item == nil {
			continue
		}
		if endAt, ok := endAts[item.SandboxID]; ok {
			item.EndAt = endAt
		}
	}
}

// enrichSandboxListBackends fills Backend from t_cube_sandbox_spec (DB source of
// truth for create-time xfs|s3). Missing specs leave Backend empty.
func enrichSandboxListBackends(ctx context.Context, items []*types.SandboxBriefData) {
	var ids []string
	for _, item := range items {
		if item == nil || item.SandboxID == "" || strings.TrimSpace(item.Backend) != "" {
			continue
		}
		if item.Annotations != nil {
			if b := strings.TrimSpace(item.Annotations[constants.CubeAnnotationStorageBackend]); b != "" {
				item.Backend = b
				continue
			}
		}
		ids = append(ids, item.SandboxID)
	}
	if len(ids) == 0 {
		return
	}
	specs, err := sandboxspec.GetMany(ctx, ids)
	if err != nil {
		if !errors.Is(err, sandboxspec.ErrSandboxSpecStoreNotReady) {
			log.G(ctx).Warnf("ListSandbox: read sandbox specs for backend: %v", err)
		}
		return
	}
	for _, item := range items {
		if item == nil || strings.TrimSpace(item.Backend) != "" {
			continue
		}
		spec := specs[item.SandboxID]
		if spec == nil {
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
					CPUMilli:    parseCPUMilli(container.GetResources().GetCpu()),
					MemoryMiB:   parseMemoryMiB(container.GetResources().GetMem()),
					Annotations: buildAnnotationsFromLabels(labels),
					Labels:      labels,
					NameSpace:   sandbox.GetNamespace(),
					CreateAt:    sandbox.GetCreatedAt(),
					PauseAt:     container.GetPausedAt(),
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
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil {
		return false
	}
	tmpFilter := cfg.Common.ListFilterOutLables
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

func parseCPUMilli(raw string) int32 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}

	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	cpuMilli := quantity.MilliValue()
	if cpuMilli <= 0 {
		return 0
	}
	const maxInt32 = int64(1<<31 - 1)
	if cpuMilli > maxInt32 {
		return int32(maxInt32)
	}
	return int32(cpuMilli)
}

func parseMemoryMiB(raw string) int32 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}

	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	bytes := quantity.Value()
	if bytes <= 0 {
		return 0
	}
	const (
		bytesPerMiB = int64(1024 * 1024)
		maxInt32    = int64(1<<31 - 1)
	)
	memoryMiB := bytes / bytesPerMiB
	if bytes%bytesPerMiB != 0 {
		memoryMiB++
	}
	if memoryMiB > maxInt32 {
		return int32(maxInt32)
	}
	return int32(memoryMiB)
}
