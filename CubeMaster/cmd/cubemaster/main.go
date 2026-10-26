// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"flag"
	stdlog "log"
	"runtime/debug"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemaster/app"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/integration"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
)

var (
	versionFlag     = flag.Bool("v", false, "show version")
	longVersionFlag = flag.Bool("version", false, "show version")
)

func main() {
	flag.Parse()
	if *versionFlag || *longVersionFlag {
		version.ShowAndExit(true)
	}

	debug.SetGCPercent(90)
	app := app.New()

	cfg, err := config.Init()
	if err != nil {
		stdlog.Fatalf("config init fail:%v", recov.DumpStacktrace(3, err))
		return
	}

	if cfg.Common.MockDebug {
		integration.MockInit()
	}
	app.Run()
}
