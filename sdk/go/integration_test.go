// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cubesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIntegrationHealthTemplateAndList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := integrationConfig(t)
	client := NewClient(cfg)

	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if health["status"] != "ok" {
		t.Fatalf("health status=%#v, want ok; full response=%#v", health["status"], health)
	}

	if cfg.TemplateID == "" {
		t.Fatal("integration config did not resolve a template ID")
	}

	list, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if list == nil {
		t.Fatal("List returned nil slice")
	}

	listV2, err := client.ListV2(ctx)
	if err != nil {
		t.Fatalf("ListV2 returned error: %v", err)
	}
	if listV2 == nil {
		t.Fatal("ListV2 returned nil slice")
	}
}

func TestIntegrationSandboxExecutionCommandsFilesAndErrors(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	sb := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout: DurationPtr(2 * time.Minute),
		EnvVars: map[string]string{
			"CUBE_GO_SDK_CREATE_ENV": "create-env-ok",
		},
		Metadata: map[string]string{
			"sdk":      "go",
			"scenario": "integration-execution",
		},
	})

	info, err := sb.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo returned error: %v", err)
	}
	if info.SandboxID != sb.SandboxID {
		t.Fatalf("GetInfo sandboxID=%q, want %q", info.SandboxID, sb.SandboxID)
	}
	if info.State == "" {
		t.Fatalf("GetInfo state is empty: %#v", info)
	}
	if info.Metadata != nil && info.Metadata["scenario"] != "integration-execution" {
		t.Fatalf("metadata scenario=%q", info.Metadata["scenario"])
	}

	assertListContainsSandbox(t, ctx, client, sb.SandboxID)

	var stdoutEvents, stderrEvents, resultEvents int
	exec, err := sb.RunCode(ctx, strings.Join([]string{
		"import os, sys",
		"print('stdout-one')",
		"print(os.environ.get('CUBE_GO_SDK_RUN_ENV', 'missing'))",
		"sys.stderr.write('stderr-one\\n')",
		"'result-one'",
	}, "\n"), RunCodeOptions{
		Envs: map[string]string{
			"CUBE_GO_SDK_RUN_ENV": "run-env-ok",
		},
		Timeout: 45 * time.Second,
		OnStdout: func(message OutputMessage) {
			stdoutEvents++
		},
		OnStderr: func(message OutputMessage) {
			stderrEvents++
			if !message.IsStderr {
				t.Errorf("stderr callback IsStderr=false for %#v", message)
			}
		},
		OnResult: func(Result) {
			resultEvents++
		},
	})
	if err != nil {
		t.Fatalf("RunCode returned error: %v", err)
	}
	if exec.Error != nil {
		t.Fatalf("RunCode execution error: %#v", exec.Error)
	}
	if !strings.Contains(strings.Join(exec.Logs.Stdout, ""), "stdout-one") {
		t.Fatalf("stdout missing expected marker: %#v", exec.Logs.Stdout)
	}
	if !strings.Contains(strings.Join(exec.Logs.Stdout, ""), "run-env-ok") {
		t.Fatalf("stdout missing run env marker: %#v", exec.Logs.Stdout)
	}
	if !strings.Contains(strings.Join(exec.Logs.Stderr, ""), "stderr-one") {
		t.Fatalf("stderr missing expected marker: %#v", exec.Logs.Stderr)
	}
	if exec.Text != "result-one" {
		t.Fatalf("execution text=%q, want result-one; results=%#v", exec.Text, exec.Results)
	}
	if stdoutEvents == 0 || stderrEvents == 0 || resultEvents == 0 {
		t.Fatalf("callbacks not invoked: stdout=%d stderr=%d result=%d", stdoutEvents, stderrEvents, resultEvents)
	}

	errExec, err := sb.RunCode(ctx, "raise ValueError('integration-boom')", RunCodeOptions{
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCode error scenario returned transport error: %v", err)
	}
	if errExec.Error == nil || errExec.Error.Name != "ValueError" || !strings.Contains(errExec.Error.Value, "integration-boom") {
		t.Fatalf("execution error mismatch: %#v", errExec.Error)
	}

	cmd, err := sb.Commands().Run(ctx, "printf 'cmd-out\\n'; >&2 printf 'cmd-err\\n'; exit 7", CommandOptions{
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Commands.Run returned error: %v", err)
	}
	if cmd.Stdout != "cmd-out\n" || cmd.Stderr != "cmd-err\n" || cmd.ExitCode != 7 {
		t.Fatalf("command result mismatch: %#v", cmd)
	}

	path := "/tmp/cubesandbox-go-sdk-integration.txt"
	writeFile, err := sb.Commands().Run(ctx, fmt.Sprintf("printf %%s file-content-ok > %s", path), CommandOptions{
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("write fixture file returned error: %v", err)
	}
	if writeFile.ExitCode != 0 {
		t.Fatalf("write fixture command failed: %#v", writeFile)
	}
	content, err := sb.Files().Read(ctx, path)
	if err != nil {
		t.Fatalf("Files.Read returned error: %v", err)
	}
	if content != "file-content-ok" {
		t.Fatalf("file content=%q", content)
	}
	if _, err := sb.Files().Read(ctx, "/tmp/cubesandbox-go-sdk-does-not-exist"); err == nil {
		t.Fatal("Files.Read missing file returned nil error")
	}

	_, err = client.Create(ctx, CreateOptions{TemplateID: "tpl-go-sdk-integration-missing-template"})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("missing template error=%v, want ErrTemplateNotFound", err)
	}
}

func TestIntegrationPauseConnectAndResumeExecution(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sb := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout: DurationPtr(3 * time.Minute),
		Metadata: map[string]string{
			"sdk":      "go",
			"scenario": "integration-pause-connect",
		},
	})

	wait := true
	if err := sb.Pause(ctx, PauseOptions{
		Wait:     &wait,
		Timeout:  90 * time.Second,
		Interval: 2 * time.Second,
	}); err != nil {
		t.Fatalf("Pause returned error: %v", err)
	}

	info, err := sb.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo after pause returned error: %v", err)
	}
	if info.State != "paused" {
		t.Fatalf("state after pause=%q, want paused", info.State)
	}

	resumed, err := client.Connect(ctx, sb.SandboxID)
	if err != nil {
		t.Fatalf("Connect after pause returned error: %v", err)
	}
	sb = resumed

	exec, err := sb.RunCode(ctx, "print('resumed-ok')\n'resumed-result'", RunCodeOptions{
		Timeout: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCode after connect returned error: %v", err)
	}
	if exec.Error != nil {
		t.Fatalf("RunCode after connect execution error: %#v", exec.Error)
	}
	if !strings.Contains(strings.Join(exec.Logs.Stdout, ""), "resumed-ok") || exec.Text != "resumed-result" {
		t.Fatalf("resume execution mismatch: text=%q stdout=%#v", exec.Text, exec.Logs.Stdout)
	}
}

func integrationConfig(t *testing.T) Config {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping CubeSandbox integration test in short mode")
	}

	cfg := NewConfigFromEnv()
	if os.Getenv("CUBE_API_URL") == "" && os.Getenv("E2B_API_URL") == "" {
		t.Skip("set CUBE_API_URL to run CubeSandbox integration tests")
	}
	if os.Getenv("CUBE_PROXY_PORT_HTTP") == "" {
		cfg.ProxyPortHTTP = 80
	}
	if os.Getenv("CUBE_SANDBOX_DOMAIN") == "" {
		cfg.SandboxDomain = "cube.app"
	}
	cfg.Timeout = 3 * time.Minute
	cfg.RequestTimeout = 10 * time.Second
	cfg = normalizeConfig(cfg)

	if cfg.TemplateID == "" {
		cfg.TemplateID = discoverReadyTemplate(t, cfg)
	}
	t.Logf("CubeSandbox integration target api=%s template=%s proxy=%s:%d domain=%s",
		cfg.APIURL, cfg.TemplateID, cfg.ProxyNodeIP, cfg.ProxyPortHTTP, cfg.SandboxDomain)
	return cfg
}

func discoverReadyTemplate(t *testing.T, cfg Config) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.APIURL+"/templates", nil)
	if err != nil {
		t.Fatalf("build templates request: %v", err)
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list templates from %s: %v", cfg.APIURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list templates HTTP %d", resp.StatusCode)
	}

	var templates []struct {
		TemplateID string `json:"templateID"`
		Status     string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&templates); err != nil {
		t.Fatalf("decode templates: %v", err)
	}
	for _, template := range templates {
		if template.TemplateID != "" && strings.EqualFold(template.Status, "READY") {
			return template.TemplateID
		}
	}
	if len(templates) > 0 && templates[0].TemplateID != "" {
		return templates[0].TemplateID
	}
	t.Fatalf("no templates found at %s; set CUBE_TEMPLATE_ID", cfg.APIURL)
	return ""
}

func createIntegrationSandbox(t *testing.T, ctx context.Context, client *Client, opts CreateOptions) *Sandbox {
	t.Helper()
	sb, err := client.Create(ctx, opts)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := sb.Kill(cleanupCtx); err != nil && !errors.Is(err, ErrSandboxNotFound) {
			t.Logf("cleanup kill sandbox %s failed: %v", sb.SandboxID, err)
		}
	})

	if sb.SandboxID == "" {
		t.Fatal("created sandbox has empty sandboxID")
	}
	if sb.GetHost(JupyterPort) == "" {
		t.Fatal("created sandbox returned empty data-plane host")
	}
	return sb
}

func assertListContainsSandbox(t *testing.T, ctx context.Context, client *Client, sandboxID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		list, err := client.ListV2(ctx)
		if err != nil {
			t.Fatalf("ListV2 returned error: %v", err)
		}
		for _, item := range list {
			if item.SandboxID == sandboxID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox %s not found in ListV2", sandboxID)
		}
		time.Sleep(time.Second)
	}
}
