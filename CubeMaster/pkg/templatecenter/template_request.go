// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func generateTemplateCreateRequest(ctx context.Context, req *types.CreateTemplateFromImageReq, artifact *models.RootfsArtifact, imageCfg DockerImageConfig, downloadBaseURL string) (*types.CreateCubeSandboxReq, error) {
	annotations := map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID:      req.TemplateID,
		constants.CubeAnnotationsAppSnapshotCreate:         "true",
		constants.CubeAnnotationAppSnapshotVersion:         DefaultTemplateVersion,
		constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		constants.CubeAnnotationRootfsArtifactID:           artifact.ArtifactID,
		constants.CubeAnnotationWritableLayerSize:          req.WritableLayerSize,
		constants.CubeAnnotationTemplateSpecFingerprint:    artifact.TemplateSpecFingerprint,
	}
	if ShouldInjectEnvdIntoTemplate(req) {
		annotations[constants.CubeAnnotationsInjectEnvd] = constants.CubeAnnotationsInjectEnvdOptIn
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
	// Always hand Cubelets / create-requests the CubeMaster download endpoint,
	// never a direct S3 presigned URL. The endpoint is the one address the
	// deployment guarantees every node can reach; it can then proxy to
	// CubeTemplateCenter / S3 as needed. This avoids K8s multi-node drift where
	// some nodes can dial the object store endpoint and others cannot.
	downloadURL := buildDownloadURL(effectiveArtifactDownloadBaseURL(downloadBaseURL, artifact), artifact.ArtifactID, artifact.DownloadToken)
	imageAnnotations := map[string]string{
		constants.CubeAnnotationRootfsArtifactID:        artifact.ArtifactID,
		constants.CubeAnnotationRootfsArtifactURL:       downloadURL,
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
	out := &types.CreateCubeSandboxReq{
		Request:           &types.Request{RequestID: req.RequestID},
		Volumes:           []*types.Volume{rootVolume},
		Containers:        []*types.Container{container},
		Annotations:       annotations,
		InstanceType:      req.InstanceType,
		NetworkType:       req.NetworkType,
		CubeNetworkConfig: cloneCubeNetworkConfig(req.CubeNetworkConfig),
		Backend:           req.Backend,
	}
	if err := stampCreateRequestBackend(out, req.Backend); err != nil {
		return nil, err
	}
	return out, nil
}

func cloneCubeNetworkConfig(in *types.CubeNetworkConfig) *types.CubeNetworkConfig {
	return in.DeepCopy()
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
	trimmed := strings.TrimRight(NormalizeBaseURL(baseURL), "/")
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

// HostReachableByOtherNodes reports whether a host[:port] value can be dialed
// from another node. Wildcard binds (0.0.0.0, [::], ::) and loopback
// (localhost, 127.0.0.0/8, ::1) are only meaningful on this host, so they must
// never be handed to a Cubelet as a download address.
func HostReachableByOtherNodes(host string) bool {
	host = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	if host[0] == '[' {
		if end := strings.Index(host, "]"); end > 0 {
			host = host[1:end]
		}
	} else if idx := strings.LastIndex(host, ":"); idx > 0 && !strings.Contains(host[idx+1:], "]") {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	switch host {
	case "", "0.0.0.0", "::", "localhost", "localhost.localdomain", "ip6-localhost":
		return false
	}
	if strings.HasPrefix(host, "127.") || host == "::1" {
		return false
	}
	return true
}

// ExternallyUsableBaseURL reports whether raw is a base URL other nodes can
// dial: non-empty, parseable, and not wildcard/loopback.
func ExternallyUsableBaseURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	u, err := url.Parse(NormalizeBaseURL(trimmed))
	if err != nil {
		return false
	}
	return HostReachableByOtherNodes(u.Host)
}

// effectiveArtifactDownloadBaseURL picks the CubeMaster base URL embedded into
// Cubelet-facing download URLs. Every candidate must be externally usable:
// one-click historically ships CUBE_MASTER_ADDR=http://127.0.0.1:8089, and old
// artifact rows may carry a wildcard master_node_ip (http://0.0.0.0:8089) --
// handing either to a remote Cubelet makes every artifact download fail while
// the build job itself reports success. Unusable candidates are skipped with a
// warning so resolution falls through to the next source (or "" , which
// callers already treat as "not ready for distribution").
func effectiveArtifactDownloadBaseURL(fallback string, artifact *models.RootfsArtifact) string {
	if cfg := config.GetConfig(); cfg != nil {
		if addr := strings.TrimSpace(cfg.MasterAddr()); addr != "" {
			if ExternallyUsableBaseURL(addr) {
				return NormalizeBaseURL(addr)
			}
			log.G(context.Background()).Warnf("configured master addr %q is wildcard/loopback; not usable as the cubelet-facing download base URL, falling through", addr)
		}
	}
	if addr := strings.TrimSpace(os.Getenv(config.EnvMasterAddr)); addr != "" {
		if ExternallyUsableBaseURL(addr) {
			return NormalizeBaseURL(addr)
		}
		log.G(context.Background()).Warnf("%s=%q is wildcard/loopback; not usable as the cubelet-facing download base URL, falling through", config.EnvMasterAddr, addr)
	}
	if trimmed := strings.TrimSpace(fallback); trimmed != "" {
		if ExternallyUsableBaseURL(trimmed) {
			return NormalizeBaseURL(trimmed)
		}
		log.G(context.Background()).Warnf("request-derived download base URL %q is wildcard/loopback; falling through to the artifact row", trimmed)
	}
	if artifact != nil {
		if trimmed := strings.TrimSpace(artifact.MasterNodeIP); trimmed != "" {
			if ExternallyUsableBaseURL(trimmed) {
				return NormalizeBaseURL(trimmed)
			}
			log.G(context.Background()).Warnf("artifact %s master_node_ip %q is wildcard/loopback; no usable download base URL", artifact.ArtifactID, trimmed)
		}
	}
	return ""
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
