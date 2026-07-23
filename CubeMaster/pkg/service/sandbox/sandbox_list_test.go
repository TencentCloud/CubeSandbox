// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestParseCPUCount(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int32
	}{
		{name: "empty", input: "", want: 0},
		{name: "millicores", input: "2000m", want: 2},
		{name: "sub core", input: "500m", want: 0},
		{name: "whole cores", input: "2", want: 2},
		{name: "invalid", input: "bad", want: 0},
		{name: "leading spaces", input: " 4000m", want: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCPUCount(tt.input); got != tt.want {
				t.Fatalf("parseCPUCount(%q)=%d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseMemoryMB(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int32
	}{
		{name: "empty", input: "", want: 0},
		{name: "kibibytes", input: "1Ki", want: 1},
		{name: "mebibytes", input: "2048Mi", want: 2148},
		{name: "gibibytes", input: "2Gi", want: 2148},
		{name: "tebibytes", input: "1Ti", want: 1099512},
		{name: "gigabytes", input: "2G", want: 2000},
		{name: "megabytes", input: "512M", want: 512},
		{name: "milliunits", input: "256m", want: 1},
		{name: "plain bytes", input: "1024", want: 1},
		{name: "overflow", input: "2147483648M", want: 2147483647},
		{name: "invalid", input: "bad", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMemoryMB(tt.input); got != tt.want {
				t.Fatalf("parseMemoryMB(%q)=%d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestListSandboxEmptyResultClassification(t *testing.T) {
	origRange := rangeDBHostFn
	origHealthy := healthyNodesByProductFn
	defer func() {
		rangeDBHostFn = origRange
		healthyNodesByProductFn = origHealthy
	}()

	stub := func(nodes node.NodeList) {
		rangeDBHostFn = func(index, size int, product string) ([]*node.Node, int) {
			return nodes.IndexByPage(index, size)
		}
		healthyNodesByProductFn = func(n int, product string) node.NodeList {
			return nodes
		}
	}

	t.Run("no queryable nodes returns retryable CubeletUnHealthy", func(t *testing.T) {
		stub(node.NodeList{})

		rsp := ListSandbox(context.Background(), &types.ListCubeSandboxReq{StartIdx: 1, Size: 10})

		assert.Equal(t, int(errorcode.ErrorCode_CubeletUnHealthy), rsp.Ret.RetCode)
		assert.Equal(t, "CubeletUnHealthy", rsp.Ret.RetMsg)
		assert.Empty(t, rsp.Data)
	})

	t.Run("paginating past the last node keeps Success with an empty page", func(t *testing.T) {
		stub(node.NodeList{{InsID: "host-1"}, {InsID: "host-2"}})

		rsp := ListSandbox(context.Background(), &types.ListCubeSandboxReq{StartIdx: 5, Size: 10})

		assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		assert.Empty(t, rsp.Data)
	})
}
