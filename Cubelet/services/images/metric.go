// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	metrictype "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func (l *local) RegisterMetrics(register *metrictype.CollectRegister) error {
	register.AddCollector(metrictype.MetricTypeCLS, func() (any, error) {
		var traces []*CubeLog.RequestTrace
		images, err := l.criImage.ListImage(context.TODO())
		if err != nil {
			return nil, fmt.Errorf("failed to list all namespace images: %w", err)
		}
		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "ImageTotal",
			Callee:  constants.CubeboxID.ID(),
			RetCode: int64(len(images)),
		})

		return traces, nil
	})
	return nil
}
