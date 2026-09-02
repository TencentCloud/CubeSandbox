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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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

func TestValidateEgressRuleMatch(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	intPtr := func(i int) *int { return &i }

	valid := []struct {
		name  string
		match *types.EgressRuleMatch
	}{
		{"nil match", nil},
		{"empty match", &types.EgressRuleMatch{}},
		{"scheme only", &types.EgressRuleMatch{Scheme: strPtr("https")}},
		{"scheme case variant", &types.EgressRuleMatch{Scheme: strPtr("HTTPS")}},
		{"scheme with spaces", &types.EgressRuleMatch{Scheme: strPtr(" http ")}},
		{"port with scheme", &types.EgressRuleMatch{Port: intPtr(8443), Scheme: strPtr("https")}},
		{"port boundary low", &types.EgressRuleMatch{Port: intPtr(1), Scheme: strPtr("http")}},
		{"port boundary high", &types.EgressRuleMatch{Port: intPtr(65535), Scheme: strPtr("https")}},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, validateEgressRuleMatch(tt.match, 0))
		})
	}

	invalid := []struct {
		name    string
		match   *types.EgressRuleMatch
		wantErr string
	}{
		{"port without scheme", &types.EgressRuleMatch{Port: intPtr(8443)}, "requires match.scheme"},
		{"port zero", &types.EgressRuleMatch{Port: intPtr(0), Scheme: strPtr("http")}, "[1, 65535]"},
		{"port negative", &types.EgressRuleMatch{Port: intPtr(-1), Scheme: strPtr("http")}, "[1, 65535]"},
		{"port too large", &types.EgressRuleMatch{Port: intPtr(65536), Scheme: strPtr("http")}, "[1, 65535]"},
		{"scheme not http", &types.EgressRuleMatch{Scheme: strPtr("ftp")}, "must be 'http' or 'https'"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEgressRuleMatch(tt.match, 0)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateMaskRequestHost(t *testing.T) {
	for _, value := range []string{
		"localhost",
		"localhost:3000",
		"localhost:${PORT}",
		"my-app.example.com:${PORT}",
		"127.0.0.1:3000",
		"[::1]:${PORT}",
	} {
		t.Run("valid_"+value, func(t *testing.T) {
			assert.NoError(t, validateMaskRequestHost(value))
		})
	}

	for _, value := range []string{
		"",
		" localhost",
		"localhost ",
		"https://example.com",
		"example.com/path",
		"example.com?x=1",
		"example.com#fragment",
		"user@example.com",
		"bad\r\nInjected: value",
		"example.com:",
		"example.com:0",
		"example.com:99999",
		"localhost:${OTHER}",
		"localhost:${PORT",
		"[::1",
		"::1",
		"[::1]]:3000",
		"[::1]:",
		"例子.测试",
	} {
		t.Run("invalid_"+value, func(t *testing.T) {
			assert.Error(t, validateMaskRequestHost(value))
		})
	}
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
