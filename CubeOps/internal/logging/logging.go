// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package logging initialises structured logging for CubeOps via the
// shared cubelog library, writing rolling files under the configured
// directory.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// Options controls logger initialisation.
type Options struct {
	// Level is the log level name: "debug", "info", "warn", "error".
	// Empty defaults to "info".
	Level string
	// LogDir is the directory for file logs. When empty, defaults to
	// "/data/log/CubeOps".
	LogDir string
	// Module is the cubelog module name used in log entries and file
	// prefixes. Defaults to "cubeops".
	Module string
	// FileNum is the number of rotated log files to retain.
	// Defaults to 10 when zero.
	FileNum int
	// FileSizeMB is the max size in MB of a single log file before
	// rotation. Defaults to 100 when zero.
	FileSizeMB int
}

// Init configures the global cubelog + slog logger from opts. It must be
// called once at program start, before any logging is done.
//
// cubelog owns the file writers, and a bridge slog handler routes Go
// slog calls into cubelog so all log lines land in the same rolling
// files instead of stdout.
func Init(opts Options) {
	module := strings.TrimSpace(opts.Module)
	if module == "" {
		module = "cubeops"
	}

	logDir := strings.TrimSpace(opts.LogDir)
	if logDir == "" {
		logDir = fmt.Sprintf("/data/log/%s", module)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		// Fall back to stdout-only if we cannot create the directory.
		slog.Error("logging: failed to create log directory, falling back to stdout", "dir", logDir, "error", err)
		return
	}

	fileNum := opts.FileNum
	if fileNum == 0 {
		fileNum = 10
	}
	fileSize := opts.FileSizeMB
	if fileSize == 0 {
		fileSize = 100
	}

	// ── Initialise cubelog (same as CubeMaster/Cubelet) ──────────────
	cubelog.SetModuleName(module)
	cubelog.EnableFileLog()
	cubelog.SetSkipCallerDepth(0)
	cubelog.SetLevel(parseCubeLogLevel(opts.Level))
	cubelog.Create(logDir)

	// Rolling file writers:
	//   <module>-req.log   — main business log
	//   <module>-stat.log  — trace/metric log
	reqLogName := fmt.Sprintf("%s-req", module)
	statLogName := fmt.Sprintf("%s-stat", module)

	reqLogWriter := cubelog.NewRollFileWriter(logDir, reqLogName, fileNum, fileSize)
	cubelog.SetOutput(reqLogWriter)

	statLogWriter := cubelog.NewRollFileWriter(logDir, statLogName, fileNum, fileSize)
	cubelog.SetTraceOutput(statLogWriter)

	// ── Bridge slog → cubelog ────────────────────────────────────────
	// CubeOps uses slog throughout its codebase. Route every slog call
	// through cubelog so the output lands in the same rolling files
	// instead of going to stdout.
	slog.SetDefault(slog.New(cubelogSlogHandler{}))
}

// cubelogSlogHandler is a minimal slog.Handler that forwards all records
// to cubelog. This keeps the existing slog.Info/Error/... call sites in
// CubeOps working unchanged while the actual I/O goes through cubelog's
// file writers.
type cubelogSlogHandler struct{}

func (cubelogSlogHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (cubelogSlogHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteByte(' ')
		sb.WriteString(a.Key)
		sb.WriteByte('=')
		sb.WriteString(a.Value.String())
		return true
	})
	msg := sb.String()

	switch {
	case r.Level >= slog.LevelError:
		cubelog.Errorf("%s", msg)
	case r.Level >= slog.LevelWarn:
		cubelog.Warnf("%s", msg)
	case r.Level >= slog.LevelInfo:
		cubelog.Infof("%s", msg)
	default:
		cubelog.Debugf("%s", msg)
	}
	return nil
}

func (cubelogSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return cubelogSlogHandler{} }
func (cubelogSlogHandler) WithGroup(_ string) slog.Handler      { return cubelogSlogHandler{} }

func parseCubeLogLevel(s string) cubelog.LogLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return cubelog.DEBUG
	case "warn", "warning":
		return cubelog.WARN
	case "error":
		return cubelog.ERROR
	default:
		return cubelog.INFO
	}
}
