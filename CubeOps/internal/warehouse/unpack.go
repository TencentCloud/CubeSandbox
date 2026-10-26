// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExtractedComponent is one inventory tree ready to install.
type ExtractedComponent struct {
	Component string
	Version   string
	Dir       string
}

// ReleaseManifest is the outer one-click release-manifest.json.
type ReleaseManifest struct {
	Components map[string]manifestComponent `json:"components"`
	GuestImage manifestGuestImage           `json:"guest_image"`
}

type manifestComponent struct {
	Version string `json:"version"`
}

type manifestGuestImage struct {
	Version      string `json:"version"`
	AgentVersion string `json:"agent_version"`
}

// UnpackOneClick extracts a one-click tar.gz into destRoot and returns the
// inventory trees (shim/image/agent plus one kernel dir per present variant).
// destRoot is a scratch directory owned by the caller.
func UnpackOneClick(archivePath, destRoot string) ([]ExtractedComponent, error) {
	outer := filepath.Join(destRoot, "outer")
	if err := os.MkdirAll(outer, dirPerm); err != nil {
		return nil, err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	err = ExtractTarGz(f, outer)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("extract outer package: %w", err)
	}

	outerRoot, err := findDirContaining(outer, "release-manifest.json")
	if err != nil {
		return nil, err
	}
	manifest, err := readReleaseManifest(filepath.Join(outerRoot, "release-manifest.json"))
	if err != nil {
		return nil, err
	}

	innerArchive := filepath.Join(outerRoot, "assets", "package", "sandbox-package.tar.gz")
	if !fileExists(innerArchive) {
		return nil, fmt.Errorf("missing assets/package/sandbox-package.tar.gz")
	}
	inner := filepath.Join(destRoot, "inner")
	if err := os.MkdirAll(inner, dirPerm); err != nil {
		return nil, err
	}
	in, err := os.Open(innerArchive)
	if err != nil {
		return nil, err
	}
	err = ExtractTarGz(in, inner)
	_ = in.Close()
	if err != nil {
		return nil, fmt.Errorf("extract sandbox-package: %w", err)
	}

	pkgRoot, err := findPackageRoot(inner)
	if err != nil {
		return nil, err
	}

	var out []ExtractedComponent
	staging := filepath.Join(destRoot, "staging")
	if err := os.MkdirAll(staging, dirPerm); err != nil {
		return nil, err
	}

	for _, name := range []string{ComponentShim, ComponentImage, ComponentAgent} {
		src := filepath.Join(pkgRoot, name)
		if st, err := os.Stat(src); err != nil || !st.IsDir() {
			continue
		}
		ver, err := resolveComponentVersion(src, name, manifest)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(staging, name+"-"+ver)
		if err := relocateTree(src, dst); err != nil {
			return nil, fmt.Errorf("copy %s: %w", name, err)
		}
		if err := ValidateInstalledTree(dst, name); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, ExtractedComponent{Component: name, Version: ver, Dir: dst})
	}

	kernelSrc := filepath.Join(pkgRoot, ComponentKernel)
	if st, err := os.Stat(kernelSrc); err == nil && st.IsDir() {
		kernels, err := inventoryKernelVariants(kernelSrc, staging)
		if err != nil {
			return nil, err
		}
		out = append(out, kernels...)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("one-click package contained no inventory components")
	}
	return out, nil
}

func findDirContaining(root, filename string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == filename {
			found = filepath.Dir(path)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("missing %s in archive", filename)
	}
	return found, nil
}

func findPackageRoot(inner string) (string, error) {
	if isPackageRoot(inner) {
		return inner, nil
	}
	nested := filepath.Join(inner, "sandbox-package")
	if isPackageRoot(nested) {
		return nested, nil
	}
	var found string
	_ = filepath.WalkDir(inner, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if isPackageRoot(path) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("sandbox-package has no cube-shim/cube-image/cube-agent/cube-kernel-scf")
	}
	return found, nil
}

func isPackageRoot(dir string) bool {
	for _, name := range []string{ComponentShim, ComponentImage, ComponentAgent, ComponentKernel} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func readReleaseManifest(path string) (*ReleaseManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read release-manifest.json: %w", err)
	}
	var m ReleaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse release-manifest.json: %w", err)
	}
	return &m, nil
}

func resolveComponentVersion(src, name string, manifest *ReleaseManifest) (string, error) {
	ver := readVersionFile(filepath.Join(src, "version"))
	if ver == "" && manifest != nil {
		switch name {
		case ComponentShim:
			ver = manifestComponentVersion(manifest, "containerd-shim-cube-rs")
			if ver == "" {
				ver = manifestComponentVersion(manifest, "cube-runtime")
			}
		case ComponentImage:
			ver = strings.TrimSpace(manifest.GuestImage.Version)
		case ComponentAgent:
			ver = manifestComponentVersion(manifest, "cube-agent")
			if ver == "" || ver == "unknown" {
				ver = strings.TrimSpace(manifest.GuestImage.AgentVersion)
			}
		}
	}
	ver = strings.TrimSpace(ver)
	if ver == "" || strings.EqualFold(ver, "unknown") {
		return "", fmt.Errorf("cannot resolve version for %s", name)
	}
	return NormalizeVersion(ver)
}

func manifestComponentVersion(m *ReleaseManifest, key string) string {
	if m == nil || m.Components == nil {
		return ""
	}
	return strings.TrimSpace(m.Components[key].Version)
}

func readVersionFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func inventoryKernelVariants(src, staging string) ([]ExtractedComponent, error) {
	var out []ExtractedComponent
	for _, variant := range []string{"bm", "pvm"} {
		file := filepath.Join(src, "vmlinux-"+variant)
		if !fileExists(file) {
			continue
		}
		digest, err := fileSHA256(file)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", file, err)
		}
		if len(digest) < 12 {
			return nil, fmt.Errorf("short sha256 for %s", file)
		}
		ver := "sha256-" + digest[:12]
		dst := filepath.Join(staging, ComponentKernel+"-"+variant+"-"+ver)
		if err := os.MkdirAll(dst, dirPerm); err != nil {
			return nil, err
		}
		if err := copyRegularFile(file, filepath.Join(dst, "vmlinux-"+variant)); err != nil {
			return nil, err
		}
		if err := os.Symlink("vmlinux-"+variant, filepath.Join(dst, "vmlinux")); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dst, "variant"), []byte(variant+"\n"), filePerm); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dst, "version"), []byte("sha256:"+digest+"\n"), filePerm); err != nil {
			return nil, err
		}
		if err := ValidateInstalledTree(dst, ComponentKernel); err != nil {
			return nil, err
		}
		out = append(out, ExtractedComponent{Component: ComponentKernel, Version: ver, Dir: dst})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cube-kernel-scf has neither vmlinux-bm nor vmlinux-pvm")
	}
	return out, nil
}

// ContentShortHash returns sha256-<12> of a file (kernel inventory key).
func ContentShortHash(path string) (string, error) {
	sum, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if len(sum) < 12 {
		return "", fmt.Errorf("short sha256")
	}
	return "sha256-" + sum[:12], nil
}
