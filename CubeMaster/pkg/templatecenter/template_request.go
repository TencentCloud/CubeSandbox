// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
)

func generateTemplateCreateRequest(req *types.CreateTemplateFromImageReq, artifact *models.RootfsArtifact, imageCfg image.DockerImageConfig, downloadBaseURL string) (*types.CreateCubeSandboxReq, error) {
	annotations := map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID:      req.TemplateID,
		constants.CubeAnnotationsAppSnapshotCreate:         "true",
		constants.CubeAnnotationAppSnapshotVersion:         DefaultTemplateVersion,
		constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		constants.CubeAnnotationRootfsArtifactID:           artifact.ArtifactID,
		constants.CubeAnnotationWritableLayerSize:          req.WritableLayerSize,
		constants.CubeAnnotationTemplateSpecFingerprint:    artifact.TemplateSpecFingerprint,
	}
	sizeGi, err := quantityToGi(req.WritableLayerSize)
	if err == nil && sizeGi > 0 {
		annotations[constants.CubeAnnotationsSystemDiskSize] = strconv.FormatInt(sizeGi, 10)
	}
	if len(req.ExposedPorts) > 0 {
		annotations[constants.AnnotationsExposedPort] = formatExposedPortsAnnotation(req.ExposedPorts)
	}
	if req.EnableIvshmem != nil && *req.EnableIvshmem {
		annotations[constants.CubeAnnotationEnableIvshmem] = "true"
	}
	rootVolume := &types.Volume{
		Name: rootfsWritableVolumeName,
		VolumeSource: &types.VolumeSource{
			EmptyDir: &types.EmptyDirVolumeSource{
				SizeLimit: req.WritableLayerSize,
			},
		},
	}
	imageAnnotations := map[string]string{
		constants.CubeAnnotationRootfsArtifactID:        artifact.ArtifactID,
		constants.CubeAnnotationRootfsArtifactURL:       buildDownloadURL(downloadBaseURL, artifact.ArtifactID, artifact.DownloadToken),
		constants.CubeAnnotationRootfsArtifactToken:     artifact.DownloadToken,
		constants.CubeAnnotationRootfsArtifactSHA256:    artifact.Ext4SHA256,
		constants.CubeAnnotationRootfsArtifactSizeBytes: strconv.FormatInt(artifact.Ext4SizeBytes, 10),
		constants.CubeAnnotationWritableLayerSize:       req.WritableLayerSize,
		constants.CubeAnnotationTemplateSpecFingerprint: artifact.TemplateSpecFingerprint,
	}
	command := imageCfg.Entrypoint
	args := imageCfg.Cmd
	if req.ContainerOverrides != nil {
		if len(req.ContainerOverrides.Command) > 0 {
			command = req.ContainerOverrides.Command
		}
		if len(req.ContainerOverrides.Args) > 0 {
			args = req.ContainerOverrides.Args
		}
	}
	envs := envListToKeyValues(imageCfg.Env)
	if req.ContainerOverrides != nil && req.ContainerOverrides.Envs != nil {
		envs = req.ContainerOverrides.Envs
	}
	workingDir := imageCfg.WorkingDir
	if req.ContainerOverrides != nil && req.ContainerOverrides.WorkingDir != "" {
		workingDir = req.ContainerOverrides.WorkingDir
	}
	resources := &types.Resource{Cpu: defaultTemplateCPU, Mem: defaultTemplateMemory}
	if req.ContainerOverrides != nil && req.ContainerOverrides.Resources != nil {
		resources = req.ContainerOverrides.Resources
	}
	securityContext := &types.ContainerSecurityContext{Privileged: true, ReadonlyRootfs: false}
	if req.ContainerOverrides != nil && req.ContainerOverrides.SecurityContext != nil {
		securityContext = req.ContainerOverrides.SecurityContext
		securityContext.ReadonlyRootfs = false
	}
	if req.ContainerOverrides != nil && req.ContainerOverrides.VolumeMounts != nil {
		for _, mount := range req.ContainerOverrides.VolumeMounts {
			if mount != nil && mount.ContainerPath == "/" {
				return nil, fmt.Errorf("container_overrides.volume_mounts must not override / because writable rootfs is template-owned")
			}
		}
	}
	volumeMounts := []*cubeboxv1.VolumeMounts{{
		Name:          rootfsWritableVolumeName,
		ContainerPath: "/",
	}}
	if req.ContainerOverrides != nil && len(req.ContainerOverrides.VolumeMounts) > 0 {
		volumeMounts = append(volumeMounts, req.ContainerOverrides.VolumeMounts...)
	}
	containerAnnotations := map[string]string{}
	if req.ContainerOverrides != nil && req.ContainerOverrides.Annotations != nil {
		for k, v := range req.ContainerOverrides.Annotations {
			containerAnnotations[k] = v
		}
	}
	container := &types.Container{
		Name:            "cubebox-name-0",
		Image:           &types.ImageSpec{Image: artifact.ArtifactID, StorageMedia: imagev1.ImageStorageMediaType_ext4.String(), WritableLayerSize: req.WritableLayerSize, Annotations: imageAnnotations},
		Command:         command,
		Args:            args,
		WorkingDir:      workingDir,
		Envs:            envs,
		VolumeMounts:    volumeMounts,
		DnsConfig:       dnsConfigOrNil(req.ContainerOverrides),
		RLimit:          defaultRLimit(req.ContainerOverrides),
		Resources:       resources,
		SecurityContext: securityContext,
		Probe:           probeOrNil(req.ContainerOverrides),
		Annotations:     containerAnnotations,
	}
	return &types.CreateCubeSandboxReq{
		Request:           &types.Request{RequestID: req.RequestID},
		Volumes:           []*types.Volume{rootVolume},
		Containers:        []*types.Container{container},
		Annotations:       annotations,
		InstanceType:      req.InstanceType,
		NetworkType:       req.NetworkType,
		CubeNetworkConfig: cloneCubeNetworkConfig(req.CubeNetworkConfig),
	}, nil
}

func cloneCubeNetworkConfig(in *types.CubeNetworkConfig) *types.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &types.CubeNetworkConfig{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
		Rules:    cloneEgressRules(in.Rules),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := *in.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func cloneEgressRules(in []*types.EgressRule) []*types.EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*types.EgressRule, 0, len(in))
	for _, r := range in {
		out = append(out, cloneEgressRule(r))
	}
	return out
}

func cloneEgressRule(in *types.EgressRule) *types.EgressRule {
	if in == nil {
		return nil
	}
	out := &types.EgressRule{Name: in.Name}
	if in.Match != nil {
		out.Match = &types.EgressRuleMatch{
			SNI:    cloneStringPtr(in.Match.SNI),
			Host:   cloneStringPtr(in.Match.Host),
			Method: append([]string(nil), in.Match.Method...),
			Path:   cloneStringPtr(in.Match.Path),
			Scheme: cloneStringPtr(in.Match.Scheme),
		}
	}
	if in.Action != nil {
		action := &types.EgressRuleAction{Allow: in.Action.Allow}
		action.Audit = cloneStringPtr(in.Action.Audit)
		if len(in.Action.Inject) > 0 {
			action.Inject = make([]*types.EgressRuleInject, 0, len(in.Action.Inject))
			for _, inj := range in.Action.Inject {
				if inj == nil {
					continue
				}
				cloned := &types.EgressRuleInject{
					Header: inj.Header,
					Secret: inj.Secret,
					Format: cloneStringPtr(inj.Format),
				}
				action.Inject = append(action.Inject, cloned)
			}
		}
		out.Action = action
	}
	return out
}

func cloneStringPtr(in *string) *string {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func formatTemplateImageCubeNetworkConfig(in *types.CubeNetworkConfig) string {
	if in == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allowInternetAccess := "default(true)"
	if in.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *in.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d",
		allowInternetAccess, in.AllowOut, in.DenyOut, len(in.Rules))
}

func dnsConfigOrNil(overrides *types.ContainerOverrides) *types.DNSConfig {
	if overrides == nil {
		return nil
	}
	return overrides.DnsConfig
}

func envListToKeyValues(envs []string) []*types.KeyValue {
	if len(envs) == 0 {
		return nil
	}
	out := make([]*types.KeyValue, 0, len(envs))
	for _, env := range envs {
		parts := strings.SplitN(env, "=", 2)
		kv := &types.KeyValue{Key: parts[0]}
		if len(parts) == 2 {
			kv.Value = parts[1]
		}
		out = append(out, kv)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out
}

func defaultRLimit(overrides *types.ContainerOverrides) *types.RLimit {
	if overrides != nil && overrides.RLimit != nil {
		return overrides.RLimit
	}
	return &types.RLimit{NoFile: 1000000}
}

func probeOrNil(overrides *types.ContainerOverrides) *types.Probe {
	if overrides == nil {
		return nil
	}
	return overrides.Probe
}

func buildDownloadURL(baseURL, artifactID, token string) string {
	trimmed := strings.TrimRight(image.NormalizeBaseURL(baseURL), "/")
	if trimmed == "" {
		trimmed = "http://" + artifactRootHostHint()
	}
	u, err := url.Parse(trimmed + "/cube/template/artifact/download")
	if err != nil {
		return trimmed
	}
	query := u.Query()
	query.Set("artifact_id", artifactID)
	query.Set("token", token)
	u.RawQuery = query.Encode()
	return u.String()
}

func artifactRootHostHint() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "127.0.0.1"
	}
	return host
}

func quantityToGi(value string) (int64, error) {
	v := strings.TrimSpace(strings.ToLower(value))
	switch {
	case strings.HasSuffix(v, "gi"):
		return strconv.ParseInt(strings.TrimSuffix(v, "gi"), 10, 64)
	case strings.HasSuffix(v, "g"):
		return strconv.ParseInt(strings.TrimSuffix(v, "g"), 10, 64)
	case strings.HasSuffix(v, "mi"):
		mi, err := strconv.ParseInt(strings.TrimSuffix(v, "mi"), 10, 64)
		if err != nil {
			return 0, err
		}
		if mi%1024 == 0 {
			return mi / 1024, nil
		}
		return mi/1024 + 1, nil
	default:
		return strconv.ParseInt(v, 10, 64)
	}
}

func formatExposedPortsAnnotation(ports []int32) string {
	if len(ports) == 0 {
		return ""
	}
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, strconv.FormatInt(int64(port), 10))
	}
	return strings.Join(values, ":")
}
