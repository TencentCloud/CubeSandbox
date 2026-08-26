// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package grpcconn provides a pool of grpc connections.
package grpcconn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	grpcpool "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/grpc-middleware/pool"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/workpool"
)

type workerGrpcConnPool struct {
	cache sync.Map
	ctx   context.Context
}

var connPool *workerGrpcConnPool

func Init(ctx context.Context) {
	connPool = &workerGrpcConnPool{
		ctx: ctx,
	}
	recov.GoWithRetry(connPool.checkWorkerConn, 1000)
}

func (wc *workerGrpcConnPool) checkWorkerConn() {
	routinePool := workpool.NewWorkerPool(config.GetConfig().CubeletConf.Grpc.CleanConnTaskRoutinePoolSize)
	interval := time.Duration(config.GetConfig().CubeletConf.Grpc.CleanConnTaskIntervalInMin) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-wc.ctx.Done():
			return
		case <-ticker.C:
			wc.cache.Range(func(addr, connPool interface{}) bool {
				active, ref := connPool.(grpcpool.Pool).GetActiveTimeAndRef()
				if time.Now().Sub(active) >
					time.Duration(config.GetConfig().CubeletConf.Grpc.ConnExpireTimeInSec)*time.Second &&
					ref == 0 {
					wc.cache.Delete(addr)
					routinePool.Exec(func() {
						_ = connPool.(grpcpool.Pool).Close()
					})
				}
				return true
			})
		}
	}
}

func GetWorkerConn(ctx context.Context, addr string) (grpcpool.Conn, error) {
	if connPool == nil {
		return nil, fmt.Errorf("worker grpc conn pool is not initialized")
	}
	key := constants.GetUA(ctx) + "+" + addr
	cp, ok := connPool.cache.Load(key)
	if ok && cp != nil {
		return cp.(grpcpool.Pool).Get()
	}
	newPool, err := grpcpool.New(constants.GetUA(ctx), addr, grpcpool.SingleConnDefaultOptions)
	if err != nil {
		return nil, err
	}
	cp, ok = connPool.cache.LoadOrStore(key, newPool)
	if ok {
		recov.GoWithRecover(func() {
			_ = newPool.Close()
		})
	}
	return cp.(grpcpool.Pool).Get()
}

func CloseWorkerConn(addr string) {
	cp, ok := connPool.cache.Load(addr)
	if ok && cp != nil {
		connPool.cache.Delete(addr)
		_ = cp.(grpcpool.Pool).Close()
	}
}
