// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

// guestArtifactIdentity is what a snapshot records about the guest runtime it
// was captured from. It is deliberately path-free: the same identity must be
// resolvable on any node.
type guestArtifactIdentity struct {
	Kernel string
	Image  string
	Agent  string
}

// resolveColdBootGuestArtifacts maps a snapshot's guest runtime identity to
// paths that exist on this node.
//
// A filesystem-only snapshot keeps no memory, so the target node must boot its
// own kernel/image/agent. The origin node's absolute paths (in config.json)
// are never used; they are meaningless here.
func (e *cubeboxInstancePlugin) resolveColdBootGuestArtifacts(
	localTemplate *templatetypes.LocalRunTemplate,
	snapshotSpecDir string,
) (kernelPath, imagePath, agentPath string, err error) {
	identity := guestArtifactIdentityFromTemplate(localTemplate)
	identity.fillMissing(snapshotSpecDir)

	kernelPath, err = e.resolveGuestArtifact(
		templatetypes.CubeComponentCubeKernel,
		identity.Kernel,
		resolveTemplateComponentPath(localTemplate, templatetypes.CubeComponentCubeKernel),
		e.artifactPaths.SharedKernelPath,
	)
	if err != nil {
		return "", "", "", err
	}
	imagePath, err = e.resolveGuestArtifact(
		templatetypes.CubeComponentCubeImage,
		identity.Image,
		resolveTemplateComponentPath(localTemplate, templatetypes.CubeComponentCubeImage),
		filepath.Join(e.config.BasePath, "cube-image", "cube-guest-image-cpu.img"),
	)
	if err != nil {
		return "", "", "", err
	}
	agentPath, err = e.resolveGuestArtifact(
		templatetypes.CubeComponentCubeAgent,
		identity.Agent,
		resolveTemplateComponentPath(localTemplate, templatetypes.CubeComponentCubeAgent),
		filepath.Join(e.config.BasePath, "cube-agent", "cube-agent.ext4"),
	)
	if err != nil {
		return "", "", "", err
	}
	return kernelPath, imagePath, agentPath, nil
}

func (e *cubeboxInstancePlugin) resolveGuestArtifact(
	component templatetypes.CubeComponent,
	version, templatePath, fallbackPath string,
) (string, error) {
	if version == "" {
		return "", fmt.Errorf("%s: snapshot does not record a %s version",
			snapshotmode.ErrFilesystemOnlyGuestRuntime, component)
	}

	// Run templates record the short inventory key ("sha256-<12>") while the
	// artifact's version file keeps the raw digest ("sha256:<hex>"); both
	// normalize to one key, so compare keys instead of literals.
	wantKey := templatetypes.InventoryVersionKey(version)
	if wantKey == "" {
		// Refuse rather than let an artifact without a version file match on
		// an empty key.
		return "", fmt.Errorf("%s: snapshot records an unusable %s version %q",
			snapshotmode.ErrFilesystemOnlyGuestRuntime, component, version)
	}

	baseDir := e.versionedBaseDir
	if baseDir == "" {
		baseDir = templatetypes.DefaultVersionedBaseDir
	}
	candidates := []string{
		templatePath,
		templatetypes.VersionedLocalPath(
			baseDir,
			string(component),
			version,
			templatetypes.DefaultRelativePath(component),
		),
		fallbackPath,
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if !regularFileExists(candidate) {
			continue
		}
		// A path without a matching version file cannot prove it holds the
		// recorded artifact.
		if templatetypes.InventoryVersionKey(componentVersionFromPath(candidate)) == wantKey {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: no local %s artifact with version %q",
		snapshotmode.ErrFilesystemOnlyGuestRuntime, component, version)
}

func guestArtifactIdentityFromTemplate(localTemplate *templatetypes.LocalRunTemplate) guestArtifactIdentity {
	version := func(component templatetypes.CubeComponent) string {
		if localTemplate == nil || localTemplate.Componts == nil {
			return ""
		}
		entry, ok := localTemplate.Componts[string(component)]
		if !ok {
			return ""
		}
		return strings.TrimSpace(entry.Component.Version)
	}
	return guestArtifactIdentity{
		Kernel: version(templatetypes.CubeComponentCubeKernel),
		Image:  version(templatetypes.CubeComponentCubeImage),
		Agent:  version(templatetypes.CubeComponentCubeAgent),
	}
}

// fillMissing reads the snapshot's metadata.json for the fields the recovered
// run template did not carry. The file is written by the shim at capture time
// and holds the same "<dir>/version" strings.
func (i *guestArtifactIdentity) fillMissing(snapshotSpecDir string) {
	if i.Kernel != "" && i.Image != "" && i.Agent != "" {
		return
	}
	metadata, err := readSnapshotMetadata(snapshotSpecDir)
	if err != nil {
		return
	}
	if i.Kernel == "" {
		i.Kernel = strings.TrimSpace(metadata.KernelVersion)
	}
	if i.Image == "" {
		i.Image = strings.TrimSpace(metadata.ImageVersion)
	}
	if i.Agent == "" {
		i.Agent = strings.TrimSpace(metadata.AgentVersion)
	}
}

// coldBootWritableLayer returns the overlay upper directory ("disk/<id>") the
// package's rootfs was captured on. The guest mounts exactly that directory, so
// a cold boot must reuse it: deriving a fresh one points the overlay at an empty
// directory, boots the base image alone and silently loses every captured file.
//
// The recorded layer name is authoritative. AppSnapshotContainerID is only the
// older encoding of the same fact and is wrong once the capturing sandbox was
// itself cold-booted (its guest id is the new sandbox's, the layer is not), so
// it is used only for packages written before the layer name was recorded.
func coldBootWritableLayer(snapshotSpecDir string) (string, error) {
	metadata, err := readSnapshotMetadata(snapshotSpecDir)
	if err != nil {
		return "", fmt.Errorf("%s: read snapshot metadata: %v",
			snapshotmode.ErrFilesystemOnlyGuestRuntime, err)
	}
	layer := strings.TrimSpace(metadata.RootfsWritableLayer)
	if layer == "" {
		containerID := strings.TrimSpace(metadata.AppSnapshotContainerID)
		if containerID == "" {
			return "", fmt.Errorf("%s: snapshot does not record its writable layer",
				snapshotmode.ErrFilesystemOnlyGuestRuntime)
		}
		layer = "disk/" + containerID
	}
	if err := validateWritableLayerSubdir(layer); err != nil {
		return "", fmt.Errorf("%s: recorded writable layer %q: %v",
			snapshotmode.ErrFilesystemOnlyGuestRuntime, layer, err)
	}
	return layer, nil
}

// validateWritableLayerSubdir keeps a package-supplied path inside the disk
// directory it is supposed to name. The value crosses nodes inside the package,
// so it is treated as untrusted input rather than as a trusted constant.
func validateWritableLayerSubdir(layer string) error {
	const prefix = "disk/"
	name, ok := strings.CutPrefix(layer, prefix)
	if !ok {
		return fmt.Errorf("must start with %q", prefix)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("must name a single directory inside %q", prefix)
	}
	return nil
}

// readSnapshotMetadata reads the package metadata the shim writes at capture
// time. A missing or malformed file is reported: callers either treat it as
// "no identity recorded here" and fall back, or refuse the boot.
func readSnapshotMetadata(snapshotSpecDir string) (snapshotMetadata, error) {
	data, err := os.ReadFile(filepath.Join(snapshotSpecDir, "metadata.json"))
	if err != nil {
		return snapshotMetadata{}, err
	}
	var metadata snapshotMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return snapshotMetadata{}, err
	}
	return metadata, nil
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// componentVersionFromPath reads the "<dir>/version" file next to an artifact,
// the same convention the shim uses when it stamps metadata.
func componentVersionFromPath(artifactPath string) string {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(artifactPath), "version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
