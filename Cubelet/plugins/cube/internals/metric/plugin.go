// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metric

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type Config struct {
	CLSReportInterval string `toml:"cls_report_interval"`
}

func defaultConfig() *Config {
	return &Config{
		CLSReportInterval: "10s",
	}
}

type Plugin struct {
	Config *Config
	HostId string
	HostIp string

	register *types.CollectRegister

	workflowEngine *workflow.Engine
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.CubeboxServicePlugin,
		ID:     constants.MetricID.ID(),
		Config: defaultConfig(),
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.WorkflowPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			l := &Plugin{
				register: types.NewCollectRegister(),
			}

			l.Config = ic.Config.(*Config)
			CubeLog.WithContext(ic.Context).Debugf("metric.Config: %+v ", l.Config)
			if l.Config.CLSReportInterval == "" {
				l.Config.CLSReportInterval = "10s"
			}

			plugins := ic.GetAll()
			for name, p := range plugins {
				i, err := p.Instance()
				if err != nil || i == nil {
					continue
				}
				mp, ok := i.(types.MetricProvider)
				if !ok {
					continue
				}

				CubeLog.Infof("register metric from plugin %q", p.Registration.ID)
				if err := mp.RegisterMetrics(l.register); err != nil {
					return nil, fmt.Errorf("plugin %q failed to register metric: %v", name, err)
				}
			}

			p, err := ic.GetByID(constants.WorkflowPlugin, constants.WorkflowID.ID())
			if err != nil {
				return nil, err
			}
			e, ok := p.(*workflow.Engine)
			if !ok {
				return nil, fmt.Errorf("not a workflow engine")
			}
			l.workflowEngine = e

			identity, err := utils.GetHostIdentity()
			if err != nil {
				return nil, err
			}
			l.HostId = identity.InstanceID

			initPrometheusMetrics(l.register, l.workflowEngine)

			rt := &CubeLog.RequestTrace{
				Action:    "Metric",
				Timestamp: time.Time{},
				Caller:    constants.MetricID.ID(),
			}
			ctx := CubeLog.WithRequestTrace(context.Background(), rt)
			ctx = log.ReNewLogger(ctx)
			go l.ReportCLS(ctx)
			return l, nil
		},
	})
}

func (l *Plugin) ReportCLS(ctx context.Context) {
	defer utils.Recover()
	t, err := time.ParseDuration(l.Config.CLSReportInterval)
	if err != nil {
		CubeLog.WithContext(ctx).Errorf("parse duration err:%v", err)
		return
	}

	jobs := l.register.Get(types.MetricTypeCLS)
	CubeLog.WithContext(ctx).Infof("There are %v cls metric jobs", len(jobs))
	if len(jobs) == 0 {
		return
	}

	ticker := time.NewTicker(t)
	defer ticker.Stop()
	for range ticker.C {
		clusterLabel := getClusterLabel()
		for _, job := range jobs {
			metricValue, err := func() (any, error) {
				defer utils.Recover()
				return job()
			}()
			if err != nil {
				log.G(ctx).Errorf("collect metric error: %v", err)
				continue
			}

			traces, ok := metricValue.([]*CubeLog.RequestTrace)
			if !ok {
				log.G(ctx).Fatalf("cls metric report not a *CubeLog.RequestTrace type")
				continue
			}
			for _, traceV := range traces {
				traceV.Caller = constants.MetricID.ID()
				traceV.CalleeCluster = clusterLabel
				CubeLog.Trace(traceV)
			}
		}
	}
}

func getClusterLabel() string {
	hostConfig := config.GetHostConf()
	if hostConfig != nil {
		return hostConfig.SchedulerLabel
	}
	return ""
}
