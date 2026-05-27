// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metric

import (
	"context"

	"github.com/docker/go-metrics"
	prom "github.com/prometheus/client_golang/prometheus"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	metrictypes "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

const (
	namespace = "cube"
	subsystem = "cubebox"
)

var metricPrefix = namespace + "_" + subsystem + "_scheduler_"

type schedulerCollector struct {
	register       *metrictypes.CollectRegister
	workflowEngine *workflow.Engine

	descQuotaCPUMilli      *prom.Desc
	descQuotaMemMB         *prom.Desc
	descMvmNum             *prom.Desc
	descMvmRunningNum      *prom.Desc
	descNicQueues          *prom.Desc
	descRealtimeCreateNum  *prom.Desc
	descRealtimeDestroyNum *prom.Desc
}

func newSchedulerCollector(register *metrictypes.CollectRegister, engine *workflow.Engine) *schedulerCollector {
	return &schedulerCollector{
		register:       register,
		workflowEngine: engine,
		descQuotaCPUMilli: prom.NewDesc(
			metricPrefix+"quota_cpu_usage_milli",
			"CPU quota usage", nil, nil),
		descQuotaMemMB: prom.NewDesc(
			metricPrefix+"quota_mem_mb_usage",
			"Memory quota usage in megabytes", nil, nil),
		descMvmNum: prom.NewDesc(
			metricPrefix+"mvm_num",
			"Total number of micro-VMs", nil, nil),
		descMvmRunningNum: prom.NewDesc(
			metricPrefix+"mvm_running_num",
			"Number of running micro-VMs", nil, nil),
		descNicQueues: prom.NewDesc(
			metricPrefix+"nic_queues",
			"Total NIC queues allocated", nil, nil),
		descRealtimeCreateNum: prom.NewDesc(
			metricPrefix+"realtime_create_num",
			"Number of in-flight create requests", nil, nil),
		descRealtimeDestroyNum: prom.NewDesc(
			metricPrefix+"realtime_destroy_num",
			"Number of in-flight destroy requests", nil, nil),
	}
}

func (c *schedulerCollector) Describe(ch chan<- *prom.Desc) {
	ch <- c.descQuotaCPUMilli
	ch <- c.descQuotaMemMB
	ch <- c.descMvmNum
	ch <- c.descMvmRunningNum
	ch <- c.descNicQueues
	ch <- c.descRealtimeCreateNum
	ch <- c.descRealtimeDestroyNum
}

func (c *schedulerCollector) Collect(ch chan<- prom.Metric) {
	// Prometheus collectors are invoked by the HTTP handler with no
	// caller-supplied context, so Background is the standard choice.
	ctx := context.Background()

	m := make(map[string]any)
	jobs := c.register.Get(metrictypes.MetricTypeOSS)
	for _, job := range jobs {
		metricValue, err := job()
		if err != nil {
			log.G(ctx).Errorf("scheduler collector: metric job error: %v", err)
			continue
		}
		metricValueMap, ok := metricValue.(map[string]any)
		if !ok {
			log.G(ctx).Errorf("scheduler collector: metric value is not map[string]any")
			continue
		}
		for k, v := range metricValueMap {
			if _, dup := m[k]; dup {
				log.G(ctx).Warnf("scheduler collector: duplicate metric key %q, overwriting", k)
			}
			m[k] = v
		}
	}

	toFloat64 := func(v any) float64 {
		switch n := v.(type) {
		case int:
			return float64(n)
		case int64:
			return float64(n)
		case uint64:
			return float64(n)
		case float64:
			return n
		default:
			log.G(ctx).Warnf("scheduler collector: unexpected metric value type %T", v)
			return 0
		}
	}

	emit := func(desc *prom.Desc, value float64) {
		if metric, err := prom.NewConstMetric(desc, prom.GaugeValue, value); err == nil {
			ch <- metric
		} else {
			log.G(ctx).Errorf("scheduler collector: emit metric error: %v", err)
		}
	}

	if v, ok := m["quota_cpu_usage"]; ok {
		emit(c.descQuotaCPUMilli, toFloat64(v))
	}
	if v, ok := m["quota_mem_mb_usage"]; ok {
		emit(c.descQuotaMemMB, toFloat64(v))
	}
	if v, ok := m["mvm_num"]; ok {
		emit(c.descMvmNum, toFloat64(v))
	}
	if v, ok := m["mvm_running_num"]; ok {
		emit(c.descMvmRunningNum, toFloat64(v))
	}
	if v, ok := m["nic_queues"]; ok {
		emit(c.descNicQueues, toFloat64(v))
	}

	// Realtime create/destroy counts come from the workflow engine's atomic
	// counters rather than the OSS collector registry, because these are
	// per-request flying counts that the cubebox service does not own.
	emit(c.descRealtimeCreateNum, float64(c.workflowEngine.GetFlowOnFlyingRequests("create")))
	emit(c.descRealtimeDestroyNum, float64(c.workflowEngine.GetFlowOnFlyingRequests("destroy")))
}

func initPrometheusMetrics(register *metrictypes.CollectRegister, engine *workflow.Engine) {
	ns := metrics.NewNamespace(namespace, subsystem, nil)
	ns.Add(newSchedulerCollector(register, engine))
	metrics.Register(ns)
}
