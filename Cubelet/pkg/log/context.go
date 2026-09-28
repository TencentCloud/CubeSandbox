// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package log 提供统一的日志包装和上下文管理
//
// 核心功能：
// 1. CubeWrapperLogEntry - 对 CubeLog.Entry 的包装，提供便利的日志方法
// 2. Context 集成 - 支持通过 context 传递和获取 logger 实例
// 3. 日志桥接 - 将 klog、logrus、containerd log 统一到 CubeLog
//
// 使用示例：
//
//	// 获取当前上下文的 logger
//	logger := log.GetLogger(ctx)
//	logger.WithField("request_id", "123").Info("message")
//
//	// 创建新的上下文并关联 logger
//	newCtx := log.WithLogger(ctx, logger)
//	// 新上下文中的日志都会使用这个 logger
//
// 日志链路：
//
//	业务代码 (CubeLog.Entry)
//	    ↓
//	CubeWrapperLogEntry (本包)
//	    ↓
//	context 传递与关联
//	    ↓
//	klog/logrus/containerd log 桥接
//	    ↓
//	日志文件输出
package log

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var (
	G = GetLogger

	L = GetLogger(context.Background())
)

type loggerKey struct{}

type CubeWrapperLogEntry struct {
	*CubeLog.Entry
}

func NewWrapperLogEntry(entry *CubeLog.Entry) *CubeWrapperLogEntry {
	return &CubeWrapperLogEntry{Entry: entry}
}

func WithLogger(ctx context.Context, e *CubeWrapperLogEntry) context.Context {
	return context.WithValue(ctx, loggerKey{}, e)
}

func (w *CubeWrapperLogEntry) WithError(err error) *CubeWrapperLogEntry {
	if err == nil {
		return w
	}
	return w.WithFields(CubeLog.Fields{"error": err.Error()})
}

func (w *CubeWrapperLogEntry) WithField(key string, value any) *CubeWrapperLogEntry {
	return w.WithFields(CubeLog.Fields{key: value})
}

func (w *CubeWrapperLogEntry) WithFields(fields CubeLog.Fields) *CubeWrapperLogEntry {
	return NewWrapperLogEntry(w.Entry.WithFields(fields))
}

func (w *CubeWrapperLogEntry) Tracef(format string, v ...interface{}) {

}

func (w *CubeWrapperLogEntry) Info(args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		args = append(args, extraContent)
	}
	w.Entry.Info(args...)
}

func (w *CubeWrapperLogEntry) Infof(format string, args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		format = format + "%s"
		args = append(args, extraContent)
	}
	w.Entry.Infof(format, args...)
}

func (w *CubeWrapperLogEntry) Debug(args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		args = append(args, extraContent)
	}
	w.Entry.Debug(args...)
}

func (w *CubeWrapperLogEntry) Debugf(format string, args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		format = format + "%s"
		args = append(args, extraContent)
	}
	w.Entry.Debugf(format, args...)
}

func (w *CubeWrapperLogEntry) Warn(args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		args = append(args, extraContent)
	}
	w.Entry.Warn(args...)
}

func (w *CubeWrapperLogEntry) Warnf(format string, args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		format = format + "%s"
		args = append(args, extraContent)
	}
	w.Entry.Warnf(format, args...)
}

func (w *CubeWrapperLogEntry) Error(args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		args = append(args, extraContent)
	}
	w.Entry.Error(args...)
}

func (w *CubeWrapperLogEntry) Errorf(format string, args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		format = format + "%s"
		args = append(args, extraContent)
	}
	w.Entry.Errorf(format, args...)
}

func (w *CubeWrapperLogEntry) Fatal(args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		args = append(args, extraContent)
	}
	w.Entry.Fatal(args...)
}

func (w *CubeWrapperLogEntry) Fatalf(format string, args ...interface{}) {
	extraContent := w.extralFieldsToContent()
	if extraContent != "" {
		format = format + "%s"
		args = append(args, extraContent)
	}
	w.Entry.Fatalf(format, args...)
}

func GetLogger(ctx context.Context) *CubeWrapperLogEntry {
	logger := ctx.Value(loggerKey{})

	if logger == nil {

		return &CubeWrapperLogEntry{Entry: CubeLog.WithContext(ctx)}
	}
	if old, ok := logger.(*CubeWrapperLogEntry); ok {
		return old
	}
	return &CubeWrapperLogEntry{Entry: logger.(*CubeLog.Entry)}
}

func ReNewLogger(ctx context.Context) context.Context {
	old := ctx.Value(loggerKey{})
	if old == nil {
		return ctx
	}
	return WithLogger(ctx, NewWrapperLogEntry(CubeLog.WithContext(ctx)))
}
