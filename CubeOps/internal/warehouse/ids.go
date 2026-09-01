// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	ComponentShim   = "cube-shim"
	ComponentKernel = "cube-kernel-scf"
	ComponentImage  = "cube-image"
	ComponentAgent  = "cube-agent"

	CodeNotFound        = "warehouse_not_found"
	CodeInvalidRequest  = "warehouse_invalid_request"
	CodeUnauthorizedJob = "warehouse_node_mismatch"
	CodeDisabled        = "warehouse_disabled"
)

var versionKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

func NormalizeArch(arch string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case ArchAMD64, "x86_64":
		return ArchAMD64, nil
	case ArchARM64, "aarch64":
		return ArchARM64, nil
	default:
		return "", fmt.Errorf("unsupported arch %q (want amd64 or arm64)", arch)
	}
}

// ComponentInfo is one entry in the closed warehouse catalog.
type ComponentInfo struct {
	Name string
}

// Catalog returns the four inventory components in display order.
func Catalog() []ComponentInfo {
	return []ComponentInfo{
		{Name: ComponentShim},
		{Name: ComponentImage},
		{Name: ComponentAgent},
		{Name: ComponentKernel},
	}
}

func KnownComponent(name string) bool {
	_, err := NormalizeComponent(name)
	return err == nil
}

func NormalizeComponent(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch name {
	case ComponentShim, ComponentKernel, ComponentImage, ComponentAgent:
		return name, nil
	default:
		return "", fmt.Errorf("unsupported component %q", name)
	}
}

func NormalizeVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || version == "." || version == "/" {
		return "", fmt.Errorf("empty version")
	}
	if strings.ContainsAny(version, `/\`) || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid version %q", version)
	}
	version = path.Base(version)
	if version == "." || version == "/" || version == "" {
		return "", fmt.Errorf("empty version")
	}
	if !versionKeyRe.MatchString(version) {
		return "", fmt.Errorf("invalid version %q", version)
	}
	return version, nil
}

const (
	Prefix        = "warehouse/"
	uploadsPrefix = Prefix + "uploads/"
	blobsPrefix   = Prefix + "blobs/"
)

func ObjectKey(arch, component, version string) string {
	return path.Join("warehouse", "blobs", arch, component, version, "component.tar.gz")
}

func UploadObjectKey(uploadID string) string {
	return uploadsPrefix + uploadID + ".tar.gz"
}
