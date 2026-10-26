// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"flag"
	stdlog "log"
	"runtime/debug"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/cmd/templatecenter/app"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
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
	a := app.New()

	// MUST run before config.Init: the shared config loader reads its path
	// variable during Init, and the shared artifact path helpers read theirs on
	// the first build. Both are consumed by CubeMaster code that cannot be
	// renamed in place, so this publishes the CUBE_TEMPLATE_CENTER_* values under
	// the legacy names those readers expect. Any deprecation notice it records is
	// logged by app.Run once the logger exists.
	tcconfig.ApplySharedEnvAliases()

	if _, err := config.Init(); err != nil {
		stdlog.Fatalf("config init fail:%v", recov.DumpStacktrace(3, err))
		return
	}

	a.Run()
}
