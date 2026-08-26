// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func newListContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	set := flag.NewFlagSet("list", flag.ContinueOnError)
	for _, cliFlag := range ListCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}

	ctx := cli.NewContext(nil, set, nil)
	ctx.Command = ListCommand
	return ctx
}

func TestBuildListRequestUsesBodyByDefault(t *testing.T) {
	port = "8089"
	ctx := newListContext(t, nil)
	req := &types.ListCubeSandboxReq{
		RequestID:    "req-1",
		StartIdx:     2,
		Size:         3,
		InstanceType: "cubebox",
	}

	url, body, err := buildListRequest(ctx, "127.0.0.1", req, nil)
	if err != nil {
		t.Fatalf("buildListRequest error=%v", err)
	}
	if want := "http://127.0.0.1:8089/cube/sandbox/list"; url != want {
		t.Fatalf("url=%q, want %q", url, want)
	}
	if body == nil {
		t.Fatal("expected request body")
	}

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll body error=%v", err)
	}
	if got := string(data); !strings.Contains(got, `"start_idx":2`) || !strings.Contains(got, `"size":3`) {
		t.Fatalf("body=%s, want encoded pagination fields", got)
	}
}

func TestBuildListRequestOldUsesQueryParameters(t *testing.T) {
	port = "8089"
	ctx := newListContext(t, []string{"--old"})
	req := &types.ListCubeSandboxReq{
		RequestID:    "req-2",
		StartIdx:     4,
		Size:         5,
		InstanceType: "cubebox",
	}

	url, body, err := buildListRequest(ctx, "127.0.0.1", req, []string{"user=alice", "team=dev"})
	if err != nil {
		t.Fatalf("buildListRequest error=%v", err)
	}
	if body != nil {
		t.Fatal("expected nil body for old request")
	}
	for _, want := range []string{
		"http://127.0.0.1:8089/cube/sandbox/list?requestID=req-2",
		"start_idx=4",
		"size=5",
		"filter.label_selector=user=alice,team=dev",
		"instance_type=cubebox",
	} {
		if !strings.Contains(url, want) {
			t.Fatalf("url=%q, missing %q", url, want)
		}
	}
}

func TestStatusWithPauseState(t *testing.T) {
	cases := []struct {
		status      int32
		pauseStatus string
		want        string
	}{
		{5, "", "paused"},
		{5, "READY", "paused"},
		{5, "DELETE_FAILED", "paused(delete_failed)"},
		{1, "", "running"},
	}
	for _, tc := range cases {
		if got := statusWithPauseState(tc.status, tc.pauseStatus); got != tc.want {
			t.Fatalf("statusWithPauseState(%d, %q)=%q, want %q", tc.status, tc.pauseStatus, got, tc.want)
		}
	}
}

func TestBuildListSummaryAll(t *testing.T) {
	req := &types.ListCubeSandboxReq{StartIdx: 1, Size: 2}
	rsp := &types.ListCubeSandboxRes{
		Size:  4,
		Total: 6,
		Data: []*types.SandboxBriefData{
			{SandboxID: "sb-1"},
			{SandboxID: "sb-2"},
		},
	}

	got := buildListSummary(req, rsp, true)
	if got.NodeScope != "all" {
		t.Fatalf("NodeScope=%q, want all", got.NodeScope)
	}
	if got.NodesScanned != 4 || got.NodesTotal != 6 || got.SandboxCount != 2 {
		t.Fatalf("summary=%+v", got)
	}
}

func TestBuildListSummaryEmptyPageUsesReadableScope(t *testing.T) {
	req := &types.ListCubeSandboxReq{StartIdx: 1, Size: 1}
	rsp := &types.ListCubeSandboxRes{
		EndIdx: -1,
		Total:  2,
	}

	got := buildListSummary(req, rsp, false)
	if got.NodeScope != "1-empty" {
		t.Fatalf("NodeScope=%q, want 1-empty", got.NodeScope)
	}
	if got.NodesScanned != 0 || got.NodesTotal != 2 || got.SandboxCount != 0 {
		t.Fatalf("summary=%+v", got)
	}
}

func TestParseListFiltersSkipsInvalidEntries(t *testing.T) {
	got, normalized := parseListFilters([]string{" user=alice ", "invalid", "", "team=dev"})
	if len(got) != 2 || got["user"] != "alice" || got["team"] != "dev" {
		t.Fatalf("parsed filters=%v", got)
	}
	if len(normalized) != 2 || normalized[0] != "user=alice" || normalized[1] != "team=dev" {
		t.Fatalf("normalized filters=%v", normalized)
	}
}

func TestPrintSandboxInfoBlockIncludesVolumeMounts(t *testing.T) {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 4, 8, 4, ' ', 0)
	printSandboxInfoBlock(w, &types.SandboxData{
		SandboxID: "sb-1",
		VolumeMounts: []*types.VolumeMountInfo{{
			Name:          "hostdir-0",
			ContainerPath: "/mnt/data",
			Readonly:      true,
		}},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("flush writer error=%v", err)
	}

	output := buf.String()
	for _, want := range []string{"VOLUME_MOUNTS", "hostdir-0", "/mnt/data", "true"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output=%q, missing %q", output, want)
		}
	}
	if strings.Contains(output, "HOST_PATH") || strings.Contains(output, "/tmp/data") {
		t.Fatalf("output=%q unexpectedly includes hostPath", output)
	}
}

func TestPrintSandboxInfoBlockIncludesMetadata(t *testing.T) {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 4, 8, 4, ' ', 0)
	printSandboxInfoBlock(w, &types.SandboxData{
		SandboxID:   "sb-1",
		Status:      1,
		HostID:      "host-1",
		HostIP:      "10.0.0.1",
		SandboxIP:   "172.16.0.2",
		TemplateID:  "tpl-1",
		NameSpace:   "ns-1",
		Annotations: map[string]string{"a": "b"},
		Labels:      map[string]string{"user": "alice"},
		Containers: []*types.ContainerInfo{
			{Name: "sandbox", ContainerID: "ctr-1", Image: "img", Status: 1, Type: "sandbox"},
		},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("flush writer error=%v", err)
	}

	output := buf.String()
	for _, want := range []string{"SANDBOX_ID", "host-1", "tpl-1", "LABELS", "CONTAINERS", "ctr-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output=%q, missing %q", output, want)
		}
	}
}

func TestDisplayValueNormalizesEmptyStrings(t *testing.T) {
	for _, input := range []string{"", "null"} {
		if got := displayValue(input); got != "-" {
			t.Fatalf("displayValue(%q)=%q, want -", input, got)
		}
	}
	if got := displayValue("value"); got != "value" {
		t.Fatalf("displayValue(value)=%q, want value", got)
	}
}
