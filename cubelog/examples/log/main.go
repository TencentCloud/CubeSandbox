// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package test

import (
	"context"
	"fmt"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"

	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func initContext() context.Context {
	ctx := context.Background()

	ctx = context.WithValue(ctx, cubelog.KeyRequestID, "xxxxxxxx-xxxxxx")

	ctx = context.WithValue(ctx, cubelog.KeyAction, "Test")

	ctx = context.WithValue(ctx, cubelog.KeyCaller, "TestCaller")

	ctx = context.WithValue(ctx, cubelog.KeyCallee, "Pydes")

	ctx = context.WithValue(ctx, cubelog.KeyCallerIp, cubelog.LocalIP)

	ctx = context.WithValue(ctx, cubelog.KeyCalleeEndpoint, "127.0.0.1")

	ctx = context.WithValue(ctx, cubelog.KeyCalleeAction, "Create")

	ctx = context.WithValue(ctx, cubelog.KeyCostTime, 10.1)

	return ctx
}

func main() {

	cubelog.SetModuleName("Testcubelog")

	cubelog.EnableFileLog()

	cubelog.SetLevel(cubelog.DEBUG)

	cubelog.EnableLogMetric()

	logPath := "/data/组件名/log"

	cubelog.Create(logPath)

	traceLogName := "cubu-test-stat.log"

	cubelog.SetTraceOutput(cubelog.NewRollFileWriter(logPath, traceLogName, 10, 100))

	logName := "cubu-test-req.log"

	reqLogWriter, _ := rotatelogs.New(
		fmt.Sprintf("%s.%%Y%%m%%d%%H", fmt.Sprintf("%s/%s", logPath, logName)),
		rotatelogs.WithMaxAge(3*time.Hour), rotatelogs.WithRotationTime(time.Hour),
	)

	cubelog.SetOutput(reqLogWriter)

	cubelog.Debugf("hello world")
	cubelog.Info("hello cubebox")

	cubelog.Fatalf("fatal!")

	ctx := initContext()

	cubelog.WithContext(ctx).Debugf("hello world")

	cubelog.WithFields(cubelog.Fields{
		"A": "B", "B": 1,
	}).Debugf("hello world")

	now := time.Now()
	time.Sleep(10 * time.Millisecond)
	trace := cubelog.RequestTrace{
		Action:         "CreateFunction",
		Timestamp:      time.Now(),
		Caller:         "Testcubelog",
		Callee:         "Testcubelog",
		CallerIP:       cubelog.LocalIP,
		CalleeEndpoint: "127.0.0.1",
		CalleeAction:   "invoke",
		ErrorCode:      cubelog.CodeInternalError,
		SubErrorCode:   "NetworkUnreachable",
		Cost:           time.Since(now),
		RetCode:        200,
	}
	time.Sleep(time.Second)
	cubelog.Trace(&trace)
}
