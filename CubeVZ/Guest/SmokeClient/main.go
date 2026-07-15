// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := cubesandbox.NewClient(cubesandbox.NewConfigFromEnv())
	defer client.Close()
	sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{})
	if err != nil {
		fail("create sandbox", err)
	}
	defer sandbox.Kill(context.Background())

	result, err := sandbox.Commands().Run(
		ctx,
		"BB=/usr/local/sbin/busybox; "+
			"for i in $($BB seq 1 100); do "+
			"if $BB ip -4 addr show dev eth0 | $BB grep -q 'inet ' && "+
			"$BB ip route show default | $BB grep -q '^default '; then "+
			"printf cube-vz-envd-network-ok; exit 0; fi; "+
			"$BB sleep 0.05; done; "+
			"$BB ip -4 addr show dev eth0 >&2; $BB ip route show >&2; exit 1",
		cubesandbox.CommandOptions{Timeout: 15 * time.Second},
	)
	if err != nil {
		fail("run envd command", err)
	}
	if result.ExitCode != 0 || result.Stdout != "cube-vz-envd-network-ok" || result.Stderr != "" {
		fail("verify envd command", fmt.Errorf("unexpected result: %#v", result))
	}
	fmt.Println("PASS CubeSandbox Go SDK -> CubeVZ -> envd command + VZNAT DHCP")
}

func fail(operation string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %s: %v\n", operation, err)
	os.Exit(1)
}
