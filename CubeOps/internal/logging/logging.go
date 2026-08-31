// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package logging initialises CubeOps logging with the shared cubelog library.
package logging

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// Options controls logger initialisation.
type Options struct {
	Level      string
	LogDir     string
	Module     string
	FileNum    int
	FileSizeMB int
}

type loggerKey struct{}

// Init configures cubelog and its rolling request/stat outputs.
func Init(opts Options) {
	module := strings.TrimSpace(opts.Module)
	if module == "" {
		module = "cubeops"
	}
	logDir := strings.TrimSpace(opts.LogDir)
	if logDir == "" {
		logDir = fmt.Sprintf("/data/log/%s", module)
	}

	// Output-independent config must run before the log-directory setup below;
	// EnableLogMetric gates Trace() emission, so the stdout fallback would
	// otherwise drop every trace.
	cubelog.SetModuleName(module)
	cubelog.SetVersion(version.ShowVersion())
	cubelog.EnableLogMetric()
	cubelog.SetLevel(parseCubeLogLevel(opts.Level))
	cubelog.SetSkipCallerDepth(0)
	cubelog.SetCallerPrettyfier(cubelog.SuccinctCallerPath)
	cubelog.SetReportCaller(parseCubeLogLevel(opts.Level) == cubelog.DEBUG)

	if err := os.MkdirAll(logDir, 0755); err != nil {
		G(context.Background()).Errorf("logging: failed to create log directory, falling back to stdout: dir=%s err=%v", logDir, err)
		cubelog.SetOutput(os.Stdout)
		cubelog.SetTraceOutput(os.Stdout)
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

	cubelog.EnableFileLog()
	cubelog.Create(logDir)
	cubelog.SetOutput(cubelog.NewRollFileWriter(logDir, module+"-req", fileNum, fileSize))
	cubelog.SetTraceOutput(cubelog.NewRollFileWriter(logDir, module+"-stat", fileNum, fileSize))
}

// WithLogger stores a cubelog entry in a context, matching CubeMaster's
// request-scoped logging pattern.
func WithLogger(ctx context.Context, entry *cubelog.Entry) context.Context {
	return context.WithValue(ctx, loggerKey{}, entry)
}

// G returns the cubelog entry for ctx, deriving it from RequestTrace when
// none was stored.
// Note: the entry snapshots RequestTrace fields, so early handler lines carry
// RetCode=0 even on later failure; only the -stat trace has the real value.
// Call ReNewLogger after mutating rt to pick up new values.
func G(ctx context.Context) *cubelog.Entry {
	if ctx == nil {
		ctx = context.Background()
	}
	if cached, ok := ctx.Value(loggerKey{}).(*cubelog.Entry); ok && cached != nil {
		return cached
	}
	return cubelog.WithContext(ctx)
}

// ReNewLogger rebuilds the context cubelog entry from the current
// RequestTrace. Call after mutating rt (e.g. setting InstanceID/RetCode) so
// later log lines pick up the new fields.
func ReNewLogger(ctx context.Context) *cubelog.Entry {
	return cubelog.WithContext(ctx)
}

func parseCubeLogLevel(s string) cubelog.LogLevel {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return cubelog.DEBUG
	case "WARN", "WARNING":
		return cubelog.WARN
	case "ERROR":
		return cubelog.ERROR
	default:
		return cubelog.INFO
	}
}
