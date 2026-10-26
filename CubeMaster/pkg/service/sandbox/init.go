// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapconcurrent"
)

type local struct {
	CubeletListLock *utils.ResourceLocks

	CreateRetryConf *wrapconcurrent.ConcurrentHandle
}

var l *local

func Init(ctx context.Context, cfg *config.Config) error {
	l = &local{
		CubeletListLock: utils.NewResourceLocks(),
		CreateRetryConf: &wrapconcurrent.ConcurrentHandle{},
	}

	l.CreateRetryConf = &wrapconcurrent.ConcurrentHandle{}
	l.CreateRetryConf.SetMaxRetry(cfg.CubeletConf.MaxRetries)
	l.CreateRetryConf.SetLoopMaxRetry(cfg.CubeletConf.LoopMaxRetries)

	if v, ok := cfg.CubeletConf.AsyncFlows["Create"]; ok {
		if v.MaxRetries > 0 {
			l.CreateRetryConf.SetMaxRetry(v.MaxRetries)

		}
		if v.LoopMaxRetries > 0 {
			l.CreateRetryConf.SetLoopMaxRetry(v.LoopMaxRetries)

		}
	}

	return nil
}
