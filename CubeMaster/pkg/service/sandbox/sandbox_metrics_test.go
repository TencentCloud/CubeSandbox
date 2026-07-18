// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestMetricInRangeUsesUnixSecondsBoundaries(t *testing.T) {
	ts := time.Unix(1700000000, 999_000_000).UnixNano()

	assert.True(t, metricInRange(ts, 0, 0))
	assert.True(t, metricInRange(ts, 1700000000, 1700000000))
	assert.True(t, metricInRange(ts, 1699999999, 1700000001))
	assert.False(t, metricInRange(ts, 1700000001, 0))
	assert.False(t, metricInRange(ts, 0, 1699999999))
}

func TestSandboxMetricsResponseSerializesEmptyDataArray(t *testing.T) {
	body, err := json.Marshal(&types.GetSandboxMetricsRes{
		Data: []*types.SandboxMetricData{},
	})

	assert.NoError(t, err)
	assert.JSONEq(t, `{"data":[]}`, string(body))
}
