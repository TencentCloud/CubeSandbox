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
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// local 本地调度器状态：维护各实例类型的任务缓冲队列，以及全局销毁并发限制
type local struct {
	// bufferTaskMap 按实例类型(product)维度组织的缓冲任务队列
	bufferTaskMap              map[string]*buffertask
	stop                       chan struct{}
	ctx                        context.Context
	lastDestroyconcurrentLimit int64 // 上一次计算出的全局销毁并发上限，用于检测变化
}

var l *local

// initTask 初始化本地调度器：为默认实例类型和所有配置的实例类型创建缓冲队列，
// 并启动指标采集、指标上报、并发限制监控三个后台协程
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

// Stop 停止调度器：关闭停止信号，并优雅停止所有缓冲队列
func Stop(ctx context.Context) {
	close(l.stop)
	for _, v := range l.bufferTaskMap {
		v.bufferQ.GraceFullStop(ctx)
	}
}

// buffertask 单个实例类型的缓冲任务队列，带并发限制与指标统计
type buffertask struct {
	bufferQ                         bufferqueue.BufferQueue
	lastCreateConcurrentLimit       int64 // 上一次设置的创建并发上限
	bufferTaskLenMax                int64 // 观测到的队列最大长度（用于上报峰值）
	bufferWorkingsMax               int64 // 观测到的最大在途任务数（用于上报峰值）
	localCreateConcurrentNumMetrics *sync.Map
}

// newTask 创建缓冲任务队列，队列容量取配置中的 BufferQueueMinJob
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

// Push 将任务加入缓冲队列
func (t *buffertask) Push(x interface{}) {
	if t == nil || t.bufferQ == nil {
		return
	}
	t.bufferQ.Push(x)
}

// Len 返回缓冲队列当前长度
func (t *buffertask) Len() int {
	if t == nil || t.bufferQ == nil {
		return 0
	}
	return t.bufferQ.Len()
}

// Workings 返回缓冲队列当前在途（正在处理）的任务数
func (t *buffertask) Workings() int {
	if t == nil || t.bufferQ == nil {
		return 0
	}
	return int(t.bufferQ.Workings())
}

// setBufferTaskConcurrent 设置该队列的并发处理上限
func (t *buffertask) setBufferTaskConcurrent(n int64) {
	if t == nil || t.bufferQ == nil {
		return
	}
	t.bufferQ.SetLimit(n)
}

// setLastCreateConcurrentLimit 记录当前生效的创建并发上限
func (t *buffertask) setLastCreateConcurrentLimit(n int64) {
	if t == nil {
		return
	}
	t.lastCreateConcurrentLimit = n
}

// AddBufferTask 将任务推入指定产品（实例类型）的缓冲队列；
// 若该实例类型未配置队列，则回退到默认实例类型的队列
func AddBufferTask(x interface{}, product string) {
	bufferQ, ok := l.bufferTaskMap[product]
	if !ok || bufferQ == nil {

		bufferQ = l.bufferTaskMap[constants.DefaultInstanceTypeName]
	}
	bufferQ.Push(x)
}

// collectMetric 周期性采集各缓冲队列的指标：记录队列长度与在途任务数的历史峰值，
// 并按节点统计单节点创建并发数（>5 时记录其峰值，供后续上报）
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

// reportMetric 周期性上报指标：将采集到的峰值（队列长度、在途任务数、单节点创建并发数）
// 通过日志 Trace 上报，上报后清零
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

// reportStdevTrace 上报集群节点资源分布的均衡度指标（标准差）：
// 分别统计 CPU 配额使用率、内存配额使用率、MVM 数量的标准差并上报
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

// limitDestroyOfEveryNode 返回每节点配置的销毁并发上限
func limitDestroyOfEveryNode() int64 {

	return config.GetConfig().CubeletConf.DestroyConcurentLimit
}

// monitorLimit 周期性监控并动态调整并发限制：
// 1. 按集群健康节点数与 master 节点数，为每个实例类型重新计算创建并发上限并下发到缓冲队列；
// 2. 计算全局销毁并发上限，变化时更新销毁任务 worker 并发数
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

				// 每节点创建并发上限：取集群平均与配置下限中的较大值
				newCreateLimitOfEveryNode := max(int64(math.Ceil(float64(totalLimitCreate*1.0/newHealthyNodes))),
					config.GetConfig().CubeletConf.CreateConcurrentLimit)

				// 按 master 节点数分摊后的全局创建并发上限
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

			newLimitDesroyOfEveryNode := limitDestroyOfEveryNode()
			limitDestroy := int64(math.Ceil(float64(newHealthyNodes * newLimitDesroyOfEveryNode * 1.0 / newMasterNodes)))
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
