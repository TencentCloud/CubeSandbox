// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"flag"
	"strings"
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

func TestTemplateImageJobWatchPhaseLabel(t *testing.T) {
	tests := []struct {
		name string
		job  *types.TemplateImageJobInfo
		want string
	}{
		{name: "pulling", job: &types.TemplateImageJobInfo{Phase: "PULLING"}, want: "[1/7] PULLING"},
		{name: "unpacking", job: &types.TemplateImageJobInfo{Phase: "UNPACKING"}, want: "[2/7] UNPACKING"},
		{name: "building ext4", job: &types.TemplateImageJobInfo{Phase: "BUILDING_EXT4"}, want: "[3/7] BUILDING_EXT4"},
		{name: "generating json", job: &types.TemplateImageJobInfo{Phase: "GENERATING_JSON"}, want: "[4/7] GENERATING_JSON"},
		{name: "distributing", job: &types.TemplateImageJobInfo{Phase: "DISTRIBUTING"}, want: "[5/7] DISTRIBUTING"},
		{name: "creating template", job: &types.TemplateImageJobInfo{Phase: "CREATING_TEMPLATE"}, want: "[6/7] CREATING_TEMPLATE"},
		{name: "ready", job: &types.TemplateImageJobInfo{Status: "READY", Phase: "READY"}, want: "[7/7] READY"},
		{name: "failed with ready phase", job: &types.TemplateImageJobInfo{Status: "FAILED", Phase: "READY"}, want: "[?/7] READY"},
		{name: "unknown", job: &types.TemplateImageJobInfo{Phase: "SOMETHING_NEW"}, want: "[?/7] SOMETHING_NEW"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTemplateImageJobWatchPhase(tt.job)
			if got != tt.want {
				t.Fatalf("phase label=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTemplateImageJobWatchLineIncludesKeyFields(t *testing.T) {
	job := &types.TemplateImageJobInfo{
		JobID:             "job-1",
		TemplateID:        "tpl-1",
		ArtifactID:        "artifact-1",
		Phase:             "DISTRIBUTING",
		Progress:          73,
		ExpectedNodeCount: 5,
		ReadyNodeCount:    3,
		FailedNodeCount:   1,
	}

	got := formatTemplateImageJobWatchLine(job)
	for _, want := range []string{
		"[5/7] DISTRIBUTING",
		"progress=73%",
		"distribution=3/5 ready, 1 failed",
		"template_id=tpl-1",
		"job_id=job-1",
		"artifact_id=artifact-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("watch line=%q, want substring %q", got, want)
		}
	}
}

func TestFormatTemplateImageJobWatchLineIncludesError(t *testing.T) {
	job := &types.TemplateImageJobInfo{
		Status:       "FAILED",
		Phase:        "BUILDING_EXT4",
		Progress:     55,
		ErrorMessage: "build ext4 failed",
	}

	got := formatTemplateImageJobWatchLine(job)
	for _, want := range []string{"[3/7] BUILDING_EXT4", "progress=55%", "error=build ext4 failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("watch line=%q, want substring %q", got, want)
		}
	}
}

func TestFormatTemplateImageJobCompletionSummarySuccess(t *testing.T) {
	job := &types.TemplateImageJobInfo{
		Status:             "READY",
		TemplateID:         "tpl-1",
		JobID:              "job-1",
		ArtifactID:         "artifact-1",
		ExpectedNodeCount:  2,
		ReadyNodeCount:     2,
		FailedNodeCount:    0,
		TemplateStatus:     "READY",
		TemplateSpecFingerprint: "sha256:abc",
	}

	got := formatTemplateImageJobCompletionSummary(job)
	for _, want := range []string{"template image job succeeded", "template_id=tpl-1", "job_id=job-1", "artifact_id=artifact-1", "distribution=2/2 ready, 0 failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary=%q, want substring %q", got, want)
		}
	}
}

func TestFormatTemplateImageJobCompletionSummaryFailure(t *testing.T) {
	job := &types.TemplateImageJobInfo{
		Status:       "FAILED",
		TemplateID:   "tpl-1",
		JobID:        "job-1",
		ErrorMessage: "pull failed",
	}

	got := formatTemplateImageJobCompletionSummary(job)
	for _, want := range []string{"template image job failed", "template_id=tpl-1", "job_id=job-1", "error=pull failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary=%q, want substring %q", got, want)
		}
	}
}

func TestFormatTemplateImageJobWatchHelpersHandleNil(t *testing.T) {
	if got := formatTemplateImageJobWatchPhase(nil); got != "[?/7] UNKNOWN" {
		t.Fatalf("nil phase label=%q, want [?/7] UNKNOWN", got)
	}
	if got := formatTemplateImageJobWatchLine(nil); got == "" {
		t.Fatal("expected non-empty watch line for nil job")
	}
	if got := formatTemplateImageJobCompletionSummary(nil); got == "" {
		t.Fatal("expected non-empty completion summary for nil job")
	}
}
