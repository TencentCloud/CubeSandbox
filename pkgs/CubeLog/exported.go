// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"context"
	"io"
	"os"
)

var (
	std          *Logger
	reportCaller = false
)

func init() {

	std = GetLogger("")
}

func Create(path string) {
	var err error
	if _, err = os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(path, 0744)
	}
	if os.IsPermission(err) {
		panic(err)
	}
}

func GetDefaultLogger() *Logger {
	return std
}

func SetReportCaller(b bool) {
	reportCaller = b
}

func SetOutputLogger(l *Logger) {
	if l != nil {
		std = l
	}
}

func SetLogFormat(format logFormat) {
	std.SetLogFormat(format)
}

func SetOutput(w io.Writer) {
	std.writer = w
}

func SetCustomFields(fields Fields) {
	std.SetCustomFields(fields)
	traceStd.SetCustomFields(fields)
}

func GetCustomFields() Fields {
	return std.GetCustomFields()
}

func EnableFileLog() {
	std.EnableFileLog()
}

func WithContext(ctx context.Context) *Entry {
	return std.WithContext(ctx)
}

func WithFields(fields Fields) *Entry {
	return std.WithFields(fields)
}

func Debug(args ...interface{}) {
	std.Debug(args...)
}

func Info(args ...interface{}) {
	std.Info(args...)
}

func Warn(args ...interface{}) {
	std.Warn(args...)
}

func Error(args ...interface{}) {
	std.Error(args...)
}

func Fatal(args ...interface{}) {
	std.Fatal(args...)
}

func Debugf(format string, args ...interface{}) {
	std.Debugf(format, args...)
}

func Infof(format string, args ...interface{}) {
	std.Infof(format, args...)
}

func Warnf(format string, args ...interface{}) {
	std.Warnf(format, args...)
}

func Errorf(format string, args ...interface{}) {
	std.Errorf(format, args...)
}

func Fatalf(format string, args ...interface{}) {
	std.Fatalf(format, args...)
}
