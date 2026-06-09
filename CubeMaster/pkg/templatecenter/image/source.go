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
	"regexp"
	"strings"
)

func PrepareLocalSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
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
		LocalRef:     spec.ImageRef,
		Digest:       firstNonEmptyDigest(inspectInfo),
		Config:       inspectInfo.Config,
		ConfigJSON:   string(configJSON),
		MasterNodeIP: NormalizeBaseURL(spec.DownloadBaseURL),
	}, nil
}

func PrepareSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	if hasDockerlessRootfsExportTools() {
		return prepareDockerlessSource(ctx, spec)
	}
	return prepareDockerSource(ctx, spec)
}

func prepareDockerlessSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
	if err := validateImageRef(spec.ImageRef); err != nil {
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
		UseDockerless:       true,
		SkopeoAuthFile:      authFile,
		CompressedSizeBytes: skopeoLayersTotalSize(inspectInfo),
		Cleanup: func(context.Context) {
			cleanup()
		},
	}, nil
}

func prepareDockerSource(ctx context.Context, spec SourceSpec) (*PreparedSource, error) {
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
		if err := dockerRun(ctx, dockerConfigDir, "pull", "--", spec.ImageRef); err != nil {
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
	inspectInfo := inspectList[0]
	configJSON, err := json.Marshal(inspectInfo.Config)
	if err != nil {
		return nil, fmt.Errorf("marshal image Config: %w", err)
	}
	source := &PreparedSource{
		LocalRef:     spec.ImageRef,
		Digest:       firstNonEmptyDigest(inspectInfo),
		Config:       inspectInfo.Config,
		ConfigJSON:   string(configJSON),
		MasterNodeIP: NormalizeBaseURL(spec.DownloadBaseURL),
		Cleanup: func(cleanupCtx context.Context) {
			if dockerConfigDir != "" {
				_ = os.RemoveAll(dockerConfigDir)
			}
			if !imageExistsLocally {
				_ = dockerRun(cleanupCtx, "", "image", "rm", "-f", "--", spec.ImageRef)
			}
		},
	}
	removeDockerConfigDir = false
	return source, nil
}

// imageRefAllowedPattern is the strict character whitelist for image
// references. It permits exactly the characters that appear in legitimate
// registry/repository[:tag][@algo:hexdigest] references: alphanumerics and
// `.`, `-`, `_`, `/`, `:`, `@`. Any other character (notably whitespace) is
// rejected so the reference cannot be split into additional argv entries.
var imageRefAllowedPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

// validateImageRef guards against argument injection (CWE-88) when the image
// reference is later passed as a positional argument to external CLIs
// (skopeo/umoci/docker). Those tools accept flags interspersed with positional
// arguments, so a ref such as `registry.example.com/image --authfile /etc/shadow`
// would otherwise smuggle extra flags into the subprocess. To prevent this we
// enforce a strict character whitelist (which excludes whitespace and other
// argument delimiters) and reject any ref that begins with a dash.
func validateImageRef(imageRef string) error {
	trimmed := strings.TrimPrefix(imageRef, "docker://")
	if trimmed == "" {
		return errors.New("empty image reference")
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("invalid image reference: %s", imageRef)
	}
	if !imageRefAllowedPattern.MatchString(trimmed) {
		return fmt.Errorf("invalid image reference: %s", imageRef)
	}
	return nil
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
