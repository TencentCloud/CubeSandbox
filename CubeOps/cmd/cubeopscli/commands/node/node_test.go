// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// captureStdout runs fn and returns everything written to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	fn()
	_ = w.Close()
	data, _ := io.ReadAll(r)
	_ = r.Close()
	return string(data)
}

func sampleSchedulerNodes() []*model.SchedulerNode {
	return []*model.SchedulerNode{
		{
			InsID: "node-1", IP: "10.0.0.1", InstanceType: "cubebox",
			Healthy: true, SchedulingDisabled: false, HostStatus: "RUNNING",
			LocalTemplates: []string{"tpl-a", "tpl-b"},
		},
		{
			InsID: "node-2", IP: "10.0.0.2", InstanceType: "cubebox",
			Healthy: false, SchedulingDisabled: true, HostStatus: "RUNNING",
		},
	}
}

func TestStripLocalTemplates(t *testing.T) {
	in := sampleSchedulerNodes()
	out := stripLocalTemplates(in)

	// Output nodes have LocalTemplates cleared.
	for _, n := range out {
		assert.Nil(t, n.LocalTemplates)
	}

	// Original nodes are not modified.
	assert.Equal(t, []string{"tpl-a", "tpl-b"}, in[0].LocalTemplates)
}

func TestIsolationAction(t *testing.T) {
	assert.Equal(t, "isolated", isolationAction("PUT"))
	assert.Equal(t, "unisolated", isolationAction("DELETE"))
	assert.Equal(t, "unisolated", isolationAction("POST"))
}

func TestFormatNodeTime(t *testing.T) {
	t.Run("zero returns dash", func(t *testing.T) {
		assert.Equal(t, "-", formatNodeTime(time.Time{}))
	})
	t.Run("valid returns formatted", func(t *testing.T) {
		ts := time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC)
		assert.Contains(t, formatNodeTime(ts), "2026")
	})
}

func TestPrintNodeSummary_Default(t *testing.T) {
	out := captureStdout(t, func() {
		printNodeSummary(sampleSchedulerNodes(), false, false)
	})
	for _, want := range []string{"NODE_ID", "NODE_IP", "node-1", "10.0.0.1", "RUNNING"} {
		assert.True(t, strings.Contains(out, want), "missing %q in:\n%s", want, out)
	}
	assert.False(t, strings.Contains(out, "LOCAL_TEMPLATES"))
}

func TestPrintNodeSummary_ShowLocalTemplates(t *testing.T) {
	out := captureStdout(t, func() {
		printNodeSummary(sampleSchedulerNodes(), false, true)
	})
	assert.True(t, strings.Contains(out, "LOCAL_TEMPLATES"))
}

func TestPrintNodeSummary_ScoreOnly(t *testing.T) {
	nodes := []*model.SchedulerNode{
		{InsID: "node-1", Score: 0.75, MetricUpdate: time.Now()},
	}
	out := captureStdout(t, func() {
		printNodeSummary(nodes, true, false)
	})
	for _, want := range []string{"NODE_ID", "SCORE", "METRIC_UPDATE", "node-1", "0.7500"} {
		assert.True(t, strings.Contains(out, want), "missing %q in:\n%s", want, out)
	}
}
