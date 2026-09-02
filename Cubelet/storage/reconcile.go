// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func (l *local) loopReconcile(ctx context.Context) {

	if config.GetConfig() == nil || config.GetConfig().Common == nil {
		CubeLog.Warnf("loopReconcile: config not initialized, skip reconcile loop")
		return
	}

	gcTicker := time.NewTicker(time.Second)
	defer gcTicker.Stop()

	rt := &CubeLog.RequestTrace{
		Callee: "Reconcile",
		Caller: constants.StorageID.ID(),
	}

	checkDeadline := time.Now().Add(config.GetConfig().Common.ReconcileInterval)
	for {
		select {
		case <-ctx.Done():
			CubeLog.Errorf("loopReconcile done: %v", ctx.Err())
			return
		case <-gcTicker.C:
			recov.WithRecover(func() {

				if config.GetCommon() == nil {
					return
				}
				if checkDeadline.After(time.Now()) {

					return
				}
				defer func() {
					if config.GetConfig() != nil && config.GetConfig().Common != nil {
						checkDeadline = time.Now().Add(config.GetConfig().Common.ReconcileInterval)
					}
				}()
				rt.RequestID = uuid.New().String()
				_ = CubeLog.WithRequestTrace(context.Background(), rt)
			}, func(panicError interface{}) {
				if config.GetConfig() != nil && config.GetConfig().Common != nil {
					checkDeadline = time.Now().Add(config.GetConfig().Common.ReconcileInterval)
				}
				CubeLog.WithContext(context.Background()).Fatalf("loopReconcile panic:%v", panicError)
			})
		}
	}
}
