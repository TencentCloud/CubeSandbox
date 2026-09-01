// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCubelogPackageMatchesGoMod(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var modulePath string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "module ") {
			modulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan go.mod: %v", err)
	}
	if modulePath == "" {
		t.Fatal("go.mod has no module line")
	}
	if cubelogPackage != modulePath {
		t.Fatalf("cubelogPackage = %q, go.mod module = %q", cubelogPackage, modulePath)
	}
}

func TestGetLoggerSingletonAndDefault(t *testing.T) {
	a := GetLogger("contract-singleton")
	b := GetLogger("contract-singleton")
	if a == nil || a != b {
		t.Fatalf("GetLogger should return the same instance for the same name")
	}
	if GetDefaultLogger() == nil {
		t.Fatal("GetDefaultLogger returned nil")
	}
	if GetDefaultLogger() != GetLogger("") {
		t.Fatal("GetDefaultLogger should be GetLogger(\"\")")
	}
}

func TestSetLevelGetLevelAndStringToLevel(t *testing.T) {
	prev := GetLevel()
	t.Cleanup(func() { SetLevel(prev) })

	cases := []struct {
		in   string
		want LogLevel
	}{
		{"DEBUG", DEBUG},
		{"INFO", INFO},
		{"WARN", WARN},
		{"ERROR", ERROR},
		{"FATAL", FATAL},
		{"debug", DEBUG},
		{"info", INFO},
		{"warn", WARN},
		{"error", ERROR},
		{"fatal", FATAL},
		{"Info", INFO},
		{" INFO ", INFO},
		{"unknown", DEBUG},
		{"", DEBUG},
	}
	for _, tc := range cases {
		got := StringToLevel(tc.in)
		if got != tc.want {
			t.Errorf("StringToLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	SetLevel(ERROR)
	if GetLevel() != ERROR {
		t.Fatalf("GetLevel() = %v, want ERROR", GetLevel())
	}
}

func TestSetCustomFieldsRoundTrip(t *testing.T) {
	prev := GetCustomFields()
	t.Cleanup(func() { SetCustomFields(prev) })

	SetCustomFields(Fields{"Fence": "contract"})
	got := GetCustomFields()
	if got["Fence"] != "contract" {
		t.Fatalf("GetCustomFields() = %#v, want Fence=contract", got)
	}
}

func TestWithRequestTraceRoundTrip(t *testing.T) {
	trace := &RequestTrace{RequestID: "req-contract", Action: "Ping"}
	ctx := WithRequestTrace(context.Background(), trace)
	got := GetTraceInfo(ctx)
	if got == nil {
		t.Fatal("GetTraceInfo returned nil")
	}
	if got != trace {
		t.Fatalf("GetTraceInfo returned %#v, want the same RequestTrace", got)
	}
	if GetTraceInfo(context.Background()) != nil {
		t.Fatal("empty context should have no request trace")
	}
}

func TestPackageInfofWritesLogContent(t *testing.T) {
	w := &noopWriter{}
	prev := GetDefaultLogger().writer
	t.Cleanup(func() { SetOutput(prev) })
	SetOutput(w)

	Infof("package-level fence")
	if len(w.Out) == 0 {
		t.Fatal("package Infof wrote nothing")
	}
	if !strings.Contains(string(w.Out), "package-level fence") {
		t.Fatalf("log output missing content: %s", w.Out)
	}
}

func TestJSONContextFieldContract(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, KeyRequestID, "req-json-contract")
	ctx = context.WithValue(ctx, KeyAction, "CreateSandbox")
	ctx = context.WithValue(ctx, KeyCluster, "cls-a")
	ctx = context.WithValue(ctx, KeyCalleeCluster, "ap-guangzhou")
	ctx = context.WithValue(ctx, KeyInstanceId, "sbx-1")
	ctx = context.WithValue(ctx, KeyNamespace, "default")
	ctx = context.WithValue(ctx, KeyCostTime, 12.5)

	logger := GetLogger("json-field-contract")
	w := &noopWriter{}
	logger.SetOutput(w)
	entry := logger.WithContext(ctx)
	entry.Infof("json fence")

	wantData := map[string]interface{}{
		"RequestId":     "req-json-contract",
		"Action":        "CreateSandbox",
		"Cluster":       "cls-a",
		"CalleeCluster": "ap-guangzhou",
		"InstanceId":    "sbx-1",
		"Namespace":     "default",
	}
	for key, want := range wantData {
		if entry.data[key] != want {
			t.Fatalf("entry.data[%q] = %#v, want %#v", key, entry.data[key], want)
		}
	}
	if entry.data["CostTime"] != 12.5 {
		t.Fatalf("entry.data[CostTime] = %#v, want 12.5", entry.data["CostTime"])
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(w.Out, &payload); err != nil {
		t.Fatalf("unmarshal log JSON: %v\nraw: %s", err, w.Out)
	}
	for key, want := range wantData {
		if payload[key] != want {
			t.Fatalf("JSON %q = %#v, want %#v", key, payload[key], want)
		}
	}
	if payload["LogContent"] != "json fence" {
		t.Fatalf("JSON LogContent = %#v, want json fence", payload["LogContent"])
	}
	if payload["LogLevel"] != "INFO" {
		t.Fatalf("JSON LogLevel = %#v, want INFO", payload["LogLevel"])
	}
	if _, ok := payload["CostTime"]; !ok {
		t.Fatal("JSON missing CostTime")
	}
}
