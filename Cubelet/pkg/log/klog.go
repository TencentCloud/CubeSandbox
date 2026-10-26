// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"context"
	"fmt"
	"strings"

	containerdlog "github.com/containerd/log"
	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
	"k8s.io/klog/v2"

	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type klogToCubeLogWrite struct {
}

func (w *klogToCubeLogWrite) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	line := string(p)
	line = strings.TrimRight(line, "\n")

	level := line[0]

	fields := CubeLog.Fields{
		"from": "klog",
	}

	if idx := strings.Index(line, "] "); idx > 0 {
		headerPart := line[:idx]
		msgPart := line[idx+2:]

		if fileIdx := strings.LastIndex(headerPart, " "); fileIdx > 0 {
			fileInfo := headerPart[fileIdx+1:]
			if colonIdx := strings.LastIndex(fileInfo, ":"); colonIdx > 0 {
				fields["file"] = fileInfo[:colonIdx]
				fields["line"] = fileInfo[colonIdx+1:]
			}
		}

		line = msgPart
	}

	log := GetLogger(context.Background()).WithFields(fields)
	if len(line) > 0 {
		switch level {
		case 'I':
			log.Info(line)
		case 'W':
			log.Warn(line)
		case 'E':
			log.Error(line)
		case 'F':
			log.Fatal(line)
		}
	}

	return len(p), nil
}

type klogToCubeLog struct {
	name  string
	depth int
}

func (l *klogToCubeLog) Init(info logr.RuntimeInfo) {
	l.depth = info.CallDepth
}

func (l *klogToCubeLog) Enabled(level int) bool {
	return true
}

func (l *klogToCubeLog) Info(level int, msg string, keysAndValues ...interface{}) {
	fields := l.kvToFields(keysAndValues)
	if l.name != "" {
		fields["logger"] = l.name
	}
	fields["source"] = "klog"
	fields["level"] = level

	if level > 0 {
		G(context.Background()).WithFields(fields).Debug(msg)
	} else {
		G(context.Background()).WithFields(fields).Info(msg)
	}
}

func (l *klogToCubeLog) Error(err error, msg string, keysAndValues ...interface{}) {
	fields := l.kvToFields(keysAndValues)
	if l.name != "" {
		fields["logger"] = l.name
	}
	fields["source"] = "klog"
	if err != nil {
		fields["error"] = err.Error()
	}
	G(context.Background()).WithFields(fields).Error(msg)
}

func (l *klogToCubeLog) WithValues(keysAndValues ...interface{}) logr.LogSink {
	return &klogToCubeLog{
		name:  l.name,
		depth: l.depth,
	}
}

func (l *klogToCubeLog) WithName(name string) logr.LogSink {
	newName := name
	if l.name != "" {
		newName = l.name + "." + name
	}
	return &klogToCubeLog{
		name:  newName,
		depth: l.depth,
	}
}

func (l *klogToCubeLog) kvToFields(keysAndValues []interface{}) cubelog.Fields {
	fields := cubelog.Fields{}
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			key := fmt.Sprintf("%v", keysAndValues[i])
			fields[key] = keysAndValues[i+1]
		}
	}
	return fields
}

func SetKlog() {

	klog.SetOutput(&klogToCubeLogWrite{})

	klogLogr := &klogToCubeLog{}
	klog.SetLogger(logr.New(klogLogr))
}

func SetContainerdLog() {

	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: containerdlog.RFC3339NanoFixed,
	})

	containerdlog.SetLevel(containerdlog.DebugLevel.String())
	containerdlog.SetFormat(containerdlog.JSONFormat)

	logrus.AddHook(&cubeLogHook{})
	logrus.SetReportCaller(true)

	logrus.SetOutput(&noout{})
}

type noout struct{}

func (noout) Write(p []byte) (n int, err error) {
	return len(p), nil
}

type cubeLogHook struct{}

func (h *cubeLogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *cubeLogHook) Fire(entry *logrus.Entry) error {

	ctx := entry.Context
	if ctx == nil {
		ctx = context.Background()
	}

	cubeEntry := GetLogger(ctx)

	if len(entry.Data) > 0 {
		fields := make(cubelog.Fields, len(entry.Data)+1)
		for k, v := range entry.Data {
			if v == nil {
				continue
			}
			fields[k] = v
			if _, ok := v.(error); ok {
				fields[k] = v.(error).Error()
			}
		}

		fields["source"] = "containerd"
		cubeEntry = cubeEntry.WithFields(fields)
	} else {

		cubeEntry = cubeEntry.WithField("source", "containerd")
	}

	msg := entry.Message
	switch entry.Level {
	case logrus.PanicLevel, logrus.FatalLevel:

		cubeEntry.Fatal(msg)
	case logrus.ErrorLevel:
		cubeEntry.Error(msg)
	case logrus.WarnLevel:
		cubeEntry.Warn(msg)
	case logrus.InfoLevel:
		cubeEntry.Info(msg)
	case logrus.DebugLevel, logrus.TraceLevel:

		cubeEntry.Debug(msg)
	}

	return nil
}
