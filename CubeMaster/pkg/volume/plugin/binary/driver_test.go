// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package binary

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
)

// TestMain also lets the test executable act as a real binary plugin without
// needing a shell or another compiler. Intercept plugin calls before m.Run
// parses Go's test flags.
func TestMain(m *testing.M) {
	if os.Getenv("CUBESANDBOX_TEST_BINARY_PLUGIN") == "1" {
		os.Exit(runTestBinaryPlugin())
	}
	os.Exit(m.Run())
}

func runTestBinaryPlugin() int {
	args, err := json.Marshal(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(os.Getenv("CUBESANDBOX_TEST_BINARY_ARGS"), args, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	flags := flag.NewFlagSet("test-volume-plugin", flag.ContinueOnError)
	op := flags.String("op", "", "operation")
	flags.String("volume-id", "", "volume ID")
	flags.String("name", "", "volume name")
	if os.Getenv("CUBESANDBOX_TEST_BINARY_LEGACY") != "1" {
		flags.String("private-data", "", "opaque state")
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if os.Getenv("CUBESANDBOX_TEST_BINARY_CUSTOM") == "1" {
		raw := os.Getenv("CUBESANDBOX_TEST_BINARY_STDOUT")
		fmt.Fprint(os.Stdout, raw)
		fmt.Fprint(os.Stderr, os.Getenv("CUBESANDBOX_TEST_BINARY_STDERR"))
		if ready := os.Getenv("CUBESANDBOX_TEST_BINARY_READY"); ready != "" {
			if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				return 2
			}
		}
		if os.Getenv("CUBESANDBOX_TEST_BINARY_WAIT") == "1" {
			for {
				time.Sleep(time.Hour)
			}
		}
		code, err := strconv.Atoi(os.Getenv("CUBESANDBOX_TEST_BINARY_EXIT"))
		if err != nil {
			return 2
		}
		return code
	}
	resp := map[string]string{"error": ""}
	switch *op {
	case "create":
		resp["token"] = "tok"
		resp["private_data"] = os.Getenv("CUBESANDBOX_TEST_BINARY_RESULT")
	case "destroy":
	default:
		fmt.Fprintln(os.Stderr, "unsupported operation")
		return 2
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newTestBinaryPlugin(t *testing.T, returnedData string) (*Plugin, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	argsPath := filepath.Join(t.TempDir(), "args.json")
	t.Setenv("CUBESANDBOX_TEST_BINARY_PLUGIN", "1")
	t.Setenv("CUBESANDBOX_TEST_BINARY_ARGS", argsPath)
	t.Setenv("CUBESANDBOX_TEST_BINARY_RESULT", returnedData)
	t.Setenv("CUBESANDBOX_TEST_BINARY_LEGACY", "")
	t.Setenv("CUBESANDBOX_TEST_BINARY_CUSTOM", "")
	t.Setenv("CUBESANDBOX_TEST_BINARY_READY", "")
	t.Setenv("CUBESANDBOX_TEST_BINARY_WAIT", "")
	return New("cos", executable), argsPath
}

func assertTestBinaryArgs(t *testing.T, path string, want []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plugin argv: %v", err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode plugin argv: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plugin argv = %q, want %q", got, want)
	}
}

func TestCreateParsesPrivateData(t *testing.T) {
	p, _ := newTestBinaryPlugin(t, "volumes/vol-1/")
	info, err := p.Create(context.Background(), "vol-1", "vol-1", "caller-input")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Token != "tok" {
		t.Fatalf("token=%q", info.Token)
	}
	if info.PrivateData != "volumes/vol-1/" {
		t.Fatalf("private_data=%q", info.PrivateData)
	}
	if info.PluginName != "cos" || info.VolumeID != "vol-1" || info.Name != "vol-1" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestCreateAllowsEmptyPrivateData(t *testing.T) {
	p, _ := newTestBinaryPlugin(t, "")
	info, err := p.Create(context.Background(), "vol-2", "vol-2", "caller-input")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.PrivateData != "" {
		t.Fatalf("private_data=%q; empty plugin output must not fall back to input", info.PrivateData)
	}
}

func TestPrivateDataArgs(t *testing.T) {
	for _, op := range []string{"create", "destroy"} {
		for _, tc := range []struct {
			name string
			data string
		}{
			{name: "empty"},
			{name: "json", data: `{"bucket":"test-bucket","prefix":"data/"}`},
			{name: "special", data: " \t桶/前缀 'single' \"double\" \\ $HOME $(echo literal)\n "},
			{name: "leading-hyphen", data: "--literal-value"},
		} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				p, argsPath := newTestBinaryPlugin(t, "plugin-output")
				want := []string{"--op", op, "--volume-id", "vol-args"}
				var err error
				if op == "create" {
					want = append(want, "--name", "vol-args")
					_, err = p.Create(context.Background(), "vol-args", "vol-args", tc.data)
				} else {
					err = p.Destroy(context.Background(), "vol-args", tc.data)
				}
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				if tc.data != "" {
					want = append(want, "--private-data", tc.data)
				}
				assertTestBinaryArgs(t, argsPath, want)
			})
		}
	}
}

// This controlled fixture documents the upgrade boundary: an old parser can
// reject the new flag, including when Create got no input but returned state.
func TestLegacyBinaryRejectsPrivateData(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		p, argsPath := newTestBinaryPlugin(t, "plugin-output")
		t.Setenv("CUBESANDBOX_TEST_BINARY_LEGACY", "1")
		_, err := p.Create(context.Background(), "vol-legacy", "vol-legacy", "caller-input")
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -private-data") {
			t.Fatalf("expected legacy parser to reject new Create flag, got %v", err)
		}
		assertTestBinaryArgs(t, argsPath, []string{
			"--op", "create", "--volume-id", "vol-legacy", "--name", "vol-legacy",
			"--private-data", "caller-input",
		})
	})
	t.Run("destroy-after-empty-create", func(t *testing.T) {
		p, argsPath := newTestBinaryPlugin(t, "plugin-output")
		t.Setenv("CUBESANDBOX_TEST_BINARY_LEGACY", "1")
		info, err := p.Create(context.Background(), "vol-legacy", "vol-legacy", "")
		if err != nil {
			t.Fatalf("legacy Create with no input: %v", err)
		}
		if info.PrivateData != "plugin-output" {
			t.Fatalf("unexpected legacy Create output: %q", info.PrivateData)
		}
		err = p.Destroy(context.Background(), "vol-legacy", info.PrivateData)
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -private-data") {
			t.Fatalf("expected legacy parser to reject new Destroy flag, got %v", err)
		}
		assertTestBinaryArgs(t, argsPath, []string{
			"--op", "destroy", "--volume-id", "vol-legacy", "--private-data", "plugin-output",
		})
	})
}

func TestPrivateDataNULDoesNotStartProcess(t *testing.T) {
	for _, op := range []string{"create", "destroy"} {
		t.Run(op, func(t *testing.T) {
			p, argsPath := newTestBinaryPlugin(t, "plugin-output")
			var err error
			if op == "create" {
				_, err = p.Create(context.Background(), "vol-nul", "vol-nul", "before\x00after")
			} else {
				err = p.Destroy(context.Background(), "vol-nul", "before\x00after")
			}
			if err == nil || !strings.Contains(err.Error(), "binary plugin \"cos\" "+op) {
				t.Fatalf("expected contextual error for NUL argument, got %v", err)
			}
			if _, statErr := os.Stat(argsPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("plugin must not start for a NUL argument; argv file stat = %v", statErr)
			}
		})
	}
}

func setTestBinaryResponse(t *testing.T, output, stderr string, exit int) {
	t.Helper()
	t.Setenv("CUBESANDBOX_TEST_BINARY_CUSTOM", "1")
	t.Setenv("CUBESANDBOX_TEST_BINARY_STDOUT", output)
	t.Setenv("CUBESANDBOX_TEST_BINARY_STDERR", stderr)
	t.Setenv("CUBESANDBOX_TEST_BINARY_EXIT", strconv.Itoa(exit))
}

// A normally exited plugin can provide a public caller error independently of
// its exit status. Internal stderr must never become part of that typed error.
func TestCreateErrorKindBoundary(t *testing.T) {
	const secret = "credential-file=/private/secret.conf"
	const public = "dynamic private_data requires cred_ref"
	for _, exit := range []int{0, 1} {
		t.Run(strconv.Itoa(exit), func(t *testing.T) {
			p, _ := newTestBinaryPlugin(t, "")
			setTestBinaryResponse(t, `{"error":"`+public+`","error_kind":"invalid_argument","token":"ignored","private_data":"ignored"}`, secret, exit)
			info, err := p.Create(context.Background(), "vol-kind", "vol-kind", "")
			var invalid *plugin.InvalidArgumentError
			if info != nil || !errors.As(err, &invalid) {
				t.Fatalf("expected typed rejection, info=%+v err=%v", info, err)
			}
			if invalid.Message != public {
				t.Fatalf("typed message=%q", invalid.Message)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "exit status") {
				t.Fatalf("typed error leaked execution diagnostics: %v", err)
			}
		})
	}
}

func TestCreateErrorKindFallback(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"legacy_error", `{"error":"backend unavailable"}`},
		{"unknown", `{"error":"failure","error_kind":"backend_down"}`},
		{"unknown_not_found", `{"error":"failure","error_kind":"not_found"}`},
		{"unknown_conflict", `{"error":"failure","error_kind":"conflict"}`},
		{"empty_kind", `{"error":"failure","error_kind":""}`},
		{"null_kind", `{"error":"failure","error_kind":null}`},
		{"numeric_kind", `{"error":"failure","error_kind":400}`},
		{"object_kind", `{"error":"failure","error_kind":{}}`},
		{"known_kind_without_error", `{"error":"","error_kind":"invalid_argument"}`},
		{"unknown_kind_without_error", `{"error":"","error_kind":"unknown"}`},
		{"null_kind_without_error", `{"error":"","error_kind":null}`},
		{"empty_kind_without_error", `{"error":"","error_kind":""}`},
		{"malformed_json", `{"error":"failure","error_kind":"invalid_argument"`},
		{"trailing_json", `{"error":"failure","error_kind":"invalid_argument"} {}`},
		{"trailing_garbage", `{"error":"failure","error_kind":"invalid_argument"} garbage`},
		{"non_string_error", `{"error":400,"error_kind":"invalid_argument"}`},
	} {
		for _, exit := range []int{0, 1} {
			t.Run(tc.name+"/"+strconv.Itoa(exit), func(t *testing.T) {
				p, _ := newTestBinaryPlugin(t, "")
				setTestBinaryResponse(t, tc.output, "backend diagnostic", exit)
				info, err := p.Create(context.Background(), "vol-kind", "vol-kind", "")
				var invalid *plugin.InvalidArgumentError
				if info != nil || err == nil || errors.As(err, &invalid) {
					t.Fatalf("expected generic rejection, info=%+v err=%v", info, err)
				}
				if exit != 0 && !strings.Contains(err.Error(), "backend diagnostic") {
					t.Fatalf("lost legacy stderr preference: %v", err)
				}
			})
		}
	}
}

func TestCreateErrorKindLegacyCompatibility(t *testing.T) {
	for _, output := range []string{
		`{"token":"tok","private_data":"state","error":""}`,
		`{"token":"tok","private_data":"state","error":"","future_field":{}}`,
	} {
		t.Run(output, func(t *testing.T) {
			p, _ := newTestBinaryPlugin(t, "")
			setTestBinaryResponse(t, output, "", 0)
			info, err := p.Create(context.Background(), "vol-legacy", "vol-legacy", "")
			if err != nil || info == nil || info.Token != "tok" || info.PrivateData != "state" {
				t.Fatalf("legacy success changed: info=%+v err=%v", info, err)
			}
		})
	}
	t.Run("stderr_only", func(t *testing.T) {
		p, _ := newTestBinaryPlugin(t, "")
		setTestBinaryResponse(t, "", "invalid_argument is only diagnostic text", 1)
		_, err := p.Create(context.Background(), "vol-kind", "vol-kind", "")
		var invalid *plugin.InvalidArgumentError
		if err == nil || errors.As(err, &invalid) || !strings.Contains(err.Error(), "only diagnostic text") {
			t.Fatalf("stderr must remain generic: %v", err)
		}
	})
	t.Run("stdout_diagnostic_fallback", func(t *testing.T) {
		p, _ := newTestBinaryPlugin(t, "")
		setTestBinaryResponse(t, "old stdout diagnostic", "", 1)
		_, err := p.Create(context.Background(), "vol-kind", "vol-kind", "")
		if err == nil || !strings.Contains(err.Error(), "old stdout diagnostic") {
			t.Fatalf("stdout fallback changed: %v", err)
		}
	})
}

func TestDestroyDoesNotInterpretErrorKind(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(strconv.Itoa(exit), func(t *testing.T) {
			p, _ := newTestBinaryPlugin(t, "")
			setTestBinaryResponse(t, `{"error":"public error","error_kind":"invalid_argument"}`, "old stderr diagnostic", exit)
			err := p.Destroy(context.Background(), "vol-kind", "")
			var invalid *plugin.InvalidArgumentError
			if err == nil || errors.As(err, &invalid) {
				t.Fatalf("Destroy must remain generic: %v", err)
			}
			want := "public error"
			if exit != 0 {
				want = "old stderr diagnostic"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("Destroy diagnostics changed: %v", err)
			}
		})
	}
	t.Run("unknown_field_on_success", func(t *testing.T) {
		p, _ := newTestBinaryPlugin(t, "")
		setTestBinaryResponse(t, `{"error":"","error_kind":"unknown"}`, "", 0)
		if err := p.Destroy(context.Background(), "vol-kind", ""); err != nil {
			t.Fatalf("Destroy started interpreting kind: %v", err)
		}
	})
}

// waitTestBinaryReady ensures typed stdout was written before interruption,
// so a generic result cannot pass merely because the child never produced it.
func waitTestBinaryReady(t *testing.T, path string) *os.Process {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(raw))
			if parseErr == nil {
				process, findErr := os.FindProcess(pid)
				if findErr != nil {
					t.Fatal(findErr)
				}
				return process
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("test plugin did not report ready")
	return nil
}

func TestCreateExecutionFaultsAreNotCallerErrors(t *testing.T) {
	t.Run("start_failure", func(t *testing.T) {
		p := New("missing", filepath.Join(t.TempDir(), "nonexistent-plugin"))
		_, err := p.Create(context.Background(), "vol-fault", "vol-fault", "")
		var invalid *plugin.InvalidArgumentError
		if err == nil || errors.As(err, &invalid) {
			t.Fatalf("startup failure classified as caller error: %v", err)
		}
	})
	for _, fault := range []string{"cancel", "timeout", "signal"} {
		t.Run(fault, func(t *testing.T) {
			if fault == "signal" && runtime.GOOS == "windows" {
				t.Skip("Unix signal exit semantics")
			}
			p, _ := newTestBinaryPlugin(t, "")
			setTestBinaryResponse(t, `{"error":"public error","error_kind":"invalid_argument"}`, "private diagnostic", 1)
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("CUBESANDBOX_TEST_BINARY_READY", ready)
			t.Setenv("CUBESANDBOX_TEST_BINARY_WAIT", "1")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "timeout" {
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
			}
			result := make(chan error, 1)
			go func() { _, err := p.Create(ctx, "vol-fault", "vol-fault", ""); result <- err }()
			process := waitTestBinaryReady(t, ready)
			t.Cleanup(func() { _ = process.Kill() })
			switch fault {
			case "cancel":
				cancel()
			case "signal":
				if err := process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-result:
				var invalid *plugin.InvalidArgumentError
				if err == nil || errors.As(err, &invalid) {
					t.Fatalf("%s classified as caller error: %v", fault, err)
				}
				if fault == "cancel" || fault == "timeout" {
					if !errors.Is(err, ctx.Err()) {
						t.Fatalf("%s lost context error identity: %v", fault, err)
					}
					if strings.Contains(err.Error(), "private diagnostic") || strings.Contains(err.Error(), "public error") {
						t.Fatalf("%s included plugin diagnostics: %v", fault, err)
					}
				}
			case <-time.After(10 * time.Second):
				t.Fatal("interrupted plugin did not return")
			}
		})
	}
}
