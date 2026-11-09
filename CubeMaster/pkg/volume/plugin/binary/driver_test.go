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
	"strings"
	"testing"
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
