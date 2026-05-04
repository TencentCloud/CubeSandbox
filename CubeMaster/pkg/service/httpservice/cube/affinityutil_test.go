// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

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
