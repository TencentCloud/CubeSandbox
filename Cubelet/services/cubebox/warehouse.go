// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/warehouse"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// StartWarehouseSync wires CubeOps download into Ensure and starts inventory
// writeback + preinstall polling. No-op when cubeops_addr is unset.
func StartWarehouseSync(stopCh <-chan struct{}, addr string, timeout time.Duration) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cm := getComponentManager()
	client := warehouse.NewClient(addr, warehouse.NodeID(), warehouse.NodeArch(), timeout)
	fetcher := warehouse.NewFetcher(client, cm.Config().VersionedBaseDir)
	cm.SetFetcher(fetcher)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		fetcher.ScanAndReport(ctx)
	}()

	log.G(context.Background()).WithFields(CubeLog.Fields{
		"mod": "warehouse", "cubeops_addr": addr,
	}).Info("component warehouse download enabled")

	go warehouse.RunPreinstallLoop(stopCh, client, fetcher, 30*time.Second)
}
