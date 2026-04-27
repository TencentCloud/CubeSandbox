// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"flag"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func newCreateFromImageContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	set := flag.NewFlagSet("create-from-image", flag.ContinueOnError)
	for _, cliFlag := range TemplateCreateFromImageCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}

	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = TemplateCreateFromImageCommand
	return ctx
}

func newCreateContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	set := flag.NewFlagSet("create", flag.ContinueOnError)
	for _, cliFlag := range TemplateCreateCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}

	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = TemplateCreateCommand
	return ctx
}

func newRedoContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	set := flag.NewFlagSet("redo", flag.ContinueOnError)
	for _, cliFlag := range TemplateRedoCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}

	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = TemplateRedoCommand
	return ctx
}

func TestCreateCommandParsesNodeScope(t *testing.T) {
	ctx := newCreateContext(t, []string{
		"--node", "node-a",
		"--node", "10.0.0.2",
	})
	if got := ctx.StringSlice("node"); len(got) != 2 || got[0] != "node-a" || got[1] != "10.0.0.2" {
		t.Fatalf("node flags=%v", got)
	}
}

func TestMergeCreateFromImageCubeVSContextFlagsEqualsSyntax(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--allow-internet-access=false",
		"--allow-out-cidr", "172.67.0.0/16",
		"--deny-out-cidr", "10.0.0.0/8",
	})

	got, err := mergeCreateFromImageCubeVSContextFlags(ctx, nil)
	if err != nil {
		t.Fatalf("mergeCreateFromImageCubeVSContextFlags error=%v", err)
	}
	if got == nil || got.AllowInternetAccess == nil || *got.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%v, want false", got)
	}
	if len(got.AllowOut) != 1 || got.AllowOut[0] != "172.67.0.0/16" {
		t.Fatalf("AllowOut=%v, want [172.67.0.0/16]", got.AllowOut)
	}
	if len(got.DenyOut) != 1 || got.DenyOut[0] != "10.0.0.0/8" {
		t.Fatalf("DenyOut=%v, want [10.0.0.0/8]", got.DenyOut)
	}
}

func TestMergeCreateFromImageCubeVSContextFlagsSupportsTrailingFalse(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--allow-internet-access", "false",
		"--allow-out-cidr", "172.67.0.0/16",
	})

	got, err := mergeCreateFromImageCubeVSContextFlags(ctx, nil)
	if err != nil {
		t.Fatalf("mergeCreateFromImageCubeVSContextFlags error=%v", err)
	}
	if got == nil || got.AllowInternetAccess == nil || *got.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%v, want false", got)
	}
	if len(got.AllowOut) != 1 || got.AllowOut[0] != "172.67.0.0/16" {
		t.Fatalf("AllowOut=%v, want [172.67.0.0/16]", got.AllowOut)
	}
}

func TestCreateFromImageCommandParsesNodeScope(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--node", "node-a",
		"--node", "10.0.0.2",
	})
	if got := ctx.StringSlice("node"); len(got) != 2 || got[0] != "node-a" || got[1] != "10.0.0.2" {
		t.Fatalf("node flags=%v", got)
	}
}

func TestMergeCreateFromImageCubeVSContextFlagsRejectsUnexpectedArgs(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--allow-internet-access", "false",
		"unexpected",
	})

	_, err := mergeCreateFromImageCubeVSContextFlags(ctx, nil)
	if err == nil {
		t.Fatal("expected error for unexpected trailing argument")
	}
}

func TestMergeCubeVSContextValuesPreservesExistingCIDRs(t *testing.T) {
	existing := &types.CubeVSContext{
		AllowOut: []string{"192.168.0.0/16"},
	}

	got := mergeCubeVSContextValues(existing, true, false, []string{"172.67.0.0/16"}, nil)
	if got == nil || got.AllowInternetAccess == nil || *got.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%v, want false", got)
	}
	if len(got.AllowOut) != 2 || got.AllowOut[0] != "192.168.0.0/16" || got.AllowOut[1] != "172.67.0.0/16" {
		t.Fatalf("AllowOut=%v, want merged CIDRs", got.AllowOut)
	}
}

func TestRedoCommandParsesNodeScope(t *testing.T) {
	ctx := newRedoContext(t, []string{
		"--template-id", "tpl-1",
		"--node", "node-a",
		"--node", "10.0.0.2",
		"--failed-only",
	})
	if got := ctx.String("template-id"); got != "tpl-1" {
		t.Fatalf("template-id=%q", got)
	}
	if got := ctx.StringSlice("node"); len(got) != 2 || got[0] != "node-a" || got[1] != "10.0.0.2" {
		t.Fatalf("node flags=%v", got)
	}
	if !ctx.Bool("failed-only") {
		t.Fatal("expected failed-only flag to be set")
	}
}

func TestParseContainerOverridesDefaultCpuMemory(t *testing.T) {
	// When neither --cpu nor --memory is set, resources should not be set in overrides.
	ctx := newCreateFromImageContext(t, []string{"--env", "KEY=VALUE"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil {
		t.Fatal("expected overrides to be non-nil due to --env flag")
	}
	if overrides.Resources != nil {
		t.Fatalf("expected Resources to be nil when cpu/memory not explicitly set, got %+v", overrides.Resources)
	}
}

func TestParseContainerOverridesCustomCpu(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--cpu", "4000"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil || overrides.Resources == nil {
		t.Fatal("expected Resources to be set when --cpu is specified")
	}
	if overrides.Resources.Cpu != "4000m" {
		t.Fatalf("expected Cpu=4000m, got %q", overrides.Resources.Cpu)
	}
	if overrides.Resources.Mem != "2000Mi" {
		t.Fatalf("expected Mem=2000Mi (default), got %q", overrides.Resources.Mem)
	}
}

func TestParseContainerOverridesCustomMemory(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--memory", "4096"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil || overrides.Resources == nil {
		t.Fatal("expected Resources to be set when --memory is specified")
	}
	if overrides.Resources.Mem != "4096Mi" {
		t.Fatalf("expected Mem=4096Mi, got %q", overrides.Resources.Mem)
	}
	if overrides.Resources.Cpu != "2000m" {
		t.Fatalf("expected Cpu=2000m (default), got %q", overrides.Resources.Cpu)
	}
}

func TestParseContainerOverridesCustomCpuAndMemory(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--cpu", "8000", "--memory", "8192"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil || overrides.Resources == nil {
		t.Fatal("expected Resources to be set")
	}
	if overrides.Resources.Cpu != "8000m" {
		t.Fatalf("expected Cpu=8000m, got %q", overrides.Resources.Cpu)
	}
	if overrides.Resources.Mem != "8192Mi" {
		t.Fatalf("expected Mem=8192Mi, got %q", overrides.Resources.Mem)
	}
}

func TestParseContainerOverridesDNS(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--dns", "8.8.8.8", "--dns", "1.1.1.1"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil || overrides.DnsConfig == nil {
		t.Fatal("expected DnsConfig to be set")
	}
	want := []string{"8.8.8.8", "1.1.1.1"}
	if len(overrides.DnsConfig.Servers) != len(want) {
		t.Fatalf("expected %d DNS servers, got %v", len(want), overrides.DnsConfig.Servers)
	}
	for i := range want {
		if overrides.DnsConfig.Servers[i] != want[i] {
			t.Fatalf("expected DNS server %d to be %q, got %q", i, want[i], overrides.DnsConfig.Servers[i])
		}
	}
}

func TestParseContainerOverridesRejectsInvalidDNS(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--dns", "not-an-ip"})
	overrides, err := parseContainerOverrides(ctx)
	if err == nil {
		t.Fatal("expected error for invalid DNS server")
	}
	if overrides != nil {
		t.Fatalf("expected overrides to be nil on invalid DNS, got %+v", overrides)
	}
}

func TestParseContainerOverridesNoDNS(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--env", "KEY=VALUE"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil {
		t.Fatal("expected overrides to be non-nil due to --env flag")
	}
	if overrides.DnsConfig != nil {
		t.Fatalf("expected DnsConfig to be nil when --dns is not set, got %+v", overrides.DnsConfig)
	}
}
