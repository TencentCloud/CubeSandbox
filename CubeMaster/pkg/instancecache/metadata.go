// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package instancecache

import (
	"context"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func trace(ctx context.Context, action string, op string, start time.Time, err error) {
	cost := time.Since(start)
	if cost.Milliseconds() > 1 {
		baseRt := CubeLog.GetTraceInfo(ctx).DeepCopy()
		baseRt.Callee = constants.Redis
		baseRt.Action = action
		baseRt.CalleeAction = op
		baseRt.Cost = cost
		baseRt.RetCode = int64(errorcode.ErrorCode_Success)
		if err != nil {
			baseRt.RetCode = int64(errorcode.ErrorCode_DBError)
		}
		CubeLog.Trace(baseRt)
	}
}

func KeyMetadata(objs ...string) string {
	segs := []string{"instance", "metadata"}
	segs = append(segs, objs...)
	return strings.Join(segs, ":")
}

func MetadataSet(ctx context.Context, key string, value string) (err error) {
	const (
		redisOp = "SET"
	)
	start := time.Now()
	defer trace(ctx, "Create", redisOp, start, err)
	_, err = wrapredis.GetRedis().Do(redisOp, key, value)
	if err != nil {
		log.G(ctx).Errorf("redis %s error, key: %s, err: %s", redisOp, key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("redis.%s:%s:%s", redisOp, key, value)
	}
	return nil
}

func MetadataPush(ctx context.Context, key string, value string) (err error) {
	const (
		redisOp = "RPUSH"
	)
	start := time.Now()
	defer trace(ctx, "Create", redisOp, start, err)

	_, err = wrapredis.GetRedis().Do(redisOp, key, value)
	if err != nil {
		log.G(ctx).Errorf("redis %s error, key: %s, err: %s", redisOp, key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("redis.%s:%s:%s", redisOp, key, value)
	}
	return nil
}

func MetadataLRem(ctx context.Context, key string, value string) (err error) {
	const (
		redisOp = "LREM"
	)
	start := time.Now()
	defer trace(ctx, "Destroy", redisOp, start, err)

	_, err = wrapredis.GetRedis().Do(redisOp, key, 0, value)
	if err != nil {
		log.G(ctx).Errorf("redis %s error, key: %s, err: %s", redisOp, key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("redis.%s:%s:%s", redisOp, key, value)
	}
	return nil
}

func MetadataDel(ctx context.Context, key string) (err error) {
	const (
		redisOp = "DEL"
	)
	start := time.Now()
	defer trace(ctx, "Destroy", redisOp, start, err)
	_, err = wrapredis.GetRedis().Do(redisOp, key)
	if err != nil {
		log.G(ctx).Errorf("redis %s error, key: %s, err: %s", redisOp, key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("redis.%s:%s", redisOp, key)
	}
	return nil
}
