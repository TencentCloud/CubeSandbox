// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metric

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
)

func (l *Plugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m := make(map[string]any)

	ctx := context.Background()

	jobs := l.register.Get(types.MetricTypeOSS)
	for _, job := range jobs {
		metricValue, err := job()
		if err != nil {
			log.G(ctx).Errorf("handler collect metric error: %v", err)
			continue
		}

		metricValueMap, ok := metricValue.(map[string]any)
		if !ok {
			log.G(ctx).Errorf("metric value is not map[string]interface{}")
			continue
		}

		for k, v := range metricValueMap {
			m[k] = v
		}
	}

	m["ins_id"] = l.HostId
	m["realtime_create_num"] = l.workflowEngine.GetFlowOnFlyingRequests("create")
	m["realtime_destroy_num"] = l.workflowEngine.GetFlowOnFlyingRequests("destroy")

	mb, err := json.Marshal(m)
	if err != nil {
		http.Error(w, "marshal metric error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(mb)
}
