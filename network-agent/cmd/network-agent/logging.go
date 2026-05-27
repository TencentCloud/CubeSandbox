// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"bytes"
	"context"
	stdlog "log"
	"os"
	"runtime/debug"
	"strings"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

const defaultLogDir = "/data/log/network-agent"
const defaultModuleName = "network-agent"
const defaultRollNum = 10
const defaultRollSizeMB = 500
const defaultLogLevel = "info"

type cubeLogStdWriter struct{}

func (w *cubeLogStdWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(bytes.TrimSpace(p)))
	if msg != "" {
		CubeLog.WithContext(context.Background()).Info(msg)
	}
	return len(p), nil
}

func initLogger(logDir, logLevel string, rollNum, rollSizeMB int) error {
	if strings.TrimSpace(logDir) == "" {
		logDir = defaultLogDir
	}
	if strings.TrimSpace(logLevel) == "" {
		logLevel = defaultLogLevel
	}
	if rollNum <= 0 {
		rollNum = defaultRollNum
	}
	if rollSizeMB <= 0 {
		rollSizeMB = defaultRollSizeMB
	}
	return initLoggerWithDir(logDir, logLevel, rollNum, rollSizeMB)
}

func initLoggerWithDir(logDir, logLevel string, rollNum, rollSizeMB int) error {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	CubeLog.SetModuleName(defaultModuleName)
	CubeLog.EnableFileLog()
	CubeLog.SetSkipCallerDepth(0)
	CubeLog.SetLevel(CubeLog.StringToLevel(strings.ToUpper(logLevel)))
	CubeLog.SetVersion(resolveVersion())
	CubeLog.Create(logDir)
	CubeLog.SetTraceOutput(CubeLog.NewRollFileWriter(logDir, defaultModuleName+"-stat", rollNum, rollSizeMB))
	CubeLog.SetOutput(CubeLog.NewRollFileWriter(logDir, defaultModuleName+"-req", rollNum, rollSizeMB))

	stdoutLogger := CubeLog.GetLogger("stdout")
	stdoutLogger.SetOutput(CubeLog.NewRollFileWriter(logDir, "stdout-req", rollNum, rollSizeMB))

	// Bridge existing stdlib log.Printf/Fatalf calls in network-agent internals to CubeLog.
	// Keep stdlib log flags empty so the message body isn't double-prefixed with timestamps.
	stdlog.SetOutput(&cubeLogStdWriter{})
	stdlog.SetFlags(0)
	// Get LocalIP safely
	localIP := "unknown"
	if CubeLog.LocalIP != "" {
		localIP = CubeLog.LocalIP
	}
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent logger initialized: module=%s version=%s localip=%s level=%s logpath=%s",
		defaultModuleName, resolveVersion(), localIP, strings.ToUpper(logLevel), logDir,
	)
	return nil
}

func resolveVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "dev"
}
