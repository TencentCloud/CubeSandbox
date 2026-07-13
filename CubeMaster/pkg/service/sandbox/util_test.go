// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func ensureSandboxTestConfig(t *testing.T) *config.Config {
	t.Helper()
	if cfg := config.GetConfig(); cfg != nil {
		return cfg
	}
	mydir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	cfgPath := filepath.Clean(filepath.Join(mydir, "../../../conf.yaml"))
	if err := os.Setenv("CUBE_MASTER_CONFIG_PATH", cfgPath); err != nil {
		t.Fatalf("set CUBE_MASTER_CONFIG_PATH failed: %v", err)
	}
	cfg, err := config.Init()
	if err != nil {
		t.Fatalf("config.Init failed: %v", err)
	}
	return cfg
}

func Test_checkAndGetHostDirVolumeSource(t *testing.T) {
	type args struct {
		src *types.HostDirVolumeSources
		out *cubebox.Volume
	}
	tests := []struct {
		name      string
		args      args
		wantErr   bool
		wantPanic bool
	}{
		{
			name: "nil_src",
			args: args{
				src: nil,
				out: &cubebox.Volume{VolumeSource: &cubebox.VolumeSource{}},
			},
			wantErr: false,
		},
		{
			name: "empty_volume_sources",
			args: args{
				src: &types.HostDirVolumeSources{},
				out: &cubebox.Volume{VolumeSource: &cubebox.VolumeSource{}},
			},
			wantErr: false,
		},
		{
			name: "missing_name",
			args: args{
				src: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{
						{Name: "", HostPath: "/data/foo"},
					},
				},
				out: &cubebox.Volume{VolumeSource: &cubebox.VolumeSource{}},
			},
			wantErr: true,
		},
		{
			name: "missing_host_path",
			args: args{
				src: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{
						{Name: "vol1", HostPath: ""},
					},
				},
				out: &cubebox.Volume{VolumeSource: &cubebox.VolumeSource{}},
			},
			wantErr: true,
		},
		{
			name: "host_path_not_absolute",
			args: args{
				src: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{
						{Name: "vol1", HostPath: "relative/path"},
					},
				},
				out: &cubebox.Volume{VolumeSource: &cubebox.VolumeSource{}},
			},
			wantErr: true,
		},
		{
			name: "valid_single_source",
			args: args{
				src: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{
						{Name: "vol1", HostPath: "/data/shared"},
					},
				},
				out: &cubebox.Volume{VolumeSource: &cubebox.VolumeSource{}},
			},
			wantErr: false,
		},
		{
			name: "out_volumeSource_nil_panics",
			args: args{
				src: &types.HostDirVolumeSources{},
				out: &cubebox.Volume{},
			},
			wantPanic: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantPanic {
				assert.Panics(t, func() {
					_ = checkAndGetHostDirVolumeSource(tt.args.src, tt.args.out)
				})
				return
			}
			err := checkAndGetHostDirVolumeSource(tt.args.src, tt.args.out)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkAndGetHostDirVolumeSource() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && tt.args.src != nil {
				assert.NotNil(t, tt.args.out.VolumeSource.HostDirVolumes)
				assert.Equal(t, len(tt.args.src.VolumeSources), len(tt.args.out.VolumeSource.HostDirVolumes.VolumeSources))
			}
		})
	}
}

func TestGetReqResourceRejectsCPUOverflowWhenMemIsValid(t *testing.T) {
	cfg := ensureSandboxTestConfig(t)
	origScheduler := cfg.Scheduler
	cfg.Scheduler = &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			MaxMvmCPU:    "1",
			MaxMvmMemory: "8Gi",
		},
	}
	defer func() {
		cfg.Scheduler = origScheduler
	}()

	req := &types.CreateCubeSandboxReq{
		Containers: []*types.Container{{
			Name: "ctr-1",
			Resources: &types.Resource{
				Cpu: "2",
				Mem: "1Gi",
			},
		}},
	}

	_, _, err := getReqResource(req)
	if err == nil {
		t.Fatal("expected cpu overflow to return error")
	}
	if !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("expected cpu validation error, got %v", err)
	}
}

func TestGetReqResourceRejectsCPUOverflowBeforeMemOverflow(t *testing.T) {
	cfg := ensureSandboxTestConfig(t)
	origScheduler := cfg.Scheduler
	cfg.Scheduler = &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			MaxMvmCPU:    "1",
			MaxMvmMemory: "8Gi",
		},
	}
	defer func() {
		cfg.Scheduler = origScheduler
	}()

	req := &types.CreateCubeSandboxReq{
		Containers: []*types.Container{{
			Name: "ctr-1",
			Resources: &types.Resource{
				Cpu: "2",
				Mem: "9Gi",
			},
		}},
	}

	_, _, err := getReqResource(req)
	if err == nil {
		t.Fatal("expected cpu and mem overflow to return error")
	}
	if !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("expected cpu validation error to win, got %v", err)
	}
}
