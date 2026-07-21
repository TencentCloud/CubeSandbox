// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// cube-volume-cos-rpc — COS VolumePlugin (gRPC Controller + Node, COS Go SDK).
//
// Usage:
//
//	cube-volume-cos-rpc serve
//
// Implements:
//   - VolumeControllerService (CubeMaster): Create / Destroy via COS API
//   - VolumePluginService (Cubelet): Attach / Detach via cosfs
//
// Config: /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf (or $CUBE_COS_CONFIG)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/grpcsrv"
)

func main() {
	log.SetPrefix("[cube-volume-cos-rpc] ")
	log.SetFlags(0)

	if len(os.Args) < 2 || os.Args[1] != "serve" {
		log.Fatalf("usage: %s serve", os.Args[0])
	}

	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := grpcsrv.Serve(ctx, cfg); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
