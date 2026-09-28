// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/urfave/cli/v2"
)

func initCubeLog(context *cli.Context, module, logPath string) {
	cubelog.SetModuleName(module)
	cubelog.EnableFileLog()
	cubelog.SetSkipCallerDepth(0)
	cubelog.SetLevel(cubelog.StringToLevel(strings.ToUpper(context.String("log-level"))))

	cubelog.SetVersion(version.ShowVersion())
	cubelog.EnableLogMetric()

	if logPath == "" {
		logPath = fmt.Sprintf("/data/log/%s", module)
	}
	cubelog.Create(logPath)
	traceLogName := fmt.Sprintf("%s-stat", module)

	num := context.Int("log-roll-num")
	size := context.Int("log-roll-size")
	cubelog.SetTraceOutput(cubelog.NewRollFileWriter(logPath, traceLogName, num, size))
}

func reportCls(cost int64, callee, action string, podID, reqID string) {
	if cost <= 0 {
		return
	}

	req := cubelog.RequestTrace{
		Caller:     "Cubecli",
		RetCode:    200,
		Callee:     callee,
		Action:     action,
		Cost:       time.Duration(cost) * time.Microsecond,
		InstanceID: podID,
		RequestID:  reqID,
	}
	cubelog.Trace(&req)
}
