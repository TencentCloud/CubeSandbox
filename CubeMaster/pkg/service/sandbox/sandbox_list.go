// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/api/resource"
)

func ListSandbox(ctx context.Context, req *types.ListCubeSandboxReq) (rsp *types.ListCubeSandboxRes) {
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

	dealRspData(ctx, done, resChan, rsp)

	var wg sync.WaitGroup

	for _, tmpNode := range nodeList {
		tmpNode := tmpNode
		recov.GoWithWaitGroup(&wg, func() {
			doOneList(ctx, req, tmpNode, resChan)
		}, func(panicError interface{}) {
			log.G(ctx).Fatalf("panic:%v", string(debug.Stack()))
		})
	}
	wg.Wait()
	close(resChan)
	<-done
	return
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

func doOneList(ctx context.Context, req *types.ListCubeSandboxReq, tmpNode *node.Node, resChan chan *types.SandboxBriefData) {
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
		log.G(ctx).WithFields(map[string]interface{}{
			"CalleeEndpoint": rt.CalleeEndpoint,
		}).Errorf("List sandbox error:%v", err)
		return
	}

	for _, sandbox := range cubeRsp.GetItems() {
		for _, container := range sandbox.GetContainers() {
			if container.GetType() == "sandbox" {
				if matchFilter(container.GetLabels()) {
					continue
				}
				labels := cloneStringMap(container.GetLabels())
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
					Annotations: buildTemplateAnnotations(templateID),
					Labels:      labels,
					NameSpace:   sandbox.GetNamespace(),
					CreateAt:    sandbox.GetCreatedAt(),
					PauseAt:     container.GetPausedAt(),
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
