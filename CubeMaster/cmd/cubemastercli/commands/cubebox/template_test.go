// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
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

func newCommitContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	set := flag.NewFlagSet("commit", flag.ContinueOnError)
	set.String("address", "", "cubemaster address")
	set.String("port", "", "cubemaster port")
	set.Duration("timeout", 0, "request timeout")
	for _, cliFlag := range TemplateCommitCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}

	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = TemplateCommitCommand
	return ctx
}

func newDeleteContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	set := flag.NewFlagSet("delete", flag.ContinueOnError)
	set.String("address", "", "cubemaster address")
	set.String("port", "", "cubemaster port")
	set.Duration("timeout", 0, "request timeout")
	for _, cliFlag := range TemplateDeleteCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}

	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = TemplateDeleteCommand
	return ctx
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCommitCommandLetsCubeMasterResolveRequest(t *testing.T) {
	var requests []string
	var commitBody map[string]interface{}
	origHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.URL.Path)
		if req.URL.Path != "/cube/sandbox/commit" {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("unexpected endpoint")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		if err := json.NewDecoder(req.Body).Decode(&commitBody); err != nil {
			t.Fatalf("decode commit body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ret":{"ret_code":200,"ret_msg":"success"},"template_id":"tpl-new"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	defer func() {
		http.DefaultClient = origHTTPClient
	}()

	ctx := newCommitContext(t, []string{
		"--address", "127.0.0.1",
		"--port", "8089",
		"--sandbox-id", "sb-auto",
		"--detach",
	})
	action, ok := TemplateCommitCommand.Action.(func(*cli.Context) error)
	if !ok {
		t.Fatalf("unexpected commit action type %T", TemplateCommitCommand.Action)
	}
	if err := action(ctx); err != nil {
		t.Fatalf("commit action returned error: %v", err)
	}
	if got, want := strings.Join(requests, ","), "/cube/sandbox/commit"; got != want {
		t.Fatalf("request paths=%q, want %q", got, want)
	}
	if _, ok := commitBody["create_request"]; ok {
		t.Fatalf("create_request should be omitted: %v", commitBody["create_request"])
	}
}

func TestCommitCommandRequiresFileForNetworkOverrides(t *testing.T) {
	ctx := newCommitContext(t, []string{
		"--address", "127.0.0.1",
		"--port", "8089",
		"--sandbox-id", "sb-auto",
		"--allow-internet-access=false",
		"--detach",
	})
	action := TemplateCommitCommand.Action.(func(*cli.Context) error)
	err := action(ctx)
	if err == nil || !strings.Contains(err.Error(), "network override flags require --file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCommitCommandUsesFileAsCompleteRequestWithNetworkOverrides(t *testing.T) {
	path := t.TempDir() + "/request.json"
	if err := os.WriteFile(path, []byte(`{
		"instance_type":"cubebox",
		"network_type":"tap",
		"cube_network_config":{"allowInternetAccess":true,"allowOut":["10.0.0.0/8"]}
	}`), 0600); err != nil {
		t.Fatalf("write request file: %v", err)
	}
	var requests []string
	var commitBody struct {
		CreateRequest struct {
			InstanceType      string `json:"instance_type"`
			NetworkType       string `json:"network_type"`
			CubeNetworkConfig struct {
				AllowInternetAccess *bool    `json:"allowInternetAccess"`
				AllowOut            []string `json:"allowOut"`
				DenyOut             []string `json:"denyOut"`
			} `json:"cube_network_config"`
		} `json:"create_request"`
	}
	origHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.URL.Path)
		if req.URL.Path != "/cube/sandbox/commit" {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("unexpected endpoint")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		if err := json.NewDecoder(req.Body).Decode(&commitBody); err != nil {
			t.Fatalf("decode commit body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ret":{"ret_code":200,"ret_msg":"success"},"template_id":"tpl-new"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	defer func() {
		http.DefaultClient = origHTTPClient
	}()

	ctx := newCommitContext(t, []string{
		"--address", "127.0.0.1",
		"--port", "8089",
		"--sandbox-id", "sb-file",
		"--file", path,
		"--allow-internet-access=false",
		"--allow-out-cidr", "172.67.0.0/16",
		"--deny-out-cidr", "192.168.0.0/16",
		"--detach",
	})
	action, ok := TemplateCommitCommand.Action.(func(*cli.Context) error)
	if !ok {
		t.Fatalf("unexpected commit action type %T", TemplateCommitCommand.Action)
	}
	if err := action(ctx); err != nil {
		t.Fatalf("commit action returned error: %v", err)
	}
	if got, want := strings.Join(requests, ","), "/cube/sandbox/commit"; got != want {
		t.Fatalf("request paths=%q, want %q", got, want)
	}
	createRequest := commitBody.CreateRequest
	if got := createRequest.InstanceType; got != "cubebox" {
		t.Fatalf("instance_type=%v", got)
	}
	if got := createRequest.NetworkType; got != "tap" {
		t.Fatalf("network_type=%v", got)
	}
	if createRequest.CubeNetworkConfig.AllowInternetAccess == nil {
		t.Fatalf("cube_network_config=%+v", createRequest.CubeNetworkConfig)
	}
	if got := *createRequest.CubeNetworkConfig.AllowInternetAccess; got {
		t.Fatalf("allowInternetAccess=%v, want false", got)
	}
	if got, want := strings.Join(createRequest.CubeNetworkConfig.AllowOut, ","), "10.0.0.0/8,172.67.0.0/16"; got != want {
		t.Fatalf("allowOut=%q, want %q", got, want)
	}
	if got, want := strings.Join(createRequest.CubeNetworkConfig.DenyOut, ","), "192.168.0.0/16"; got != want {
		t.Fatalf("denyOut=%q, want %q", got, want)
	}
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

func TestMergeCreateFromImageCubeNetworkConfigFlagsEqualsSyntax(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--allow-internet-access=false",
		"--allow-out-cidr", "172.67.0.0/16",
		"--deny-out-cidr", "10.0.0.0/8",
	})

	got, err := mergeCreateFromImageCubeNetworkConfigFlags(ctx, nil)
	if err != nil {
		t.Fatalf("mergeCreateFromImageCubeNetworkConfigFlags error=%v", err)
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

func TestMergeCreateFromImageCubeNetworkConfigFlagsSupportsTrailingFalse(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--allow-internet-access", "false",
		"--allow-out-cidr", "172.67.0.0/16",
	})

	got, err := mergeCreateFromImageCubeNetworkConfigFlags(ctx, nil)
	if err != nil {
		t.Fatalf("mergeCreateFromImageCubeNetworkConfigFlags error=%v", err)
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

func TestApplyCreateFromImageIvshmemFlag(t *testing.T) {
	withoutFlag := &types.CreateTemplateFromImageReq{}
	applyCreateFromImageIvshmemFlag(newCreateFromImageContext(t, nil), withoutFlag)
	if withoutFlag.EnableIvshmem != nil {
		t.Fatalf("EnableIvshmem=%v, want nil when flag is not set", *withoutFlag.EnableIvshmem)
	}

	withFlag := &types.CreateTemplateFromImageReq{}
	applyCreateFromImageIvshmemFlag(newCreateFromImageContext(t, []string{"--enable-ivshmem"}), withFlag)
	if withFlag.EnableIvshmem == nil || !*withFlag.EnableIvshmem {
		t.Fatalf("EnableIvshmem=%v, want true", withFlag.EnableIvshmem)
	}
}

func TestMergeCreateFromImageCubeNetworkConfigFlagsRejectsUnexpectedArgs(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{
		"--allow-internet-access", "false",
		"unexpected",
	})

	_, err := mergeCreateFromImageCubeNetworkConfigFlags(ctx, nil)
	if err == nil {
		t.Fatal("expected error for unexpected trailing argument")
	}
}

func TestMergeCubeNetworkConfigValuesPreservesExistingCIDRs(t *testing.T) {
	existing := &types.CubeNetworkConfig{
		AllowOut: []string{"192.168.0.0/16"},
	}

	got := mergeCubeNetworkConfigValues(existing, true, false, []string{"172.67.0.0/16"}, nil)
	if got == nil || got.AllowInternetAccess == nil || *got.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%v, want false", got)
	}
	if len(got.AllowOut) != 2 || got.AllowOut[0] != "192.168.0.0/16" || got.AllowOut[1] != "172.67.0.0/16" {
		t.Fatalf("AllowOut=%v, want merged CIDRs", got.AllowOut)
	}
}

func TestMergeCubeNetworkConfigValuesPreservesRulesAndAllowPublicTraffic(t *testing.T) {
	host := "api.internal.example.com"
	scheme := "https"
	port := 8443
	audit := "full"
	format := "Bearer ${SECRET}"
	allowPublic := false
	existing := &types.CubeNetworkConfig{
		AllowPublicTraffic: &allowPublic,
		Rules: []*types.EgressRule{
			{
				Name: "api-8443-https",
				Match: &types.EgressRuleMatch{
					Host:   &host,
					Scheme: &scheme,
					Port:   &port,
				},
				Action: &types.EgressRuleAction{
					Allow: true,
					Audit: &audit,
					Inject: []*types.EgressRuleInject{
						{Header: "Authorization", Secret: "s3cret", Format: &format},
					},
				},
			},
		},
	}

	// Any --allow-* flag triggers the merge path, which clones `existing`.
	got := mergeCubeNetworkConfigValues(existing, false, false, []string{"10.0.0.0/8"}, nil)
	if got == nil {
		t.Fatal("got nil merged config")
	}
	if got.AllowPublicTraffic == nil || *got.AllowPublicTraffic != false {
		t.Fatalf("AllowPublicTraffic=%v, want pointer to false (template value preserved)", got.AllowPublicTraffic)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("Rules=%v, want the template rule preserved", got.Rules)
	}
	rule := got.Rules[0]
	if rule.Name != "api-8443-https" || rule.Match == nil || rule.Match.Port == nil || *rule.Match.Port != 8443 {
		t.Fatalf("rule=%+v, want port-pinned rule preserved", rule)
	}
	if rule.Action == nil || len(rule.Action.Inject) != 1 || rule.Action.Inject[0].Header != "Authorization" {
		t.Fatalf("action=%+v, want inject preserved", rule.Action)
	}

	// The clone must be deep: mutating the merged copy must not touch the template.
	*got.AllowPublicTraffic = true
	*rule.Match.Port = 443
	*rule.Match.Host = "mutated.example.com"
	*rule.Action.Inject[0].Format = "mutated"
	if *existing.AllowPublicTraffic != false {
		t.Fatal("mutation of merged AllowPublicTraffic leaked into template")
	}
	if *existing.Rules[0].Match.Port != 8443 {
		t.Fatal("mutation of merged Match.Port leaked into template")
	}
	if *existing.Rules[0].Match.Host != "api.internal.example.com" {
		t.Fatal("mutation of merged Match.Host leaked into template")
	}
	if *existing.Rules[0].Action.Inject[0].Format != "Bearer ${SECRET}" {
		t.Fatal("mutation of merged Inject.Format leaked into template")
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

func TestParseContainerOverridesEnableInjectEnvd(t *testing.T) {
	ctx := newCreateFromImageContext(t, []string{"--enable-inject-envd"})
	overrides, err := parseContainerOverrides(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil || overrides.Annotations == nil {
		t.Fatal("expected annotations to be set")
	}
	if got := overrides.Annotations[constants.CubeAnnotationsInjectEnvd]; got != constants.CubeAnnotationsInjectEnvdOptIn {
		t.Fatalf("expected inject envd annotation=true, got %q", got)
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
		Status:                  "READY",
		TemplateID:              "tpl-1",
		JobID:                   "job-1",
		ArtifactID:              "artifact-1",
		ExpectedNodeCount:       2,
		ReadyNodeCount:          2,
		FailedNodeCount:         0,
		TemplateStatus:          "READY",
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

func TestPrintTemplateSummaryIncludesOptionalMetadata(t *testing.T) {
	var logBuf bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
	})

	stdout := captureStdout(t, func() {
		printTemplateSummary(&templateResponse{
			TemplateID:   "tpl-1",
			DisplayName:  "python-template",
			InstanceType: "cubebox",
			Version:      "v2",
			Status:       "READY",
			CreatedAt:    "2026-06-17 12:00:00",
			ImageInfo:    "docker.io/library/python:3.12",
		})
	})

	logOutput := logBuf.String()
	for _, want := range []string{
		"template_id: tpl-1",
		"alias: python-template",
		"created_at: 2026-06-17 12:00:00",
		"image_info: docker.io/library/python:3.12",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output=%q, missing %q", logOutput, want)
		}
	}
	if !strings.Contains(stdout, "NODE_ID") {
		t.Fatalf("stdout=%q, missing replica table header", stdout)
	}
}

func TestResolveTemplateIDFromFlag(t *testing.T) {
	ctx := newRedoContext(t, []string{"--template-id", "tpl-1"})
	if got := resolveTemplateID(ctx); got != "tpl-1" {
		t.Fatalf("got %q, want tpl-1", got)
	}
}

func TestResolveTemplateIDFromPositional(t *testing.T) {
	ctx := newRedoContext(t, []string{"tpl-1"})
	if got := resolveTemplateID(ctx); got != "tpl-1" {
		t.Fatalf("got %q, want tpl-1", got)
	}
}

func TestResolveTemplateIDFlagOverridesPositional(t *testing.T) {
	ctx := newRedoContext(t, []string{"--template-id", "flag-id", "positional-id"})
	if got := resolveTemplateID(ctx); got != "flag-id" {
		t.Fatalf("got %q, want flag-id", got)
	}
}

func TestResolveTemplateIDEmpty(t *testing.T) {
	ctx := newRedoContext(t, nil)
	if got := resolveTemplateID(ctx); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// newCmdContext builds a *cli.Context from a command definition and args,
// used to verify resolveTemplateID works correctly with each command's
// specific flag set (info, delete, redo).
func newCmdContext(t *testing.T, cmd cli.Command, args []string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	for _, cliFlag := range cmd.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}
	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = cmd
	return ctx
}

func TestResolveTemplateIDFromAllTemplateCommands(t *testing.T) {
	tests := []struct {
		name string
		cmd  cli.Command
		args []string
		want string
	}{
		{
			name: "info via positional arg",
			cmd:  TemplateInfoCommand,
			args: []string{"tpl-info-1"},
			want: "tpl-info-1",
		},
		{
			name: "delete via positional arg",
			cmd:  TemplateDeleteCommand,
			args: []string{"tpl-delete-1"},
			want: "tpl-delete-1",
		},
		{
			name: "redo via positional arg",
			cmd:  TemplateRedoCommand,
			args: []string{"tpl-redo-1"},
			want: "tpl-redo-1",
		},
		{
			name: "set-alias via positional arg",
			cmd:  TemplateSetAliasCommand,
			args: []string{"tpl-set-1"},
			want: "tpl-set-1",
		},
		{
			name: "info flag overrides positional",
			cmd:  TemplateInfoCommand,
			args: []string{"--template-id", "flag-id", "positional-id"},
			want: "flag-id",
		},
		{
			name: "delete flag overrides positional",
			cmd:  TemplateDeleteCommand,
			args: []string{"--template-id", "flag-id", "positional-id"},
			want: "flag-id",
		},
		{
			name: "redo flag overrides positional",
			cmd:  TemplateRedoCommand,
			args: []string{"--template-id", "flag-id", "positional-id"},
			want: "flag-id",
		},
		{
			name: "set-alias flag overrides positional",
			cmd:  TemplateSetAliasCommand,
			args: []string{"--template-id", "flag-id", "positional-id"},
			want: "flag-id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newCmdContext(t, tt.cmd, tt.args)
			if got := resolveTemplateID(ctx); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTemplateIDs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "multiple positional ids",
			args: []string{"tpl-1", "tpl-2", "tpl-3"},
			want: []string{"tpl-1", "tpl-2", "tpl-3"},
		},
		{
			name: "flag preserves existing override behavior",
			args: []string{"--template-id", "tpl-flag", "tpl-positional"},
			want: []string{"tpl-flag"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newDeleteContext(t, tt.args)
			if got := resolveTemplateIDs(ctx); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTemplateDeleteCommandDeletesAllTemplateIDs(t *testing.T) {
	var deleted []string
	origHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body templateDeleteRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode delete request: %v", err)
		}
		deleted = append(deleted, body.TemplateID)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ret":{"ret_code":200,"ret_msg":"success"}}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	defer func() { http.DefaultClient = origHTTPClient }()

	ctx := newDeleteContext(t, []string{
		"--address", "127.0.0.1",
		"--port", "8089",
		"tpl-1", "tpl-2", "tpl-3",
	})
	action := TemplateDeleteCommand.Action.(func(*cli.Context) error)
	if err := action(ctx); err != nil {
		t.Fatalf("delete returned error: %v", err)
	}
	if want := []string{"tpl-1", "tpl-2", "tpl-3"}; !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted %v, want %v", deleted, want)
	}
}

func TestTemplateDeleteCommandContinuesAfterFailure(t *testing.T) {
	var deleted []string
	var logBuf bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
	})

	origHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body templateDeleteRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode delete request: %v", err)
		}
		deleted = append(deleted, body.TemplateID)
		response := `{"ret":{"ret_code":200,"ret_msg":"success"}}`
		if body.TemplateID == "tpl-fail" {
			response = `{"ret":{"ret_code":500,"ret_msg":"delete failed"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(response)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	defer func() { http.DefaultClient = origHTTPClient }()

	ctx := newDeleteContext(t, []string{
		"--address", "127.0.0.1",
		"--port", "8089",
		"tpl-first", "tpl-fail", "tpl-last",
	})
	action := TemplateDeleteCommand.Action.(func(*cli.Context) error)
	err := action(ctx)
	if err == nil || !strings.Contains(err.Error(), "tpl-fail: delete failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"tpl-first", "tpl-fail", "tpl-last"}; !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted %v, want %v", deleted, want)
	}
	if got := logBuf.String(); !strings.Contains(got, "template delete failed. delete failed. TemplateId: tpl-fail. RequestId:") {
		t.Fatalf("failure log %q does not identify failed template", got)
	}
}
