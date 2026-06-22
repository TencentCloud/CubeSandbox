// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package instancecache

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
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

// KeyMetadata builds the namespaced metadata key for the given segments.
func KeyMetadata(objs ...string) string {
	return rediskey.InstanceMeta(objs...)
}

func MetadataSet(ctx context.Context, value string, objs ...string) (err error) {
	const redisOp = "SET"
	start := time.Now()
	defer func() {
		trace(ctx, "Create", redisOp, start, err)
	}()
	key := rediskey.InstanceMeta(objs...)
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

func MetadataPush(ctx context.Context, value string, objs ...string) (err error) {
	const redisOp = "RPUSH"
	start := time.Now()
	defer func() {
		trace(ctx, "Create", redisOp, start, err)
	}()
	key := rediskey.InstanceMeta(objs...)
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

func MetadataLRem(ctx context.Context, value string, objs ...string) (err error) {
	const redisOp = "LREM"
	start := time.Now()
	defer func() {
		trace(ctx, "Destroy", redisOp, start, err)
	}()
	key := rediskey.InstanceMeta(objs...)
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

func MetadataDel(ctx context.Context, objs ...string) (err error) {
	const redisOp = "DEL"
	start := time.Now()
	defer func() {
		trace(ctx, "Destroy", redisOp, start, err)
	}()
	var firstErr error
	for _, key := range rediskey.DeleteKeys(rediskey.InstanceMeta(objs...), rediskey.LegacyInstanceMeta(objs...)) {
		if _, e := wrapredis.GetRedis().Do(redisOp, key); e != nil {
			log.G(ctx).Errorf("redis %s error, key: %s, err: %s", redisOp, key, e)
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		if log.IsDebug() {
			log.G(ctx).Debugf("redis.%s:%s", redisOp, key)
		}
	}
	err = firstErr
	return err
}
