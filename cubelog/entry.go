// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"context"
	"fmt"

	"reflect"
	"runtime"
	"strings"
	"time"
)

type Entry struct {
	ctx    context.Context
	logger *Logger
	data   Fields
	err    string
}

type contextKey string

const (
	KeyRequestID      contextKey = "request_id"
	KeyAction         contextKey = "action"
	KeyCaller         contextKey = "caller"
	KeyCallee         contextKey = "callee"
	KeyCallerIp       contextKey = "caller_ip"
	KeyCalleeEndpoint contextKey = "callee_endpoint"
	KeyCalleeAction   contextKey = "callee_action"
	KeyRetCode        contextKey = "retcode"
	KeyCostTime       contextKey = "cost_time"

	KeyNamespace contextKey = "namespace"

	KeyInstanceId contextKey = "instance_id"

	KeyAppID contextKey = "app_id"

	KeyModuleVersion contextKey = "version"
	KeyContainerId   contextKey = "container_id"

	KeyRegion        contextKey = "region"
	KeyCluster       contextKey = "cluster"
	KeyCalleeCluster contextKey = "callee_cluster"
	KeyFunctionType  contextKey = "function_type"
	KeyDeployMode    contextKey = "deploy_mode"
	KeyInstanceType  contextKey = "instance_type"
)

func SuccinctCallerPath(f *runtime.Frame) string {
	s := strings.Split(f.File, "/")
	len := len(s)
	if len <= 1 {
		return f.File
	}
	return strings.Join(s[(len-2):], "/")
}

func (entry *Entry) WithFields(fields Fields) *Entry {
	data := make(Fields, len(entry.data)+len(fields))
	for k, v := range entry.data {
		data[k] = v
	}
	fieldErr := entry.err
	for k, v := range fields {
		isErrField := false
		if t := reflect.TypeOf(v); t != nil {
			switch t.Kind() {
			case reflect.Func:
				isErrField = true
			case reflect.Ptr:
				isErrField = t.Elem().Kind() == reflect.Func
			}
		}
		if isErrField {
			tmp := fmt.Sprintf("can not add field %q", k)
			if fieldErr != "" {
				fieldErr = entry.err + ", " + tmp
			} else {
				fieldErr = tmp
			}
		} else {
			data[k] = v
		}
	}
	return &Entry{ctx: entry.ctx, logger: entry.logger, data: data, err: fieldErr}
}

func NewEntry(logger *Logger) *Entry {
	return &Entry{
		ctx:    context.TODO(),
		logger: logger,
		data:   make(Fields, 7),
	}
}

func (entry *Entry) WithContext(ctx context.Context) *Entry {
	data := make(Fields, len(entry.data)+16)
	var builtinTag Fields
	if rt := GetTraceInfo(ctx); rt != nil {
		builtinTag = makeLogFieldsFromTrace(rt)
	} else {
		builtinTag = makeLogFields(ctx)
	}

	for k, v := range entry.data {
		data[k] = v
	}
	for k, v := range builtinTag {
		data[k] = v
	}

	return &Entry{ctx: ctx, logger: entry.logger, data: data, err: entry.err}
}

func getCallerPath() string {
	if !reportCaller {
		return ""
	}

	pcs := make([]uintptr, maximumCallerDepth)
	depth := runtime.Callers(minimumCallerDepth, pcs)
	frames := runtime.CallersFrames(pcs[:depth])

	sd := skipCallerDepth
	for f, again := frames.Next(); again; f, again = frames.Next() {
		pkg := getPackageName(f.Function)

		if pkg != cubelogPackage {
			if sd == 0 {
				if CallerPrettyfier != nil {
					file := CallerPrettyfier(&f)
					return fmt.Sprintf("%s:%d", file, f.Line)
				}
				return fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			sd = sd - 1
		}
	}

	return "errorCallerPath"
}

func makeLogFields(ctx context.Context) Fields {
	fields := make(Fields, 16)

	requestId, _ := ctx.Value(KeyRequestID).(string)
	fields["RequestId"] = requestId
	action, _ := ctx.Value(KeyAction).(string)
	fields["Action"] = action
	caller, _ := ctx.Value(KeyCaller).(string)
	fields["Caller"] = caller
	callee, _ := ctx.Value(KeyCallee).(string)
	fields["Callee"] = callee
	callerIp, _ := ctx.Value(KeyCallerIp).(string)
	fields["CallerIp"] = callerIp
	calleeEndPoint, _ := ctx.Value(KeyCalleeEndpoint).(string)
	fields["CalleeEndpoint"] = calleeEndPoint
	calleeAction, _ := ctx.Value(KeyCalleeAction).(string)
	fields["CalleeAction"] = calleeAction
	costTime, _ := ctx.Value(KeyCostTime).(float64)
	fields["CostTime"] = costTime
	retcode, _ := ctx.Value(KeyRetCode).(int64)
	fields["RetCode"] = retcode

	appId, _ := ctx.Value(KeyAppID).(int64)
	fields["AppId"] = appId
	namespace, _ := ctx.Value(KeyNamespace).(string)
	fields["Namespace"] = namespace

	instanceId, _ := ctx.Value(KeyInstanceId).(string)
	fields["InstanceId"] = instanceId

	version, _ := ctx.Value(KeyModuleVersion).(string)
	if version == "" {
		version = moduleVersion
	}

	fields["Version"] = version
	containerId, _ := ctx.Value(KeyContainerId).(string)
	fields["ContainerId"] = containerId
	functionType, _ := ctx.Value(KeyFunctionType).(string)
	fields["FunctionType"] = functionType

	if region, ok := ctx.Value(KeyRegion).(string); ok {
		fields["Region"] = region
	}
	if cluster, ok := ctx.Value(KeyCluster).(string); ok {
		fields["Cluster"] = cluster
	}
	if calleeCluster, ok := ctx.Value(KeyCalleeCluster).(string); ok {
		fields["CalleeCluster"] = calleeCluster
	}
	instanceType, _ := ctx.Value(KeyInstanceType).(string)
	fields["InstanceType"] = instanceType
	return fields
}

func makeLogFieldsFromTrace(rt *RequestTrace) Fields {
	fields := make(Fields, 11)
	fields["RequestId"] = rt.RequestID
	fields["Action"] = rt.Action
	fields["Caller"] = rt.Caller
	fields["Callee"] = rt.Callee
	fields["CalleeEndpoint"] = rt.CalleeEndpoint
	fields["CalleeAction"] = rt.CalleeAction
	fields["InstanceId"] = rt.InstanceID
	fields["RetCode"] = rt.RetCode
	version := rt.Version
	if version == "" {
		version = moduleVersion
	}
	fields["Version"] = version
	fields["InstanceType"] = rt.InstanceType

	if rt.AppID > 0 {
		fields["AppId"] = rt.AppID
	}
	if rt.Namespace != "" {
		fields["Namespace"] = rt.Namespace
	}
	if rt.ContainerID != "" {
		fields["ContainerId"] = rt.ContainerID
	}
	if rt.FunctionType != "" {
		fields["FunctionType"] = rt.FunctionType
	}
	if rt.Region != "" {
		fields["Region"] = rt.Region
	}
	if rt.InstanceType != "" {
		fields["InstanceType"] = rt.InstanceType
	}
	if rt.Cluster != "" {
		fields["Cluster"] = rt.Cluster
	}
	if rt.CalleeCluster != "" {
		fields["CalleeCluster"] = rt.CalleeCluster
	}
	return fields
}

func (entry *Entry) writef(ctx context.Context, level LogLevel, format string, v []interface{}) {
	_, ok := entry.data["RegionInvokeLog"]
	if !ok && level < logLevel {
		return
	}

	fields := make(Fields, 6+len(entry.data))
	fields["LocalIp"] = LocalIP
	fields["Module"] = moduleName
	fields["CodeLine"] = getCallerPath()
	now := time.Now()
	fields["@timestamp"] = now.Format(time.RFC3339Nano)
	fields["LogLevel"] = level.String()

	for k, v := range entry.data {
		fields[k] = v
	}

	if format == "" {
		fields["LogContent"] = fmt.Sprint(v...)
	} else {
		fields["LogContent"] = fmt.Sprintf(format, v...)
	}

	var bs []byte
	var err error
	l := entry.logger
	switch l.format {
	case JSONFormat:
		bs, err = jsonCodec.Marshal(fields)
		if err != nil {
			return
		}
		bs = append(bs, '\n')
	case TextFormat:
		buf := getBuffer()
		defer putBuffer(buf)
		fmt.Fprintf(buf, "%s|%s|%s|%s|%s|%s|%s|%s|%s|%s", now.Format("2006-01-02 15:04:05.000"), fields["Region"],
			fields["CodeLine"], level.String(), fields["RequestId"], fields["Module"], LocalIP, fields["ErrorCode"],
			fields["SubErrorCode"], fields["LogContent"])
		buf.WriteString("\n")
		bs = buf.Bytes()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.debug {
		switch config.Net {
		case CloudVpc:

			select {
			case logQueue <- &logValue{value: bs, logger: l}:
			default:

			}
		default:
			if config.asyncFlush {
				select {
				case logQueue <- &logValue{value: bs, logger: l}:
				default:

				}
			} else {
				l.writer.Write(bs)
			}
		}
	}
}

func (entry *Entry) Debug(v ...interface{}) {
	entry.writef(entry.ctx, DEBUG, "", v)
}

func (entry *Entry) Info(v ...interface{}) {
	entry.writef(entry.ctx, INFO, "", v)
}

func (entry *Entry) Warn(v ...interface{}) {
	entry.writef(entry.ctx, WARN, "", v)
}

func (entry *Entry) Error(v ...interface{}) {
	entry.writef(entry.ctx, ERROR, "", v)
}

func (entry *Entry) Fatal(v ...interface{}) {
	entry.writef(entry.ctx, FATAL, "", v)
}

func (entry *Entry) Debugf(format string, v ...interface{}) {
	entry.writef(entry.ctx, DEBUG, format, v)
}

func (entry *Entry) Infof(format string, v ...interface{}) {
	entry.writef(entry.ctx, INFO, format, v)
}

func (entry *Entry) Warnf(format string, v ...interface{}) {
	entry.writef(entry.ctx, WARN, format, v)
}

func (entry *Entry) Errorf(format string, v ...interface{}) {
	entry.writef(entry.ctx, ERROR, format, v)
}

func (entry *Entry) Fatalf(format string, v ...interface{}) {
	entry.writef(entry.ctx, FATAL, format, v)
}

func (entry *Entry) GetFields() Fields {
	return entry.data
}
