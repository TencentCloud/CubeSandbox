// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func withNodeAffinitySelectorAllowedKeys(t *testing.T, keys ...string) {
	t.Helper()
	scheduler := config.GetConfig().Scheduler
	original := append([]string(nil), scheduler.NodeAffinitySelectorAllowedKeys...)
	scheduler.NodeAffinitySelectorAllowedKeys = append([]string(nil), keys...)
	t.Cleanup(func() {
		scheduler.NodeAffinitySelectorAllowedKeys = original
	})
}

func Test_isLargeMemSize(t *testing.T) {
	type args struct {
		ctx          context.Context
		req          *types.CreateCubeSandboxReq
		largeMemSize string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "LargeMemSizeEmpty",
			args: args{
				ctx:          context.Background(),
				req:          &types.CreateCubeSandboxReq{},
				largeMemSize: "",
			},
			want: false,
		},
		{
			name: "InvalidContainerMem",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{
							Resources: &types.Resource{Mem: "invalid"},
						},
					},
				},
				largeMemSize: "1Gi",
			},
			want: false,
		},
		{
			name: "ExactMatchMem",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Mem: "1Gi"}},
						{Resources: &types.Resource{Mem: "1Gi"}},
					},
				},
				largeMemSize: "2Gi",
			},
			want: true,
		},
		{
			name: "BelowThreshold",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Mem: "500Mi"}},
					},
				},
				largeMemSize: "1Gi",
			},
			want: false,
		},
		{
			name: "AboveThreshold",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Mem: "2Gi"}},
						{Resources: &types.Resource{Mem: "500Mi"}},
					},
				},
				largeMemSize: "2Gi",
			},
			want: true,
		},
		{
			name: "NoContainers",
			args: args{
				ctx:          context.Background(),
				req:          &types.CreateCubeSandboxReq{},
				largeMemSize: "1Gi",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLargeMemSize(tt.args.ctx, tt.args.req, tt.args.largeMemSize)
			assert.Equal(t, tt.want, got, "isLargeMemSize() = %v, want %v", got, tt.want)
		})
	}
}

func Test_isLargeCpucores(t *testing.T) {
	type args struct {
		ctx           context.Context
		req           *types.CreateCubeSandboxReq
		largeCpucores string
	}

	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "empty largeCpucores",
			args: args{
				ctx:           context.Background(),
				req:           &types.CreateCubeSandboxReq{Containers: []*types.Container{}},
				largeCpucores: "",
			},
			want: false,
		},
		{
			name: "total cpu equals threshold",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Cpu: "1000m"}},
						{Resources: &types.Resource{Cpu: "1000m"}},
					},
				},
				largeCpucores: "2",
			},
			want: true,
		},
		{
			name: "total cpu exceeds threshold",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Cpu: "1500m"}},
						{Resources: &types.Resource{Cpu: "500m"}},
					},
				},
				largeCpucores: "2",
			},
			want: true,
		},
		{
			name: "invalid cpu format in container",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Cpu: "invalid"}},
					},
				},
				largeCpucores: "1",
			},
			want: false,
		},
		{
			name: "zero value boundary check",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Cpu: "0"}},
					},
				},
				largeCpucores: "0",
			},
			want: true,
		},
		{
			name: "mixed valid and invalid cpu formats",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Cpu: "1"}},
						{Resources: &types.Resource{Cpu: "invalid"}},
					},
				},
				largeCpucores: "1",
			},
			want: false,
		},
		{
			name: "large cpu threshold with decimal",
			args: args{
				ctx: context.Background(),
				req: &types.CreateCubeSandboxReq{
					Containers: []*types.Container{
						{Resources: &types.Resource{Cpu: "1500m"}},
					},
				},
				largeCpucores: "1.5",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLargeCpucores(context.TODO(), tt.args.req, tt.args.largeCpucores)
			assert.Equal(t, tt.want, got, "isLargeCpucores() = %v, want %v", got, tt.want)
		})
	}
}

func Test_isContainerReqWhiteTag(t *testing.T) {
	tests := []struct {
		name string
		pre  func()
		tag  string
		want bool
	}{
		{
			name: "ReqTemplateConf is nil",
			pre: func() {
				config.GetConfig().ReqTemplateConf = nil
			},
			tag:  "test",
			want: false,
		},
		{
			name: "WhitelistReqTag is nil",
			pre: func() {
				config.GetConfig().ReqTemplateConf = &config.ReqTemplateConf{
					WhitelistReqTag: nil,
				}
			},
			tag:  "test",
			want: false,
		},
		{
			name: "Tag exists in WhitelistReqTag",
			pre: func() {
				config.GetConfig().ReqTemplateConf = &config.ReqTemplateConf{
					WhitelistReqTag: map[string]interface{}{"test": struct{}{}},
				}
			},
			tag:  "test",
			want: true,
		},
		{
			name: "Tag does not exist in WhitelistReqTag",
			pre: func() {
				config.GetConfig().ReqTemplateConf = &config.ReqTemplateConf{
					WhitelistReqTag: map[string]interface{}{"other": struct{}{}},
				}
			},
			tag:  "test",
			want: false,
		},
		{
			name: "Empty tag exists",
			pre: func() {
				config.GetConfig().ReqTemplateConf = &config.ReqTemplateConf{
					WhitelistReqTag: map[string]interface{}{"": struct{}{}},
				}
			},
			tag:  "",
			want: true,
		},
		{
			name: "Empty tag not exists",
			pre: func() {
				config.GetConfig().ReqTemplateConf = &config.ReqTemplateConf{
					WhitelistReqTag: map[string]interface{}{"other": struct{}{}},
				}
			},
			tag:  "",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			original := config.GetConfig().ReqTemplateConf
			defer func() {
				config.GetConfig().ReqTemplateConf = original
			}()

			if tt.pre != nil {
				tt.pre()
			}

			got := isContainerReqWhiteTag(tt.tag)

			assert.Equal(t, tt.want, got, "isContainerReqWhiteTag(%q) mismatch", tt.tag)
		})
	}
}

func TestGetTemplateVolumes(t *testing.T) {
	tests := []struct {
		name            string
		sourceVolume    interface{}
		templateVolumes []*types.Volume
		expectedResult  *types.Volume
	}{
		{
			name:         "EmptyDir类型存在匹配",
			sourceVolume: &types.EmptyDirVolumeSource{Medium: 1},
			templateVolumes: []*types.Volume{
				{
					Name: "test-volume",
					VolumeSource: &types.VolumeSource{
						EmptyDir: &types.EmptyDirVolumeSource{Medium: 2},
					},
				},
			},
			expectedResult: &types.Volume{
				Name: "test-volume",
				VolumeSource: &types.VolumeSource{
					EmptyDir: &types.EmptyDirVolumeSource{Medium: 2},
				},
			},
		},
		{
			name:         "HostDirVolumeSources类型存在匹配",
			sourceVolume: &types.HostDirVolumeSources{},
			templateVolumes: []*types.Volume{
				{
					Name: "cos-volume",
					VolumeSource: &types.VolumeSource{
						HostDirVolumeSources: &types.HostDirVolumeSources{},
					},
				},
			},
			expectedResult: &types.Volume{
				Name: "cos-volume",
				VolumeSource: &types.VolumeSource{
					HostDirVolumeSources: &types.HostDirVolumeSources{},
				},
			},
		},
		{
			name:            "templateVolumes为空",
			sourceVolume:    &types.EmptyDirVolumeSource{Medium: 1},
			templateVolumes: []*types.Volume{},
			expectedResult:  nil,
		},
		{
			name:         "templateVolume为nil",
			sourceVolume: &types.EmptyDirVolumeSource{Medium: 1},
			templateVolumes: []*types.Volume{
				nil,
			},
			expectedResult: nil,
		},
		{
			name:         "templateVolume.VolumeSource为nil",
			sourceVolume: &types.EmptyDirVolumeSource{Medium: 1},
			templateVolumes: []*types.Volume{
				{
					Name:         "test-volume",
					VolumeSource: nil,
				},
			},
			expectedResult: nil,
		},
		{
			name:         "没有匹配的类型",
			sourceVolume: &types.EmptyDirVolumeSource{Medium: 1},
			templateVolumes: []*types.Volume{
				{
					Name: "test-volume",
					VolumeSource: &types.VolumeSource{
						SandboxPath: &types.SandboxPathVolumeSource{
							Path: "/data",
							Type: "Directory",
						},
					},
				},
			},
			expectedResult: nil,
		},
		{
			name:         "sourceVolume为nil",
			sourceVolume: nil,
			templateVolumes: []*types.Volume{
				{
					Name: "test-volume",
					VolumeSource: &types.VolumeSource{
						EmptyDir: &types.EmptyDirVolumeSource{Medium: 2},
					},
				},
			},
			expectedResult: nil,
		},
		{
			name: "遍历多个元素找到匹配",
			sourceVolume: &types.SandboxPathVolumeSource{
				Path: "/data",
				Type: "Directory",
			},
			templateVolumes: []*types.Volume{
				{
					Name: "volume1",
					VolumeSource: &types.VolumeSource{
						EmptyDir: &types.EmptyDirVolumeSource{Medium: 1},
					},
				},
				{
					Name: "volume2",
					VolumeSource: &types.VolumeSource{
						SandboxPath: &types.SandboxPathVolumeSource{
							Path: "/data",
							Type: "Directory",
						},
					},
				},
				{
					Name: "volume3",
					VolumeSource: &types.VolumeSource{
						HostDirVolumeSources: &types.HostDirVolumeSources{},
					},
				},
			},
			expectedResult: &types.Volume{
				Name: "volume2",
				VolumeSource: &types.VolumeSource{
					SandboxPath: &types.SandboxPathVolumeSource{
						Path: "/data",
						Type: "Directory",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getTemplateVolumes(tt.sourceVolume, tt.templateVolumes)

			if tt.expectedResult == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, tt.expectedResult.Name, result.Name)

				switch tt.sourceVolume.(type) {
				case *types.EmptyDirVolumeSource:
					assert.NotNil(t, result.VolumeSource.EmptyDir)
				case *types.HostDirVolumeSources:
					assert.NotNil(t, result.VolumeSource.HostDirVolumeSources)
				case *types.SandboxPathVolumeSource:
					assert.NotNil(t, result.VolumeSource.SandboxPath)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseNodeAffinitySelector tests
// ---------------------------------------------------------------------------

func Test_parseNodeAffinitySelector(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		allowedKeys []string
		wantErr     bool
		errMsg      string
		check       func(t *testing.T, result []affinity.NodeSelectorRequirement)
	}{
		// ---- valid cases ----
		{
			name: "empty array",
			json: `[]`,
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Empty(t, result)
			},
		},
		{
			name:        "In with single value",
			json:        `[{"key":"custom-label","operator":"In","values":["val1"]}]`,
			allowedKeys: []string{"custom-label"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "custom-label", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpIn, result[0].Operator)
				assert.Equal(t, map[string]any{"val1": struct{}{}}, result[0].Values)
			},
		},
		{
			name:        "In with multiple values",
			json:        `[{"key":"zone","operator":"In","values":["zone-a","zone-b","zone-c"]}]`,
			allowedKeys: []string{"zone"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "zone", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpIn, result[0].Operator)
				assert.Len(t, result[0].Values, 3)
				assert.Contains(t, result[0].Values, "zone-a")
				assert.Contains(t, result[0].Values, "zone-b")
				assert.Contains(t, result[0].Values, "zone-c")
			},
		},
		{
			name:        "In deduplicates values",
			json:        `[{"key":"label","operator":"In","values":["v","v","v"]}]`,
			allowedKeys: []string{"label"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Len(t, result[0].Values, 1)
				assert.Contains(t, result[0].Values, "v")
			},
		},
		{
			name:        "NotIn with values",
			json:        `[{"key":"instance-type","operator":"NotIn","values":["gpu","fpga"]}]`,
			allowedKeys: []string{"instance-type"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "instance-type", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpNotIn, result[0].Operator)
				assert.Len(t, result[0].Values, 2)
			},
		},
		{
			name:        "Exists operator",
			json:        `[{"key":"gpu","operator":"Exists"}]`,
			allowedKeys: []string{"gpu"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "gpu", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpExists, result[0].Operator)
				assert.Empty(t, result[0].Values)
			},
		},
		{
			name:        "DoesNotExist operator",
			json:        `[{"key":"tainted","operator":"DoesNotExist"}]`,
			allowedKeys: []string{"tainted"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "tainted", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpDoesNotExist, result[0].Operator)
				assert.Empty(t, result[0].Values)
			},
		},
		{
			name: "Gt on memory-size",
			json: `[{"key":"kubernetes.io/memory-size","operator":"Gt","values":["4096Mi"]}]`,
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "kubernetes.io/memory-size", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpGt, result[0].Operator)
				assert.Len(t, result[0].Values, 1)
				assert.Contains(t, result[0].Values, "4096Mi")
			},
		},
		{
			name: "Lt on cpu-cores",
			json: `[{"key":"kubernetes.io/cpu-cores","operator":"Lt","values":["8000m"]}]`,
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "kubernetes.io/cpu-cores", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpLt, result[0].Operator)
				assert.Len(t, result[0].Values, 1)
				assert.Contains(t, result[0].Values, "8000m")
			},
		},
		{
			name: "Lt on memory-size",
			json: `[{"key":"kubernetes.io/memory-size","operator":"Lt","values":["8192Mi"]}]`,
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 1)
				assert.Equal(t, "kubernetes.io/memory-size", result[0].Key)
				assert.Equal(t, affinity.NodeSelectorOpLt, result[0].Operator)
			},
		},
		{
			name: "multiple mixed requirements",
			json: `[
				{"key":"gpu","operator":"Exists"},
				{"key":"kubernetes.io/memory-size","operator":"Gt","values":["2048Mi"]},
				{"key":"custom-label","operator":"In","values":["a","b"]}
			]`,
			allowedKeys: []string{"gpu", "custom-label"},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				assert.Len(t, result, 3)
				assert.Equal(t, affinity.NodeSelectorOpExists, result[0].Operator)
				assert.Equal(t, affinity.NodeSelectorOpGt, result[1].Operator)
				assert.Equal(t, affinity.NodeSelectorOpIn, result[2].Operator)
			},
		},

		// ---- invalid JSON ----
		{
			name:    "malformed JSON",
			json:    `{not json`,
			wantErr: true,
			errMsg:  "invalid com.nodeaffinity.selector JSON",
		},
		{
			name:    "JSON object instead of array",
			json:    `{"key":"x","operator":"In"}`,
			wantErr: true,
			errMsg:  "invalid com.nodeaffinity.selector JSON",
		},

		// ---- empty key ----
		{
			name:    "empty key",
			json:    `[{"key":"","operator":"In","values":["v"]}]`,
			wantErr: true,
			errMsg:  "node selector key must not be empty",
		},

		// ---- In/NotIn with empty values ----
		{
			name:        "In with empty values array",
			json:        `[{"key":"label","operator":"In","values":[]}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "requires non-empty values",
		},
		{
			name:        "In with omitted values",
			json:        `[{"key":"label","operator":"In"}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "requires non-empty values",
		},
		{
			name:        "NotIn with empty values array",
			json:        `[{"key":"label","operator":"NotIn","values":[]}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "requires non-empty values",
		},
		{
			name:        "NotIn with omitted values",
			json:        `[{"key":"label","operator":"NotIn"}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "requires non-empty values",
		},

		// ---- Exists/DoesNotExist with values ----
		{
			name:        "Exists with values",
			json:        `[{"key":"label","operator":"Exists","values":["x"]}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "requires empty values",
		},
		{
			name:        "DoesNotExist with values",
			json:        `[{"key":"label","operator":"DoesNotExist","values":["x"]}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "requires empty values",
		},

		// ---- Gt/Lt with wrong value count ----
		{
			name:    "Gt with zero values",
			json:    `[{"key":"kubernetes.io/memory-size","operator":"Gt"}]`,
			wantErr: true,
			errMsg:  "requires exactly one value",
		},
		{
			name:    "Gt with two values",
			json:    `[{"key":"kubernetes.io/memory-size","operator":"Gt","values":["1Gi","2Gi"]}]`,
			wantErr: true,
			errMsg:  "requires exactly one value",
		},
		{
			name:    "Lt with zero values",
			json:    `[{"key":"kubernetes.io/cpu-cores","operator":"Lt"}]`,
			wantErr: true,
			errMsg:  "requires exactly one value",
		},
		{
			name:    "Lt with two values",
			json:    `[{"key":"kubernetes.io/cpu-cores","operator":"Lt","values":["1","2"]}]`,
			wantErr: true,
			errMsg:  "requires exactly one value",
		},

		// ---- Gt/Lt on unsupported keys ----
		{
			name:    "Gt on instance-type key",
			json:    `[{"key":"kubernetes.io/instance-type","operator":"Gt","values":["1"]}]`,
			wantErr: true,
			errMsg:  "is only supported for keys",
		},
		{
			name:    "Lt on zone key",
			json:    `[{"key":"topology.kubernetes.io/zone","operator":"Lt","values":["1"]}]`,
			wantErr: true,
			errMsg:  "is only supported for keys",
		},
		{
			name:        "Gt on custom key",
			json:        `[{"key":"custom-label","operator":"Gt","values":["100"]}]`,
			allowedKeys: []string{"custom-label"},
			wantErr:     true,
			errMsg:      "is only supported for keys",
		},
		{
			name:        "Lt on custom key",
			json:        `[{"key":"custom-label","operator":"Lt","values":["100"]}]`,
			allowedKeys: []string{"custom-label"},
			wantErr:     true,
			errMsg:      "is only supported for keys",
		},

		// ---- unsupported operator ----
		{
			name:        "unsupported operator",
			json:        `[{"key":"label","operator":"BadOp"}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "unsupported operator",
		},
		{
			name:        "empty operator string",
			json:        `[{"key":"label","operator":""}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "unsupported operator",
		},
		{
			name:    "unauthorized custom key",
			json:    `[{"key":"gpu","operator":"Exists"}]`,
			wantErr: true,
			errMsg:  `node selector key "gpu" is not allowed`,
		},
		{
			name:    "unauthorized internal key probe",
			json:    `[{"key":"topology.kubernetes.io/disaster-recover-group-id","operator":"Exists"}]`,
			wantErr: true,
			errMsg:  `node selector key "topology.kubernetes.io/disaster-recover-group-id" is not allowed`,
		},

		// ---- size / complexity limits (DoS hardening) ----
		{
			name:    "JSON exceeds 4KB limit",
			json:    `[{"key":"` + strings.Repeat("k", 4*1024) + `","operator":"Exists"}]`,
			wantErr: true,
			errMsg:  "exceeds maximum size",
		},
		{
			name: "more than 10 selector requirements",
			json: `[
				{"key":"k0","operator":"Exists"},
				{"key":"k1","operator":"Exists"},
				{"key":"k2","operator":"Exists"},
				{"key":"k3","operator":"Exists"},
				{"key":"k4","operator":"Exists"},
				{"key":"k5","operator":"Exists"},
				{"key":"k6","operator":"Exists"},
				{"key":"k7","operator":"Exists"},
				{"key":"k8","operator":"Exists"},
				{"key":"k9","operator":"Exists"},
				{"key":"k10","operator":"Exists"}
			]`,
			wantErr: true,
			errMsg:  "allows at most",
		},
		{
			name:        "In with more than 50 values",
			json:        `[{"key":"label","operator":"In","values":[` + strings.Repeat(`"v",`, 50) + `"v"]}]`,
			allowedKeys: []string{"label"},
			wantErr:     true,
			errMsg:      "allows at most",
		},

		// ---- second element fails validation ----
		{
			name: "first valid, second invalid",
			json: `[
				{"key":"gpu","operator":"Exists"},
				{"key":"","operator":"In","values":["v"]}
			]`,
			allowedKeys: []string{"gpu"},
			wantErr:     true,
			errMsg:      "node selector key must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.allowedKeys != nil {
				withNodeAffinitySelectorAllowedKeys(t, tt.allowedKeys...)
			}
			result, err := parseNodeAffinitySelector(tt.json)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				if tt.check != nil {
					tt.check(t, result)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// constructNodeAffinity tests
// ---------------------------------------------------------------------------

func Test_constructNodeAffinity(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		req         *types.CreateCubeSandboxReq
		allowedKeys []string
		wantErr     bool
		errMsg      string
		check       func(t *testing.T, result []affinity.NodeSelectorRequirement)
	}{
		{
			name: "nil annotations",
			req: &types.CreateCubeSandboxReq{
				Request:      &types.Request{RequestID: "req-1"},
				Annotations:  nil,
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				// nil annotations produce no annotation-derived expressions
				// (Go nil slice is fine)
			},
		},
		{
			name: "empty annotations",
			req: &types.CreateCubeSandboxReq{
				Request:      &types.Request{RequestID: "req-2"},
				Annotations:  map[string]string{},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				// empty annotations produce no annotation-derived expressions
			},
		},
		{
			name: "only cluster label",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-3"},
				Annotations: map[string]string{
					"com.nodeaffinity.cluster.label": "cls-a:cls-b",
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				found := false
				for _, r := range result {
					if r.Key == "topology.kubernetes.io/cluster-id" {
						found = true
						assert.Equal(t, affinity.NodeSelectorOpIn, r.Operator)
						assert.NotEmpty(t, r.Values)
					}
				}
				assert.True(t, found, "expected cluster-id requirement")
			},
		},
		{
			name: "only instance type",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-4"},
				Annotations: map[string]string{
					"com.nodeaffinity.instancetype": "SA2:SA3",
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				found := false
				for _, r := range result {
					if r.Key == "kubernetes.io/instance-type" {
						found = true
						assert.Equal(t, affinity.NodeSelectorOpIn, r.Operator)
						assert.NotEmpty(t, r.Values)
					}
				}
				assert.True(t, found, "expected instance-type requirement")
			},
		},
		{
			name:        "valid affinity selector only",
			allowedKeys: []string{"gpu"},
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-5"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `[{"key":"gpu","operator":"Exists"}]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				found := false
				for _, r := range result {
					if r.Key == "gpu" {
						found = true
						assert.Equal(t, affinity.NodeSelectorOpExists, r.Operator)
					}
				}
				assert.True(t, found, "expected gpu Exists requirement from selector annotation")
			},
		},
		{
			name:        "cluster label + instance type + selector combined",
			allowedKeys: []string{"gpu", "custom"},
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-6"},
				Annotations: map[string]string{
					"com.nodeaffinity.cluster.label": "cls-a",
					"com.nodeaffinity.instancetype":  "SA2",
					"com.nodeaffinity.selector":      `[{"key":"gpu","operator":"Exists"},{"key":"custom","operator":"In","values":["v1","v2"]}]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				keys := make(map[string]int)
				for _, r := range result {
					keys[r.Key]++
				}
				assert.Equal(t, 1, keys["topology.kubernetes.io/cluster-id"], "cluster-id requirement")
				assert.Equal(t, 1, keys["kubernetes.io/instance-type"], "instance-type requirement")
				assert.Equal(t, 1, keys["gpu"], "gpu from selector")
				assert.Equal(t, 1, keys["custom"], "custom from selector")
			},
		},
		{
			name:        "selector with Gt and Lt and Exists",
			allowedKeys: []string{"ssd"},
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-7"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `[
						{"key":"kubernetes.io/memory-size","operator":"Gt","values":["4096Mi"]},
						{"key":"kubernetes.io/cpu-cores","operator":"Lt","values":["16000m"]},
						{"key":"ssd","operator":"Exists"}
					]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				gtFound, ltFound, existsFound := false, false, false
				for _, r := range result {
					switch r.Key {
					case "kubernetes.io/memory-size":
						gtFound = true
						assert.Equal(t, affinity.NodeSelectorOpGt, r.Operator)
					case "kubernetes.io/cpu-cores":
						ltFound = true
						assert.Equal(t, affinity.NodeSelectorOpLt, r.Operator)
					case "ssd":
						existsFound = true
						assert.Equal(t, affinity.NodeSelectorOpExists, r.Operator)
					}
				}
				assert.True(t, gtFound && ltFound && existsFound,
					"all three selector requirements should be present")
			},
		},
		{
			name: "Gt on memory-size with Gi unit",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-9"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `[{"key":"kubernetes.io/memory-size","operator":"Gt","values":["4Gi"]}]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				found := false
				for _, r := range result {
					if r.Key == "kubernetes.io/memory-size" {
						found = true
						assert.Contains(t, r.Values, "4Gi")
					}
				}
				assert.True(t, found)
			},
		},

		// ---- error cases ----
		{
			name: "invalid selector JSON returns error",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-err-1"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `not-valid-json`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			wantErr: true,
			errMsg:  "parsing annotation",
		},
		{
			name: "selector with empty key returns error",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-err-2"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `[{"key":"","operator":"In","values":["v"]}]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			wantErr: true,
			errMsg:  "node selector key must not be empty",
		},
		{
			name:        "selector with unsupported Gt key returns error",
			allowedKeys: []string{"bad-key"},
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-err-3"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `[{"key":"bad-key","operator":"Gt","values":["1"]}]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			wantErr: true,
			errMsg:  "is only supported for keys",
		},
		{
			name: "selector with unauthorized custom key returns error",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-err-4"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": `[{"key":"gpu","operator":"Exists"}]`,
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			wantErr: true,
			errMsg:  `node selector key "gpu" is not allowed`,
		},
		{
			name: "empty selector string is silently ignored",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-8"},
				Annotations: map[string]string{
					"com.nodeaffinity.selector": "",
				},
				Containers:   []*types.Container{},
				InstanceType: "cubebox",
			},
			check: func(t *testing.T, result []affinity.NodeSelectorRequirement) {
				// Empty selector string → treated as not set, no extra requirements
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.allowedKeys != nil {
				withNodeAffinitySelectorAllowedKeys(t, tt.allowedKeys...)
			}
			result, err := constructNodeAffinity(ctx, tt.req)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				assert.NoError(t, err)
				if tt.check != nil {
					tt.check(t, result)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// runInsReq2Affinity tests
// ---------------------------------------------------------------------------

func Test_runInsReq2Affinity(t *testing.T) {
	ctx := context.Background()

	t.Run("no annotations returns context unchanged", func(t *testing.T) {
		req := &types.CreateCubeSandboxReq{
			Request:      &types.Request{RequestID: "req-1"},
			Annotations:  nil,
			Containers:   []*types.Container{},
			InstanceType: "cubebox",
		}
		newCtx, err := runInsReq2Affinity(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, newCtx)
	})

	t.Run("valid selector stores NodeSelector in context", func(t *testing.T) {
		withNodeAffinitySelectorAllowedKeys(t, "gpu")
		req := &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "req-2"},
			Annotations: map[string]string{
				"com.nodeaffinity.selector": `[{"key":"gpu","operator":"Exists"}]`,
			},
			Containers:   []*types.Container{},
			InstanceType: "cubebox",
		}
		newCtx, err := runInsReq2Affinity(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, newCtx)

		ns := constants.GetNodeSelector(newCtx)
		assert.NotNil(t, ns, "NodeSelector should be stored in context")
	})

	t.Run("invalid selector returns error", func(t *testing.T) {
		req := &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "req-3"},
			Annotations: map[string]string{
				"com.nodeaffinity.selector": `[{"key":"","operator":"In","values":["v"]}]`,
			},
			Containers:   []*types.Container{},
			InstanceType: "cubebox",
		}
		newCtx, err := runInsReq2Affinity(ctx, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "node selector key must not be empty")
		assert.NotNil(t, newCtx)
	})

	t.Run("instance type annotation also sets backoff NodeSelector", func(t *testing.T) {
		withNodeAffinitySelectorAllowedKeys(t, "ssd")
		req := &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "req-4"},
			Annotations: map[string]string{
				"com.nodeaffinity.instancetype": "SA2:SA3",
				"com.nodeaffinity.selector":     `[{"key":"ssd","operator":"Exists"}]`,
			},
			Containers:   []*types.Container{},
			InstanceType: "cubebox",
		}
		newCtx, err := runInsReq2Affinity(ctx, req)
		assert.NoError(t, err)

		ns := constants.GetNodeSelector(newCtx)
		assert.NotNil(t, ns, "NodeSelector should be set")

		bns := constants.GetBackoffNodeSelector(newCtx)
		assert.Nil(t, bns, "BackoffNodeSelector should be nil when instance type annotation is present")
	})
}
