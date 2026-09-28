// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/trace"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type GCServicesConfig struct {
	CleanupIntervalStr string `toml:"cleanup_interval"`
	cleanupInterval    time.Duration
}
type gcService struct {
	config         *GCServicesConfig
	engine         *workflow.Engine
	gc             *local
	concurrentLock sync.Map
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.CubeboxServicePlugin,
		ID:     constants.GCServiceID.ID(),
		Config: &GCServicesConfig{},
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.WorkflowPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					CubeLog.Fatalf("plugin %s init fail:%v", constants.GCServiceID, err.Error())
				}
			}()

			config := ic.Config.(*GCServicesConfig)
			t, err := time.ParseDuration(config.CleanupIntervalStr)
			if err != nil || t == 0 {
				config.cleanupInterval = 5 * time.Second
			} else {
				config.cleanupInterval = t
			}

			p, err := ic.GetByID(constants.WorkflowPlugin, constants.WorkflowID.ID())
			if err != nil {
				return nil, err
			}

			e, ok := p.(*workflow.Engine)
			if !ok {
				return nil, err
			}
			s := &gcService{engine: e, config: config, gc: l}
			go s.run(ic.Context)
			return s, nil
		},
	})
}
func (l *gcService) run(ctx context.Context) {
	cleanUpTicker := time.NewTicker(l.config.cleanupInterval)
	defer cleanUpTicker.Stop()

	rt := &CubeLog.RequestTrace{
		Action: "CleanUp",
		Caller: constants.GCID.ID(),
		Callee: l.engine.ID(),
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanUpTicker.C:
			recov.WithRecover(func() {
				dirtyInfos, err := l.gc.readAll()
				if err == nil && len(dirtyInfos) > 0 {
					for _, v := range dirtyInfos {
						info := v

						exist, unlock := l.loadOrStore(info.SandboxID)
						if exist {
							continue
						}

						recov.GoWithRecover(func() {
							info := info
							innerUnlock := unlock
							defer innerUnlock()

							opts := &workflow.CleanContext{
								BaseWorkflowInfo: workflow.BaseWorkflowInfo{
									SandboxID: info.SandboxID,
								},
							}
							tmpCtx, cancel := context.WithTimeout(context.Background(), config.GetCommon().CommonTimeout)
							defer cancel()

							defer recov.HandleCrash(func(panicError interface{}) {
								err = fmt.Errorf("%v", panicError)
								log.G(ctx).Fatalf("cleanUpTicker panic :%v %v", panicError, string(debug.Stack()))
							})

							tmpCtx = namespaces.WithNamespace(tmpCtx, info.Namespace)
							gcRt := rt.DeepCopy()
							gcRt.CalleeAction = "CleanUp"
							gcRt.RequestID = uuid.New().String()
							gcRt.InstanceID = info.SandboxID

							tmpCtx = CubeLog.WithRequestTrace(tmpCtx, gcRt)
							tmpCtx = log.WithLogger(tmpCtx, log.NewWrapperLogEntry(log.AuditLogger.WithContext(tmpCtx)))
							if err := l.engine.CleanUp(tmpCtx, opts); err != nil {
								gcRt.RetCode = int64(errorcode.ErrorCode_RemoveContainerFailed)
								CubeLog.WithContext(tmpCtx).Fatalf("Cubelet CleanUp fail:%v", err)
							}
							reportTrace(tmpCtx, opts.GetMetric())
						})
					}

					CubeLog.WithContext(ctx).Infof("Cubelet CleanUp")
				}
			})
		}
	}
}

func (l *gcService) loadOrStore(sandboxID string) (bool, func()) {
	_, exist := l.concurrentLock.LoadOrStore(sandboxID, struct{}{})
	return exist, func() {
		l.concurrentLock.Delete(sandboxID)
	}
}

func reportTrace(ctx context.Context, metrics []*workflow.Metric) {
	action, _ := ctx.Value(CubeLog.KeyAction).(string)
	calleeA, _ := ctx.Value(CubeLog.KeyCalleeAction).(string)
	for _, m := range metrics {
		if m != nil {
			cubelogCode := CubeLog.CodeSuccess
			retCode := errorcode.ErrorCode_Success
			if m.Error() != nil {
				cubelogCode = CubeLog.CodeInternalError
				retCode = errorcode.ErrorCode_Unknown
			}
			trace.Report(ctx, m.ID(), "", action, calleeA, m.Duration(), retCode, cubelogCode)
		}
	}
}
