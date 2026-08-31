// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type captureWriter struct {
	out []byte
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.out = append([]byte(nil), p...)
	return len(p), nil
}

func TestReportCallerSkipsCubelogFrames(t *testing.T) {
	cubelog.SetReportCaller(true)
	cubelog.SetSkipCallerDepth(0)
	cubelog.SetCallerPrettyfier(nil)
	t.Cleanup(func() {
		cubelog.SetReportCaller(false)
		cubelog.SetSkipCallerDepth(0)
		cubelog.SetCallerPrettyfier(nil)
	})

	logger := cubelog.GetLogger("caller-contract")
	w := &captureWriter{}
	logger.SetOutput(w)

	logFromThisTestFile(logger)

	if len(w.out) == 0 {
		t.Fatal("logger wrote nothing")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(w.out, &payload); err != nil {
		t.Fatalf("unmarshal log JSON: %v\nraw: %s", err, w.out)
	}
	codeLine, _ := payload["CodeLine"].(string)
	if codeLine == "" || codeLine == "errorCallerPath" {
		t.Fatalf("CodeLine = %q, want a caller path outside cubelog", codeLine)
	}
	if strings.Contains(codeLine, "logger.go") || strings.Contains(codeLine, "entry.go") || strings.Contains(codeLine, "exported.go") {
		t.Fatalf("CodeLine %q still points inside cubelog; cubelogPackage skip is broken", codeLine)
	}
	if !strings.Contains(codeLine, filepath.Base("caller_contract_test.go")) {
		t.Fatalf("CodeLine %q does not include caller_contract_test.go", codeLine)
	}
}

func logFromThisTestFile(logger *cubelog.Logger) {
	logger.Infof("caller fence")
}
