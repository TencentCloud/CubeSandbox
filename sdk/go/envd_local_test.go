// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestDataPlaneAgainstLocalEnvd drives the real SDK data plane — Connect
// framing, exit-code recovery, and the /files API — against a cube-envd that is
// listening on CUBE_ENVD_LOCAL_ADDR (host:port). It is skipped unless that
// variable is set, so `go test ./...` stays hermetic.
//
// Run it locally with:
//
//	docker run -d --rm -p 49983:49983 cubesandbox-base:local
//	CUBE_ENVD_LOCAL_ADDR=127.0.0.1:49983 go test -run TestDataPlaneAgainstLocalEnvd -v ./...
func TestDataPlaneAgainstLocalEnvd(t *testing.T) {
	addr := os.Getenv("CUBE_ENVD_LOCAL_ADDR")
	if addr == "" {
		t.Skip("set CUBE_ENVD_LOCAL_ADDR=host:port to run against a local cube-envd")
	}
	host, portString, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("CUBE_ENVD_LOCAL_ADDR = %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("CUBE_ENVD_LOCAL_ADDR port = %q: %v", portString, err)
	}

	client := NewClient(Config{
		ProxyNodeIP:    host,
		ProxyPortHTTP:  port,
		SandboxDomain:  "cube.app",
		RequestTimeout: 15 * time.Second,
	})
	defer client.Close()

	sandbox := &Sandbox{SandboxID: "local-envd"}
	client.attachSandbox(sandbox)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Scenario: command execution.
	result, err := sandbox.Commands().Run(ctx, "printf sdk-hello; printf sdk-err >&2; exit 3", CommandOptions{})
	if err != nil {
		t.Fatalf("Commands.Run: %v", err)
	}
	if result.Stdout != "sdk-hello" {
		t.Fatalf("stdout = %q, want %q", result.Stdout, "sdk-hello")
	}
	if result.Stderr != "sdk-err" {
		t.Fatalf("stderr = %q, want %q", result.Stderr, "sdk-err")
	}
	if result.ExitCode != 3 {
		t.Fatalf("exitCode = %d, want 3", result.ExitCode)
	}

	// Scenario: file write + read + stat.
	path := "/tmp/cube-envd-sdk-check.txt"
	if err := sandbox.Files().Write(ctx, path, []byte("sdk-file-body")); err != nil {
		t.Fatalf("Files.Write: %v", err)
	}
	content, err := sandbox.Files().Read(ctx, path)
	if err != nil {
		t.Fatalf("Files.Read: %v", err)
	}
	if content != "sdk-file-body" {
		t.Fatalf("file content = %q, want %q", content, "sdk-file-body")
	}
	entry, err := sandbox.Files().Stat(ctx, path)
	if err != nil {
		t.Fatalf("Files.Stat: %v", err)
	}
	if entry.Type != "FILE_TYPE_FILE" {
		t.Fatalf("stat type = %q, want FILE_TYPE_FILE", entry.Type)
	}
}
