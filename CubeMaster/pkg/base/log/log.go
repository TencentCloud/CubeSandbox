// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package log provides log for cube master.
package log

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func Init(conf *Conf) {
	CubeLog.SetRegion(CubeLog.Region(conf.Region))
	CubeLog.SetModuleName(conf.Module)
	CubeLog.SetCluster(conf.Cluster)

	CubeLog.EnableFileLog()
	CubeLog.SetLevel(CubeLog.StringToLevel(conf.Level))
	CubeLog.SetSkipCallerDepth(0)
	CubeLog.SetCallerPrettyfier(CubeLog.SuccinctCallerPath)
	if IsDebug() {
		CubeLog.SetReportCaller(true)
	}
	if conf.EnableLogMetric {
		CubeLog.EnableLogMetric()
	}
	CubeLog.SetVersion(version.ShowVersion())

	logPath := conf.Path
	if logPath == "" {
		logPath = "/data/log/" + conf.Module
	}
	CubeLog.Create(logPath)

	traceLogName := conf.Module + "-stat"
	traceLogWriter := CubeLog.NewRollFileWriter(logPath, traceLogName, conf.FileNum, conf.FileSize)
	CubeLog.SetTraceOutput(traceLogWriter)

	logName := conf.Module + "-req"
	reqLogWriter := CubeLog.NewRollFileWriter(logPath, logName, conf.FileNum, conf.FileSize)
	CubeLog.SetOutput(reqLogWriter)

	onionLogName := "onion" + "-req"
	onionStdoutLogger := CubeLog.GetLogger("onion")
	onionStdoutLogger.SetOutput(CubeLog.NewRollFileWriter(logPath, onionLogName, conf.FileNum, conf.FileSize))
}

func IsDebug() bool {
	return CubeLog.GetLevel() <= CubeLog.DEBUG
}

func OnChangeConf(c *Conf) {
	if c.EnableLogMetric {
		CubeLog.EnableLogMetric()
	}
	if !c.EnableLogMetric {
		CubeLog.DisableLogMetric()
	}
	CubeLog.SetLevel(CubeLog.StringToLevel(c.Level))
	if IsDebug() {
		CubeLog.SetReportCaller(true)
	} else {
		CubeLog.SetReportCaller(false)
	}
	fmt.Printf("setLogLevel:%+v\n", c.Level)
}

func TraceReport(reqID string, startTime time.Time, callee, calleeEndPoint, action string, retCode int) {
	trace := CubeLog.RequestTrace{
		RequestID:      reqID,
		Action:         action,
		Timestamp:      startTime,
		Caller:         CubeLog.GetModuleName(),
		Callee:         callee,
		CallerIP:       CubeLog.LocalIP,
		RetCode:        int64(retCode),
		ErrorCode:      CubeLog.CodeSuccess,
		CalleeEndpoint: calleeEndPoint,
		CalleeAction:   action,
		Cost:           time.Since(startTime),
	}
	CubeLog.Trace(&trace)
}

func ReportExt(ctx context.Context, callee, endpoint, action,
	calleeAction string, cost time.Duration, code int64) {

	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		return
	}

	rt = rt.DeepCopy()

	rt.Cost = cost
	if callee != "" {

		rt.Callee = callee
	}
	if endpoint != "" {

		rt.CalleeEndpoint = endpoint
	}
	if action != "" {

		rt.Action = action
	}
	if calleeAction != "" {

		rt.CalleeAction = calleeAction
	}
	rt.RetCode = int64(code)

	containerID, _ := ctx.Value(CubeLog.KeyContainerId).(string)
	ft, _ := ctx.Value(CubeLog.KeyFunctionType).(string)
	ns, _ := ctx.Value(CubeLog.KeyNamespace).(string)
	if containerID != "" {
		rt.ContainerID = containerID
	}
	if ft != "" {
		rt.FunctionType = ft
	}
	if ns != "" {
		rt.Namespace = ns
	}

	CubeLog.Trace(rt)
}

func Report(ctx context.Context, cost time.Duration, code int64) {
	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		return
	}

	rt = rt.DeepCopy()

	rt.RetCode = code
	rt.Cost = cost
	CubeLog.Trace(rt)
}
