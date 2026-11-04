// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/sandboxrestart"
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

func TestCheckAndGetAnnotationStripsPlatformPauseKeys(t *testing.T) {
	ensureSandboxTestConfig(t)
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationPauseSnapshotID:                  "snap-forged-pause",
			constants.CubeAnnotationLaunchMemorySnapshotID:           "tpl-forged",
			constants.CubeAnnotationRuntimeRestoreSnapshotID:         "snap-forged-restore",
			constants.CubeAnnotationRuntimeRestoreSnapshotAttachedAt: "2026-09-04T00:00:00Z",
			constants.CubeAnnotationRuntimeSnapshotID:                "snap-user-runtime",
		},
	}
	out := &cubebox.RunCubeSandboxRequest{}
	if err := checkAndGetAnnotation(req, out); err != nil {
		t.Fatalf("checkAndGetAnnotation: %v", err)
	}
	if _, ok := out.Annotations[constants.CubeAnnotationPauseSnapshotID]; ok {
		t.Fatal("user pause snapshot id must not be forwarded")
	}
	if _, ok := out.Annotations[constants.CubeAnnotationLaunchMemorySnapshotID]; ok {
		t.Fatal("user launch ancestor must not be forwarded")
	}
	if _, ok := out.Annotations[constants.CubeAnnotationRuntimeRestoreSnapshotID]; ok {
		t.Fatal("user restore-base must not be forwarded")
	}
	if got := out.Annotations[constants.CubeAnnotationRuntimeSnapshotID]; got != "snap-user-runtime" {
		t.Fatalf("other cube.master keys still forward, got %q", got)
	}
}

func TestStripUserCubeMasterLabelsDropsPauseKeys(t *testing.T) {
	got := stripUserCubeMasterLabels(map[string]string{
		constants.CubeAnnotationPauseSnapshotID:                  "snap-forged-pause",
		constants.CubeAnnotationLaunchMemorySnapshotID:           "tpl-forged",
		constants.CubeAnnotationRuntimeRestoreSnapshotID:         "snap-forged-restore",
		constants.CubeAnnotationRuntimeRestoreSnapshotAttachedAt: "2026-09-04T00:00:00Z",
		"user.label": "keep-me",
	})
	if _, ok := got[constants.CubeAnnotationPauseSnapshotID]; ok {
		t.Fatal("forged pause snapshot id must not reach Cubelet Labels")
	}
	if _, ok := got[constants.CubeAnnotationLaunchMemorySnapshotID]; ok {
		t.Fatal("forged launch ancestor must not reach Cubelet Labels")
	}
	if _, ok := got[constants.CubeAnnotationRuntimeRestoreSnapshotID]; ok {
		t.Fatal("forged restore-base must not reach Cubelet Labels")
	}
	if _, ok := got[constants.CubeAnnotationRuntimeRestoreSnapshotAttachedAt]; ok {
		t.Fatal("forged restore-base timestamp must not reach Cubelet Labels")
	}
	if got["user.label"] != "keep-me" {
		t.Fatalf("ordinary labels must pass through, got %#v", got)
	}
	if stripUserCubeMasterLabels(nil) != nil {
		t.Fatal("nil labels stay nil")
	}
}

func TestConstructCubeletReqStripsForgedPauseLabels(t *testing.T) {
	ensureSandboxTestConfig(t)
	req := &types.CreateCubeSandboxReq{
		Request: &types.Request{RequestID: "req-label-strip"},
		Containers: []*types.Container{{
			Name: "ctr-1",
			Resources: &types.Resource{
				Cpu: "1",
				Mem: "1Gi",
			},
			Image: &types.ImageSpec{
				Image: "busybox:latest",
			},
		}},
		Annotations: map[string]string{
			constants.CubeAnnotationPauseSnapshotID:          "snap-anno-forged",
			constants.CubeAnnotationRuntimeRestoreSnapshotID: "snap-anno-restore",
		},
		Labels: map[string]string{
			constants.CubeAnnotationPauseSnapshotID:          "snap-label-forged",
			constants.CubeAnnotationRuntimeRestoreSnapshotID: "snap-label-restore",
			"app": "ok",
		},
	}
	out, err := ConstructCubeletReq(context.Background(), req)
	if err != nil {
		t.Fatalf("ConstructCubeletReq: %v", err)
	}
	if _, ok := out.Labels[constants.CubeAnnotationPauseSnapshotID]; ok {
		t.Fatal("user Label pause snapshot id must not be forwarded")
	}
	if _, ok := out.Labels[constants.CubeAnnotationRuntimeRestoreSnapshotID]; ok {
		t.Fatal("user Label restore-base must not be forwarded")
	}
	if out.Labels["app"] != "ok" {
		t.Fatalf("ordinary labels must pass through, got %#v", out.Labels)
	}
	if _, ok := out.Annotations[constants.CubeAnnotationPauseSnapshotID]; ok {
		t.Fatal("user annotation pause snapshot id must not be re-added after strip")
	}
	if _, ok := out.Annotations[constants.CubeAnnotationRuntimeRestoreSnapshotID]; ok {
		t.Fatal("user annotation restore-base must not be re-added after strip")
	}
}

func TestValidateRestartLiveness(t *testing.T) {
	probe := &cubebox.LivenessProbe{
		ProbeHandler: &cubebox.ProbeHandler{HttpGet: &cubebox.HTTPGetAction{Port: 49983}},
	}
	container := func(p *cubebox.LivenessProbe) []*types.Container {
		return []*types.Container{{Name: "ctr-0", LivenessProbe: p}}
	}

	rejected := []struct {
		name string
		req  *types.CreateCubeSandboxReq
	}{
		{
			name: "Never with a request-level probe",
			req:  &types.CreateCubeSandboxReq{RestartPolicy: "Never", LivenessProbe: probe},
		},
		{
			name: "Never with a container-level probe",
			req:  &types.CreateCubeSandboxReq{RestartPolicy: "Never", Containers: container(probe)},
		},
		{
			name: "on-wire Never spelling with a probe",
			req:  &types.CreateCubeSandboxReq{RestartPolicy: "RESTART_POLICY_NEVER", LivenessProbe: probe},
		},
		{
			// Empty means Never, so a probe without an explicit policy is
			// rejected too, matching Cubelet's Validate.
			name: "empty policy with a probe",
			req:  &types.CreateCubeSandboxReq{LivenessProbe: probe},
		},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRestartLiveness(tc.req)
			if err == nil {
				t.Fatal("expected Never + liveness probe to be rejected")
			}
			if !strings.Contains(err.Error(), "liveness_probe requires restart policy") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	accepted := []struct {
		name string
		req  *types.CreateCubeSandboxReq
	}{
		{
			name: "OnFailure with a probe",
			req:  &types.CreateCubeSandboxReq{RestartPolicy: "OnFailure", LivenessProbe: probe},
		},
		{
			name: "Always with a container probe",
			req:  &types.CreateCubeSandboxReq{RestartPolicy: "Always", Containers: container(probe)},
		},
		{
			name: "Never without a probe",
			req:  &types.CreateCubeSandboxReq{RestartPolicy: "Never"},
		},
		{
			name: "empty policy without a probe",
			req:  &types.CreateCubeSandboxReq{},
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateRestartLiveness(tc.req); err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
		})
	}
}

func restartTestProbe() *cubebox.LivenessProbe {
	return &cubebox.LivenessProbe{
		ProbeHandler: &cubebox.ProbeHandler{
			HttpGet: &cubebox.HTTPGetAction{Port: 49983},
		},
	}
}

func TestApplyRestartPolicyForwardsRestartFields(t *testing.T) {
	topProbe := restartTestProbe()
	containerProbe := restartTestProbe()
	backoff := &cubebox.RestartBackoffConfig{InitialIntervalSecond: 5, MaxRestarts: 3}

	cases := []struct {
		name               string
		req                *types.CreateCubeSandboxReq
		wantPolicy         string
		wantBackoff        *cubebox.RestartBackoffConfig
		wantTopProbe       bool
		wantContainerProbe bool
	}{
		{
			name: "policy, backoff and top-level probe",
			req: &types.CreateCubeSandboxReq{
				RestartPolicy:  "OnFailure",
				RestartBackoff: backoff,
				LivenessProbe:  topProbe,
				Containers:     []*types.Container{{Name: "ctr-0"}},
			},
			wantPolicy:   sandboxrestart.WireOnFailure,
			wantBackoff:  backoff,
			wantTopProbe: true,
		},
		{
			name: "container probe and on-wire policy",
			req: &types.CreateCubeSandboxReq{
				RestartPolicy: "RESTART_POLICY_ALWAYS",
				Containers:    []*types.Container{{Name: "ctr-0", LivenessProbe: containerProbe}},
			},
			wantPolicy:         sandboxrestart.WireAlways,
			wantContainerProbe: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{{Name: "ctr-0"}},
			}
			if err := applyRestartPolicy(tc.req, out); err != nil {
				t.Fatalf("applyRestartPolicy: %v", err)
			}
			assert.Equal(t, tc.wantPolicy, out.RestartPolicy)
			if tc.wantBackoff == nil {
				assert.Nil(t, out.RestartBackoff)
			} else {
				assert.Same(t, tc.wantBackoff, out.RestartBackoff)
			}
			if tc.wantTopProbe {
				assert.Same(t, topProbe, out.Containers[0].LivenessProbe)
			}
			if tc.wantContainerProbe {
				assert.Same(t, containerProbe, out.Containers[0].LivenessProbe)
			}
		})
	}
}

func TestApplyRestartPolicyNoopWithoutRestartFields(t *testing.T) {
	out := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{{Name: "ctr-0"}},
	}
	if err := applyRestartPolicy(&types.CreateCubeSandboxReq{}, out); err != nil {
		t.Fatalf("applyRestartPolicy: %v", err)
	}
	assert.Empty(t, out.RestartPolicy)
	assert.Nil(t, out.RestartBackoff)
	assert.Nil(t, out.Containers[0].LivenessProbe)
}

func TestApplyRestartPolicyRejectsInvalidPolicy(t *testing.T) {
	err := applyRestartPolicy(
		&types.CreateCubeSandboxReq{RestartPolicy: "Sometimes"},
		&cubebox.RunCubeSandboxRequest{},
	)
	if err == nil {
		t.Fatal("expected an invalid policy error")
	}
	assert.Contains(t, err.Error(), "invalid restart policy")
}

func TestApplyRestartPolicyClampsMaxRestarts(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{CubeletConf: &config.CubeletConf{MaxRestartsCap: 7}}
	})

	cases := []struct {
		name string
		in   int32
		want int32
	}{
		{name: "unlimited is tightened to the cap", in: 0, want: 7},
		{name: "above the cap is tightened", in: 10, want: 7},
		{name: "below the cap is kept", in: 3, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &cubebox.RunCubeSandboxRequest{}
			req := &types.CreateCubeSandboxReq{
				RestartPolicy:  "OnFailure",
				RestartBackoff: &cubebox.RestartBackoffConfig{MaxRestarts: tc.in},
			}
			if err := applyRestartPolicy(req, out); err != nil {
				t.Fatalf("applyRestartPolicy: %v", err)
			}
			if out.RestartBackoff == nil {
				t.Fatal("expected restart backoff to be forwarded")
			}
			assert.Equal(t, tc.want, out.RestartBackoff.GetMaxRestarts())
		})
	}
}

func TestCopyTemplateRestartFields(t *testing.T) {
	templateProbe := restartTestProbe()
	templateBackoff := &cubebox.RestartBackoffConfig{MaxRestarts: 2}
	templateReq := &types.CreateCubeSandboxReq{
		RestartPolicy:  "OnFailure",
		RestartBackoff: templateBackoff,
		LivenessProbe:  templateProbe,
	}

	req := &types.CreateCubeSandboxReq{}
	CopyTemplateRestartFields(templateReq, req)
	assert.Equal(t, "OnFailure", req.RestartPolicy)
	assert.Same(t, templateBackoff, req.RestartBackoff)
	assert.Same(t, templateProbe, req.LivenessProbe)

	callerProbe := restartTestProbe()
	callerBackoff := &cubebox.RestartBackoffConfig{MaxRestarts: 9}
	callerReq := &types.CreateCubeSandboxReq{
		RestartPolicy:  "Never",
		RestartBackoff: callerBackoff,
		LivenessProbe:  callerProbe,
	}
	CopyTemplateRestartFields(templateReq, callerReq)
	assert.Equal(t, "Never", callerReq.RestartPolicy)
	assert.Same(t, callerBackoff, callerReq.RestartBackoff)
	assert.Same(t, callerProbe, callerReq.LivenessProbe)

	CopyTemplateRestartFields(nil, req)
	CopyTemplateRestartFields(templateReq, nil)
}

func TestTemplateRestartFieldsMergeThenValidate(t *testing.T) {
	probe := restartTestProbe()
	cases := []struct {
		name     string
		template *types.CreateCubeSandboxReq
		caller   *types.CreateCubeSandboxReq
		wantErr  bool
	}{
		{
			name:     "template policy and probe satisfy the rule",
			template: &types.CreateCubeSandboxReq{RestartPolicy: "OnFailure", LivenessProbe: probe},
			caller:   &types.CreateCubeSandboxReq{},
		},
		{
			name:     "template probe without a policy stays Never",
			template: &types.CreateCubeSandboxReq{LivenessProbe: probe},
			caller:   &types.CreateCubeSandboxReq{},
			wantErr:  true,
		},
		{
			name:     "caller Never wins and conflicts with the template probe",
			template: &types.CreateCubeSandboxReq{RestartPolicy: "OnFailure", LivenessProbe: probe},
			caller:   &types.CreateCubeSandboxReq{RestartPolicy: "Never"},
			wantErr:  true,
		},
		{
			name:     "caller OnFailure accepts the template probe",
			template: &types.CreateCubeSandboxReq{LivenessProbe: probe},
			caller:   &types.CreateCubeSandboxReq{RestartPolicy: "OnFailure"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			CopyTemplateRestartFields(tc.template, tc.caller)
			err := ValidateRestartLiveness(tc.caller)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "liveness_probe requires restart policy")
				return
			}
			assert.NoError(t, err)
		})
	}
}
