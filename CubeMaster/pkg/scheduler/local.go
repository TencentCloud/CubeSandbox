// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rcrowley/go-metrics"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/bufferqueue"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type local struct {
	bufferTaskMap              map[string]*buffertask
	stop                       chan struct{}
	ctx                        context.Context
	lastDestroyconcurrentLimit int64
}

var l *local

func initTask(ctx context.Context) error {
	l = &local{
		ctx:                        ctx,
		stop:                       make(chan struct{}),
		bufferTaskMap:              map[string]*buffertask{},
		lastDestroyconcurrentLimit: 0,
	}
	l.bufferTaskMap[constants.DefaultInstanceTypeName] = newTask()
	for _, k := range config.GetSchedulerInstanceTypeConfs() {
		l.bufferTaskMap[k] = newTask()
	}

	recov.GoWithRecover(l.collectMetric)
	recov.GoWithRecover(l.reportMetric)
	recov.GoWithRecover(l.monitorLimit)
	return nil
}

func Stop(ctx context.Context) {
	close(l.stop)
	for _, v := range l.bufferTaskMap {
		v.bufferQ.GraceFullStop(ctx)
	}
}

type buffertask struct {
	bufferQ                         bufferqueue.BufferQueue
	lastCreateConcurrentLimit       int64
	bufferTaskLenMax                int64
	bufferWorkingsMax               int64
	localCreateConcurrentNumMetrics *sync.Map
}

func newTask() *buffertask {
	return &buffertask{
		bufferQ: bufferqueue.New(
			&bufferqueue.Options{Limit: config.GetConfig().CubeletConf.BufferQueueMinJob}),
		lastCreateConcurrentLimit:       0,
		bufferTaskLenMax:                math.MinInt64,
		bufferWorkingsMax:               math.MinInt64,
		localCreateConcurrentNumMetrics: &sync.Map{},
	}
}

func (t *buffertask) Push(x interface{}) {
	if t == nil || t.bufferQ == nil {
		return
	}
	t.bufferQ.Push(x)
}

func (t *buffertask) Len() int {
	if t == nil || t.bufferQ == nil {
		return 0
	}
	return t.bufferQ.Len()
}
func (t *buffertask) Workings() int {
	if t == nil || t.bufferQ == nil {
		return 0
	}
	return int(t.bufferQ.Workings())
}

func (t *buffertask) setBufferTaskConcurrent(n int64) {
	if t == nil || t.bufferQ == nil {
		return
	}
	t.bufferQ.SetLimit(n)
}
func (t *buffertask) setLastCreateConcurrentLimit(n int64) {
	if t == nil {
		return
	}
	t.lastCreateConcurrentLimit = n
}

func AddBufferTask(x interface{}, product string) {
	bufferQ, ok := l.bufferTaskMap[product]
	if !ok || bufferQ == nil {

		bufferQ = l.bufferTaskMap[constants.DefaultInstanceTypeName]
	}
	bufferQ.Push(x)
}

func (l *local) collectMetric() {
	ticker := time.NewTicker(config.GetConfig().Common.CollectMetricInterval)
	defer ticker.Stop()

	for range ticker.C {
		select {
		case <-l.stop:
			return
		case <-l.ctx.Done():
			return
		default:
		}
		recov.WithRecover(func() {
			for product, bufferQ := range l.bufferTaskMap {
				v := int64(bufferQ.Len())
				if v > atomic.LoadInt64(&bufferQ.bufferTaskLenMax) {
					atomic.StoreInt64(&bufferQ.bufferTaskLenMax, v)
				}
				v = int64(bufferQ.Workings())
				if v > atomic.LoadInt64(&bufferQ.bufferWorkingsMax) {
					atomic.StoreInt64(&bufferQ.bufferWorkingsMax, v)
				}
				if config.GetConfig().Common.ReportLocalCreateNum {
					for _, n := range localcache.GetHealthyNodesByInstanceType(-1, product) {
						v := localcache.LocalCreateConcurrentLimit(n)
						if v > 5 {
							if oldv, ok := bufferQ.localCreateConcurrentNumMetrics.Load(n.IP); !ok {
								bufferQ.localCreateConcurrentNumMetrics.Store(n.IP, v)
							} else {
								if oldv.(int64) < v {
									bufferQ.localCreateConcurrentNumMetrics.Store(n.IP, v)
								}
							}
						}
					}
				}
			}
		}, func(panicError interface{}) {
			CubeLog.WithContext(context.Background()).Fatalf("collect panic:%v", panicError)
		})

	}
}

func (l *local) reportMetric() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	metricTrace := &CubeLog.RequestTrace{
		Caller: constants.CubeMasterServiceID,
		Callee: "metric",
	}
	for range ticker.C {
		select {
		case <-l.stop:
			return
		case <-l.ctx.Done():
			return
		default:
		}
		recov.WithRecover(func() {
			metricTrace.CalleeEndpoint = ""
			for product, bufferQ := range l.bufferTaskMap {
				metricTrace.InstanceType = product
				if v := atomic.SwapInt64(&bufferQ.bufferTaskLenMax, 0); v > 0 {
					metricTrace.Action = "BufferTask"
					metricTrace.RetCode = v
					CubeLog.Trace(metricTrace)
				}
				if v := atomic.SwapInt64(&bufferQ.bufferWorkingsMax, 0); v > 0 {
					metricTrace.Action = "BufferWorkings"
					metricTrace.RetCode = v
					CubeLog.Trace(metricTrace)
				}

				if config.GetConfig().Common.ReportStdevMetric {
					reportStdevTrace()
				}

				if config.GetConfig().Common.ReportLocalCreateNum {
					bufferQ.localCreateConcurrentNumMetrics.Range(func(k, v interface{}) bool {
						num, ok := v.(int64)
						if ok && num > 0 {
							metricTrace.Action = "LocalCreateConcurrentNum"
							metricTrace.CalleeEndpoint = k.(string)
							metricTrace.RetCode = v.(int64)
							CubeLog.Trace(metricTrace)
							bufferQ.localCreateConcurrentNumMetrics.Store(k, int64(0))
						}
						return true
					})
				}
			}
		}, func(panicError interface{}) {
			CubeLog.WithContext(context.Background()).Fatalf("reportMetric panic:%v", panicError)
		})
	}
}

func reportStdevTrace() {
	metricTrace := &CubeLog.RequestTrace{
		Caller: constants.CubeMasterServiceID,
		Callee: "metric",
	}
	cpuQuotaUsagePercent := []int64{}
	memQuotaUsagePercent := []int64{}
	mvmNumPercent := []int64{}
	zeroWrap := func(n int64) int64 {
		if n == 0 {
			return int64(1)
		}
		return n
	}
	for _, n := range localcache.GetHealthyNodes(-1) {
		cpuQuotaUsagePercent = append(cpuQuotaUsagePercent, n.QuotaCpuUsage*100/zeroWrap(n.QuotaCpu))
		memQuotaUsagePercent = append(memQuotaUsagePercent, n.QuotaMemUsage*100/zeroWrap(n.QuotaMem))
		mvmNumPercent = append(mvmNumPercent, n.MvmNum*100/localcache.MaxMvmLimit(n))
	}

	cpuquotausagestdDev := metrics.SampleStdDev(cpuQuotaUsagePercent) * 100.0
	memquotausagestdDev := metrics.SampleStdDev(memQuotaUsagePercent) * 100.0
	mvmNumstdev := metrics.SampleStdDev(mvmNumPercent) * 100.0

	metricTrace.Action = "mvmumstdDev"
	metricTrace.RetCode = int64(math.Ceil(mvmNumstdev))
	CubeLog.Trace(metricTrace)

	metricTrace.Action = "cpustdDev"
	metricTrace.RetCode = int64(math.Ceil(cpuquotausagestdDev))
	CubeLog.Trace(metricTrace)

	metricTrace.Action = "memstdDev"
	metricTrace.RetCode = int64(math.Ceil(memquotausagestdDev))
	CubeLog.Trace(metricTrace)
}

func limitDestroyOfEveryNode() int64 {

	return config.GetConfig().CubeletConf.DestroyConcurrentLimit
}

func (l *local) monitorLimit() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		select {
		case <-l.stop:
			return
		case <-l.ctx.Done():
			return
		default:
		}
		limitFn := func(product string, bufferQ *buffertask) {
			recov.WithRecover(func() {
				nodes := localcache.GetHealthyNodesByInstanceType(-1, product)
				totalLimitCreate := int64(0)
				for i := range nodes {
					totalLimitCreate += localcache.CreateConcurrentLimit(nodes[i])
				}
				newHealthyNodes := int64(nodes.Len())
				if newHealthyNodes <= 0 {
					return
				}
				newMasterNodes := localcache.HealthyMasterNodes()

				newCreateLimitOfEveryNode := max(int64(math.Ceil(float64(totalLimitCreate*1.0/newHealthyNodes))),
					config.GetConfig().CubeletConf.CreateConcurrentLimit)

				limitCreate := int64(math.Ceil(float64(newHealthyNodes * newCreateLimitOfEveryNode * 1.0 / newMasterNodes)))
				if bufferQ.lastCreateConcurrentLimit != limitCreate {
					bufferQ.setLastCreateConcurrentLimit(limitCreate)
					bufferQ.setBufferTaskConcurrent(limitCreate)
					CubeLog.WithContext(context.Background()).Warnf("monitorLimit,limitCreate:%s:%d", product, limitCreate)
				}
			}, func(panicError interface{}) {
				CubeLog.WithContext(context.Background()).Fatalf("monitorLimit panic:%s,%v", product, panicError)
			})
		}

		for product, bufferQ := range l.bufferTaskMap {
			limitFn(product, bufferQ)
		}
		recov.WithRecover(func() {
			nodes := localcache.GetHealthyNodes(-1)
			newHealthyNodes := int64(nodes.Len())
			if newHealthyNodes <= 0 {
				return
			}
			newMasterNodes := localcache.HealthyMasterNodes()

			newLimitDestroyOfEveryNode := limitDestroyOfEveryNode()
			limitDestroy := int64(math.Ceil(float64(newHealthyNodes * newLimitDestroyOfEveryNode * 1.0 / newMasterNodes)))
			if l.lastDestroyconcurrentLimit != limitDestroy && limitDestroy >= 1 {
				l.lastDestroyconcurrentLimit = limitDestroy
				task.SetTaskWorkerConcurrent(task.DestroySandbox, limitDestroy)
				CubeLog.WithContext(context.Background()).Warnf("monitorLimit,limitDestroy:%d", limitDestroy)
			}
		}, func(panicError interface{}) {
			CubeLog.WithContext(context.Background()).Fatalf("monitorLimit panic:%v", panicError)
		})
	}
}
