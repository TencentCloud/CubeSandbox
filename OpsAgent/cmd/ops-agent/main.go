// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// ops-agent is the node-local operations agent: it receives desired
// configuration from CubeOps (push), reconciles it on a timer (pull), and
// applies it to node-local files atomically. It never executes commands and
// never talks to CubeMaster.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"

	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/agent"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/config"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/upstream"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/version"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/tasks/quota"
)

func main() {
	configPath := flag.String("config", "/usr/local/services/cubetoolbox/ops-agent/conf/config.yaml", "config file path")
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.VersionString("ops-agent"))
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	initLogging(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	quotaDomain := quota.New(cfg.NodeID, cfg.DynamicconfPath, cfg.BackupKeep, upstream.New(cfg.CubeOpsURL))
	a := agent.New(cfg, quotaDomain)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		cubelog.Errorf("ops-agent exited: %v", err)
		os.Exit(1)
	}
	cubelog.Infof("ops-agent stopped")
}

func initLogging(cfg *config.Config) {
	cubelog.SetModuleName("ops-agent")
	cubelog.SetVersion(version.ShowVersion())
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		cubelog.SetOutput(os.Stdout)
		cubelog.SetTraceOutput(os.Stdout)
		return
	}
	cubelog.EnableFileLog()
	cubelog.Create(cfg.LogDir)
	cubelog.SetOutput(cubelog.NewRollFileWriter(cfg.LogDir, "ops-agent-req", 10, 100))
	cubelog.SetTraceOutput(cubelog.NewRollFileWriter(cfg.LogDir, "ops-agent-stat", 10, 100))
}
