// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	metrictype "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type contextKey string

const (
	KCreateContext contextKey = "kCreateContext"

	KInitContext contextKey = "KInitContext"

	KDestroyContext contextKey = "KDestroyContext"
)

func RecordCreateMetric(ctx context.Context, err error, id string, cost time.Duration) {
	createCtxTmp := ctx.Value(KCreateContext)
	if createCtxTmp != nil {
		createCtx, ok := createCtxTmp.(*CreateContext)
		if ok {
			innerIndex, _ := ctx.Value(constants.KCubeIndexContext).(string)
			if innerIndex == "0" {
				innerIndex = "sandbox"
			} else {
				innerIndex = fmt.Sprintf("%s-%s", "container", innerIndex)
			}
			switch id {
			case constants.CubeNewContainerId:
				id = fmt.Sprintf("create-%s-metadata", innerIndex)
			case constants.CubeContainerSpecId:
				id = fmt.Sprintf("gen-spec-%s", innerIndex)
			case constants.CubeShimBinaryStartId, constants.CubeShimCreatetId, constants.CubeShimStartId,
				constants.CubeShimUpdateId, constants.CubeShimWaitId, constants.CubeProbeId:
				id = fmt.Sprintf("%s-%s", innerIndex, id)
			default:
			}
			createCtx.AddMetric(err, id, cost)
		}
	}
}

func RecordCreateMetricIfGreaterThan(ctx context.Context, err error, id string, cost time.Duration, threshold time.Duration) {
	if cost <= threshold {
		return
	}
	RecordCreateMetric(ctx, err, id, cost)
}

func RecordDestroyMetric(ctx context.Context, err error, id string, cost time.Duration) {
	destroyCtxTmp := ctx.Value(KDestroyContext)
	if destroyCtxTmp != nil {
		destroyCtx, ok := destroyCtxTmp.(*DestroyContext)
		if ok {
			innerIndex, _ := ctx.Value(constants.KCubeIndexContext).(string)
			if innerIndex == "0" {
				innerIndex = "sandbox"
			} else if innerIndex == "" {
				innerIndex = "unknown-container"
			} else {
				innerIndex = fmt.Sprintf("%s-%s", "container", innerIndex)
			}

			switch id {
			case constants.CubeDelContainerId:
				id = fmt.Sprintf("del-%s-metadata", innerIndex)
			case constants.DelContainer, constants.DelSandbox:
				id = fmt.Sprintf("%s-%s", innerIndex, id)
			default:
				id = fmt.Sprintf("%s-%s", id, innerIndex)
			}
			destroyCtx.AddMetric(err, id, cost)
		}
	}
}

func (e *Engine) RegisterMetrics(register *metrictype.CollectRegister) error {
	register.AddCollector(metrictype.MetricTypeCLS, func() (any, error) {
		var traces []*CubeLog.RequestTrace
		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "CreationPeak",
			Callee:  constants.WorkflowID.ID(),
			RetCode: int64(e.GetFlowPeakRequests("create")),
		})
		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "DeletionPeak",
			Callee:  constants.WorkflowID.ID(),
			RetCode: int64(e.GetFlowPeakRequests("destroy")),
		})
		return traces, nil
	})
	return nil
}
