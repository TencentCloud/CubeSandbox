// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"context"
	"strings"
	"testing"
	"time"
)

func makeLogContext() context.Context {
	ctx := context.Background()
	sandBoxID := "7a945b0848a4c0a410b37dd734322078ea0de03609a5c52aa167cda2da044a6e"
	ctx = context.WithValue(ctx, KeyInstanceId, sandBoxID)
	ctx = context.WithValue(ctx, KeyNamespace, "default")
	ctx = context.WithValue(ctx, KeyCostTime, 12.3456789011111)
	return ctx
}

func BenchmarkLogger_Infof(b *testing.B) {
	ctx := makeLogContext()

	fields := Fields{"Module": "Shim"}
	logger := GetLogger("test")
	logger.SetOutput(&noopWriter{})
	e := logger.WithContext(ctx).WithFields(fields)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Infof("test log")
	}
}

func TestLogger_Infof(t *testing.T) {
	ctx := makeLogContext()

	fields := Fields{"Module": "Shim"}
	logger := GetLogger("test")
	w := &noopWriter{}
	logger.SetOutput(w)
	e := logger.WithContext(ctx).WithFields(fields)

	e.Infof("test log")
	assertReturnChar(w, t)
	e.Infof("test log2")
	assertReturnChar(w, t)
	if _, ok := e.data["Cluster"]; ok {
		t.Errorf("cluster should not be set,%+v", e.data["Cluster"])
	}
	if _, ok := e.data["CalleeCluster"]; ok {
		t.Errorf("callee cluster should not be set,%+v", e.data["CalleeCluster"])
	}

	ctx = context.WithValue(ctx, KeyCluster, "default")
	ctx = context.WithValue(ctx, KeyCalleeCluster, "ap-guangzhou")
	e2 := logger.WithContext(ctx)
	e2.Info("test log3")
	if e2.data["Cluster"] != "default" {
		t.Errorf("cluster not set")
	}
	if e2.data["CalleeCluster"] != "ap-guangzhou" {
		t.Errorf("callee cluster not set")
	}
}

func TestTraceEnds(t *testing.T) {
	EnableLogMetric()
	w := &noopWriter{}
	SetTraceOutput(w)

	trace := RequestTrace{
		RequestID:      "test-req",
		Action:         "CreateFunction",
		Timestamp:      time.Now(),
		Caller:         "Testcubelog",
		Callee:         "Testcubelog",
		CallerIP:       LocalIP,
		CalleeEndpoint: "127.0.0.1",
		CalleeAction:   "invoke",
		ErrorCode:      CodeInternalError,
		SubErrorCode:   "NetworkUnreachable",
		Cost:           time.Minute,
		RetCode:        200,
	}

	Trace(&trace)
	assertReturnChar(w, t)
	if strings.Contains(string(w.Out), "Cluster") || strings.Contains(string(w.Out), "ap-guangzhou") {
		t.Errorf("expect contains Cluster and ap-guangzhou")
	}

	trace2 := RequestTrace{
		RequestID:      "test-req",
		Action:         "CreateFunction",
		Timestamp:      time.Now(),
		Caller:         "Testcubelog",
		Callee:         "Testcubelog",
		CallerIP:       LocalIP,
		CalleeEndpoint: "127.0.0.1",
		CalleeAction:   "invoke",
		ErrorCode:      CodeInternalError,
		SubErrorCode:   "NetworkUnreachable",
		Cost:           time.Minute,
		RetCode:        200,
		Cluster:        "default",
		CalleeCluster:  "ap-guangzhou",
	}
	Trace(&trace2)
	assertReturnChar(w, t)
	if !strings.Contains(string(w.Out), "Cluster") || !strings.Contains(string(w.Out), "ap-guangzhou") {
		t.Errorf("expect contains Cluster and ap-guangzhou")
	}
}

func assertReturnChar(w *noopWriter, t *testing.T) {
	if len(w.Out) == 0 {
		t.Errorf("expect logs")
	}
	if w.Out[len(w.Out)-1] != '\n' {
		t.Errorf("show end with '\\n'")
	}
	return
}

type noopWriter struct {
	Out []byte
}

func (w *noopWriter) Write(p []byte) (n int, err error) {
	w.Out = p
	return len(p), nil
}
