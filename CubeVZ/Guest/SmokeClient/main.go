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
		"printf cube-vz-envd-ok",
		cubesandbox.CommandOptions{Timeout: 10 * time.Second},
	)
	if err != nil {
		fail("run envd command", err)
	}
	if result.ExitCode != 0 || result.Stdout != "cube-vz-envd-ok" || result.Stderr != "" {
		fail("verify envd command", fmt.Errorf("unexpected result: %#v", result))
	}
	fmt.Println("PASS CubeSandbox Go SDK -> CubeVZ -> existing envd command")
}

func fail(operation string, err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %s: %v\n", operation, err)
	os.Exit(1)
}
