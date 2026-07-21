// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func PrepareLocalSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	// Validate the reference up front so both the docker and dockerless branches
	// enforce the same argument-injection guard before the ref is handed to
	// external CLI subprocesses (docker / skopeo).
	if err := ValidateImageRef(spec.ImageRef); err != nil {
		return nil, err
	}
	if nativeRootfsExportEnabled() {
		return prepareNativeSource(ctx, spec)
	}
	// In dockerless mode there is no local docker daemon to hold the image, so a
	// redo re-resolves the source from the registry via skopeo. This intentionally
	// relaxes the docker-path requirement that the image still exist locally.
	if hasDockerlessRootfsExportTools() {
		return prepareDockerlessSource(ctx, spec)
	}
	inspectOutput, err := dockerOutput(ctx, "", "image", "inspect", "--", spec.ImageRef)
	if err != nil {
		return nil, fmt.Errorf("redo requires source image %s to still exist locally: %w", spec.ImageRef, err)
	}
	var inspectList []dockerInspectImage
	if err := json.Unmarshal(inspectOutput, &inspectList); err != nil {
		return nil, fmt.Errorf("unmarshal local docker inspect output: %w", err)
	}
	if len(inspectList) == 0 {
		return nil, fmt.Errorf("docker image inspect returned empty result for %s", spec.ImageRef)
	}
	inspectInfo := inspectList[0]
	configJSON, err := json.Marshal(inspectInfo.Config)
	if err != nil {
		return nil, fmt.Errorf("marshal image Config: %w", err)
	}
	return &PreparedSource{
		LocalRef:       spec.ImageRef,
		Digest:         firstNonEmptyDigest(inspectInfo),
		Config:         inspectInfo.Config,
		ConfigJSON:     string(configJSON),
		MasterNodeIP:   NormalizeBaseURL(spec.DownloadBaseURL),
		OnPullProgress: spec.OnPullProgress,
	}, nil
}

func PrepareSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	if nativeRootfsExportEnabled() {
		return prepareNativeSource(ctx, spec)
	}
	if hasDockerlessRootfsExportTools() {
		return prepareDockerlessSource(ctx, spec)
	}
	return prepareDockerSource(ctx, spec)
}

func prepareNativeSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	if err := ValidateImageRef(spec.ImageRef); err != nil {
		return nil, err
	}

	rawRef := strings.TrimPrefix(spec.ImageRef, "docker://")
	ref, err := name.ParseReference(rawRef)
	if err != nil {
		return nil, fmt.Errorf("native export failed to parse image ref %q: %w", rawRef, err)
	}

	var authCfg *RegistryAuthConfig
	if spec.RegistryUsername != "" || spec.RegistryPassword != "" {
		authCfg = &RegistryAuthConfig{Username: spec.RegistryUsername, Password: spec.RegistryPassword}
	}
	authOpt := registryAuthOption(authCfg)
	jobs := nativeExportConcurrency()
	platOpt := remote.WithPlatform(defaultPlatform())
	jobsOpt := remote.WithJobs(jobs)

	img, err := remote.Image(ref, remote.WithContext(ctx), authOpt, platOpt, jobsOpt)
	if err != nil {
		return nil, fmt.Errorf("native export failed to resolve image %q (if this is a multi-arch index, verify the requested platform exists): %w", rawRef, err)
	}

	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("failed to get config file for %q: %w", rawRef, err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest for %q: %w", rawRef, err)
	}

	var compressedSize int64
	for _, layer := range manifest.Layers {
		compressedSize += layer.Size
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("failed to get digest for %q: %w", rawRef, err)
	}

	// Make sure the digest aligns with the dockerless/skopeo canonical format
	// (name@sha256:...) to preserve fingerprint cache hits.
	unifiedDigest := imageDigestFromReference(ref, digest.String())

	cfg := convertV1Config(cfgFile.Config)
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal image Config: %w", err)
	}

	return &PreparedSource{
		LocalRef:            spec.ImageRef,
		Digest:              unifiedDigest,
		Config:              cfg,
		ConfigJSON:          string(configJSON),
		MasterNodeIP:        NormalizeBaseURL(spec.DownloadBaseURL),
		ExportMode:          ExportModeNative,
		CompressedSizeBytes: compressedSize,
		RegistryAuth:        authCfg,
		nativeImage:         img,
		Cleanup:             func(context.Context) {}, // no-op for native
		OnPullProgress:      spec.OnPullProgress,
	}, nil
}

// convertV1Config extracts only the fields necessary for container execution.
// Fields like ExposedPorts, Volumes, Labels, StopSignal, and Healthcheck
// are intentionally omitted as they are not relevant to Cube's runtime
// configuration or rootfs extraction logic.
func convertV1Config(cfg v1.Config) DockerImageConfig {
	return DockerImageConfig{
		Entrypoint: cfg.Entrypoint,
		Cmd:        cfg.Cmd,
		Env:        cfg.Env,
		WorkingDir: cfg.WorkingDir,
		User:       cfg.User,
	}
}

func prepareDockerlessSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	if err := ValidateImageRef(spec.ImageRef); err != nil {
		return nil, err
	}
	authFile, cleanup, err := createSkopeoAuthFile(spec.ImageRef, spec.RegistryUsername, spec.RegistryPassword)
	if err != nil {
		return nil, err
	}
	sourceRef := skopeoDockerImageRef(spec.ImageRef)
	inspectOutput, err := skopeoOutput(ctx, authFile, "inspect", sourceRef)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("skopeo inspect %s failed: %w", spec.ImageRef, err)
	}
	configOutput, err := skopeoOutput(ctx, authFile, "inspect", "--config", sourceRef)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("skopeo inspect --config %s failed: %w", spec.ImageRef, err)
	}

	var inspectInfo skopeoInspectImage
	if err := json.Unmarshal(inspectOutput, &inspectInfo); err != nil {
		cleanup()
		return nil, fmt.Errorf("unmarshal skopeo inspect output: %w", err)
	}
	var configInfo skopeoInspectConfig
	if err := json.Unmarshal(configOutput, &configInfo); err != nil {
		cleanup()
		return nil, fmt.Errorf("unmarshal skopeo inspect config output: %w", err)
	}
	configJSON, err := json.Marshal(configInfo.Config)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("marshal image Config: %w", err)
	}
	return &PreparedSource{
		LocalRef:            spec.ImageRef,
		Digest:              skopeoImageDigest(inspectInfo, spec.ImageRef),
		Config:              configInfo.Config,
		ConfigJSON:          string(configJSON),
		MasterNodeIP:        NormalizeBaseURL(spec.DownloadBaseURL),
		ExportMode:          ExportModeDockerless,
		SkopeoAuthFile:      authFile,
		CompressedSizeBytes: skopeoLayersTotalSize(inspectInfo),
		OnPullProgress:      spec.OnPullProgress,
		Cleanup: func(context.Context) {
			cleanup()
		},
	}, nil
}

func prepareDockerSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	// Validate the reference up front so the docker prepare path enforces the
	// same argument-injection guard as the dockerless path before the ref is
	// handed to docker subprocesses.
	if err := ValidateImageRef(spec.ImageRef); err != nil {
		return nil, err
	}
	if source, err := prepareDockerSourceWithEngine(ctx, spec); err == nil {
		return source, nil
	}
	return prepareDockerSourceWithCLI(ctx, spec)
}

func prepareDockerSourceWithEngine(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	cli, err := newEngineClient()
	if err != nil {
		return nil, err
	}
	inspectInfo, err := engineImageInspectWithClient(ctx, cli, spec.ImageRef)
	imageExistsLocally := err == nil
	if err != nil && !errors.Is(err, errEngineImageNotFound) {
		return nil, err
	}
	if !imageExistsLocally {
		if err := engineImagePullWithClient(ctx, cli, spec); err != nil {
			return nil, err
		}
		inspectInfo, err = engineImageInspectWithClient(ctx, cli, spec.ImageRef)
		if err != nil {
			return nil, fmt.Errorf("engine image inspect %s failed after pull: %w", spec.ImageRef, err)
		}
	}
	return dockerInspectToPreparedSource(spec, *inspectInfo, imageExistsLocally, "")
}

func prepareDockerSourceWithCLI(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	var (
		dockerConfigDir       string
		removeDockerConfigDir bool
		imageExistsLocally    bool
		inspectOutput         []byte
		err                   error
	)
	defer func() {
		if removeDockerConfigDir && dockerConfigDir != "" {
			_ = os.RemoveAll(dockerConfigDir)
		}
	}()
	inspectOutput, err = dockerOutput(ctx, "", "image", "inspect", "--", spec.ImageRef)
	if err == nil {
		imageExistsLocally = true
	}
	if spec.RegistryUsername != "" || spec.RegistryPassword != "" {
		tmpDir, err := os.MkdirTemp("", "cubemaster-docker-config-*")
		if err != nil {
			return nil, err
		}
		dockerConfigDir = tmpDir
		removeDockerConfigDir = true
		if err := dockerLogin(ctx, dockerConfigDir, spec.ImageRef, spec.RegistryUsername, spec.RegistryPassword); err != nil {
			return nil, err
		}
	}
	if !imageExistsLocally {
		if err := dockerPull(ctx, dockerConfigDir, spec.ImageRef, spec.OnPullProgress); err != nil {
			return nil, fmt.Errorf("docker pull %s failed: %w", spec.ImageRef, err)
		}
		inspectOutput, err = dockerOutput(ctx, dockerConfigDir, "image", "inspect", "--", spec.ImageRef)
		if err != nil {
			return nil, fmt.Errorf("docker image inspect %s failed: %w", spec.ImageRef, err)
		}
	}
	var inspectList []dockerInspectImage
	if err := json.Unmarshal(inspectOutput, &inspectList); err != nil {
		return nil, fmt.Errorf("unmarshal docker inspect output: %w", err)
	}
	if len(inspectList) == 0 {
		return nil, fmt.Errorf("docker image inspect returned empty result for %s", spec.ImageRef)
	}
	source, err := dockerInspectToPreparedSource(spec, inspectList[0], imageExistsLocally, dockerConfigDir)
	if err != nil {
		return nil, err
	}
	removeDockerConfigDir = false
	return source, nil
}

func dockerInspectToPreparedSource(spec SourceSpec, inspectInfo dockerInspectImage, imageExistsLocally bool, dockerConfigDir string) (*PreparedSource, error) {
	configJSON, err := json.Marshal(inspectInfo.Config)
	if err != nil {
		return nil, fmt.Errorf("marshal image Config: %w", err)
	}
	source := &PreparedSource{
		LocalRef:       spec.ImageRef,
		Digest:         firstNonEmptyDigest(inspectInfo),
		Config:         inspectInfo.Config,
		ConfigJSON:     string(configJSON),
		MasterNodeIP:   NormalizeBaseURL(spec.DownloadBaseURL),
		OnPullProgress: spec.OnPullProgress,
		Cleanup: func(cleanupCtx context.Context) {
			if dockerConfigDir != "" {
				_ = os.RemoveAll(dockerConfigDir)
			}
			if !imageExistsLocally {
				_ = dockerRun(cleanupCtx, "", "image", "rm", "-f", "--", spec.ImageRef)
			}
		},
	}
	return source, nil
}

func createSkopeoAuthFile(imageRef, username, password string) (string, func(), error) {
	if username == "" && password == "" {
		return "", func() {}, nil
	}
	tmpDir, err := os.MkdirTemp("", "cubemaster-skopeo-auth-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}
	authPayload := map[string]any{
		"auths": map[string]any{
			registryHostFromImageRef(imageRef): map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
			},
		},
	}
	payload, err := json.Marshal(authPayload)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	authFile := filepath.Join(tmpDir, "auth.json")
	if err := os.WriteFile(authFile, payload, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return authFile, cleanup, nil
}

func skopeoImageDigest(info skopeoInspectImage, imageRef string) string {
	if info.Digest == "" {
		return ""
	}
	name := info.Name
	if name == "" {
		name = imageNameWithoutTagDigest(imageRef)
	}
	if name == "" {
		return info.Digest
	}
	return name + "@" + info.Digest
}

// imageDigestFromReference unifies native digest formatting with skopeo's
// canonical dockerless form so equivalent refs share cache keys.
func imageDigestFromReference(ref name.Reference, hexDigest string) string {
	if hexDigest == "" {
		return ""
	}
	namePart := dockerlessCanonicalName(ref.Context().Name())
	if namePart == "" {
		return hexDigest
	}
	return namePart + "@" + hexDigest
}

func dockerlessCanonicalName(namePart string) string {
	if strings.HasPrefix(namePart, "index.docker.io/") {
		return "docker.io/" + strings.TrimPrefix(namePart, "index.docker.io/")
	}
	return namePart
}

func firstNonEmptyDigest(info dockerInspectImage) string {
	if len(info.RepoDigests) > 0 && info.RepoDigests[0] != "" {
		rd := info.RepoDigests[0]
		// RepoDigests entries are canonical references of the form
		// "name@sha256:...". We only want the digest portion so that
		// callers can compose "ref@digest" without producing
		// "name:tag@name@sha256:..." style duplication.
		if at := strings.Index(rd, "@"); at >= 0 && at+1 < len(rd) {
			return rd[at+1:]
		}
		return rd
	}
	return info.ID
}
