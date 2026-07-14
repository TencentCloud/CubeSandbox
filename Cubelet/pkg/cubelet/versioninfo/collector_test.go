// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package versioninfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func versionOf(t *testing.T, list []ComponentVersion, component string) (ComponentVersion, bool) {
	t.Helper()
	for _, v := range list {
		if v.Component == component {
			return v, true
		}
	}
	return ComponentVersion{}, false
}

func writeManifest(t *testing.T, dir string) {
	t.Helper()
	manifest := `{
  "release_version": "v0.5.0",
  "components": {
    "cubemaster": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cubemastercli": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cube-api": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cubelet": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cubecli": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "network-agent": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "containerd-shim-cube-rs": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cube-runtime": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cube-egress": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cube-agent": {"version": "v0.5.0", "commit": "abc", "build_time": "t"}
  },
  "guest_image": {"version": "cube-image/2026.01", "agent_version": "agent-1.2.3"},
  "kernel": {
    "version": "5.10.0-100",
    "pvm_version": "6.6.69-1.2.cubesandbox",
    "vmlinux_digest_sha256": "sha256:ordinary",
    "vmlinux_pvm_digest_sha256": "sha256:pvm"
  }
}`
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func mkComponentDir(t *testing.T, base, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, name, "v0.5.0"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
}

func mkComponentFile(t *testing.T, base string, rel ...string) {
	t.Helper()
	path := filepath.Join(append([]string{base}, rel...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkKernelLayout(t *testing.T, base, active string) {
	t.Helper()
	kernelDir := filepath.Join(base, "cube-kernel-scf")
	if err := os.MkdirAll(kernelDir, 0o755); err != nil {
		t.Fatalf("mkdir kernel dir: %v", err)
	}
	for _, name := range []string{"vmlinux-bm", "vmlinux-pvm"} {
		if err := os.WriteFile(filepath.Join(kernelDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	link := filepath.Join(kernelDir, "vmlinux")
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove kernel symlink: %v", err)
	}
	if err := os.Symlink(active, link); err != nil {
		t.Fatalf("symlink active kernel: %v", err)
	}
}

func TestCollectFiltersUninstalledComponents(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	// Only the compute-node components are actually installed.
	mkComponentDir(t, dir, "containerd-shim-cube-rs")
	mkComponentDir(t, dir, "cube-runtime")
	// Deliberately do NOT create cubemaster/cube-api dirs.

	c := NewCollector(dir)
	got := c.Collect()

	if _, ok := versionOf(t, got, "cubemaster"); ok {
		t.Errorf("cubemaster should be filtered out on a node without it installed")
	}
	if _, ok := versionOf(t, got, "cube-api"); ok {
		t.Errorf("cube-api should be filtered out on a node without it installed")
	}
	if _, ok := versionOf(t, got, "containerd-shim-cube-rs"); !ok {
		t.Errorf("installed containerd-shim-cube-rs should be reported")
	}
	if _, ok := versionOf(t, got, "cube-runtime"); !ok {
		t.Errorf("installed cube-runtime should be reported")
	}
}

func TestCollectRecognizesOneClickInstallLayout(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)

	mkComponentFile(t, dir, "CubeMaster", "bin", "cubemaster")
	mkComponentFile(t, dir, "CubeMaster", "bin", "cubemastercli")
	mkComponentFile(t, dir, "CubeAPI", "bin", "cube-api")
	mkComponentFile(t, dir, "Cubelet", "bin", "cubecli")
	mkComponentFile(t, dir, "network-agent", "bin", "network-agent")
	mkComponentFile(t, dir, "cube-shim", "bin", "containerd-shim-cube-rs")
	mkComponentFile(t, dir, "cube-shim", "bin", "cube-runtime")
	mkComponentFile(t, dir, "cube-egress", "version")

	c := NewCollector(dir)
	got := c.Collect()

	for _, name := range []string{
		"cubemaster",
		"cubemastercli",
		"cube-api",
		"cubecli",
		"network-agent",
		"containerd-shim-cube-rs",
		"cube-runtime",
	} {
		if _, ok := versionOf(t, got, name); !ok {
			t.Errorf("%s should be reported when the one-click install path exists", name)
		}
	}
	// cube-egress is reported from the host-side version marker file (Source=File),
	// not from the manifest loop, so check it separately.
	if v, ok := versionOf(t, got, "cube-egress"); !ok {
		t.Errorf("cube-egress should be reported when the version marker exists")
	} else if v.Source != SourceFile {
		t.Errorf("cube-egress Source should be file, got %s", v.Source)
	}
}

func TestCollectCubeletFromBinaryAndSpecialComponents(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	imgDir := filepath.Join(dir, "cube-image")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "version"), []byte("effective-image-2026.02\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(dir)
	got := c.Collect()

	cubelet, ok := versionOf(t, got, ComponentCubelet)
	if !ok || cubelet.Source != SourceBinary {
		t.Errorf("cubelet must come from binary, got %+v ok=%v", cubelet, ok)
	}

	agent, ok := versionOf(t, got, ComponentCubeAgent)
	if !ok || agent.Version != "agent-1.2.3" {
		t.Errorf("cube-agent must come from guest_image.agent_version, got %+v ok=%v", agent, ok)
	}

	kernel, ok := versionOf(t, got, ComponentKernel)
	if !ok || kernel.Version != "5.10.0-100@sha256:ordinary" {
		t.Errorf("kernel must come from kernel identity, got %+v ok=%v", kernel, ok)
	}

	img, ok := versionOf(t, got, ComponentGuestImage)
	if !ok || img.Version != "effective-image-2026.02" || img.Source != SourceFile {
		t.Errorf("guest-image must come from on-node version file, got %+v ok=%v", img, ok)
	}

	// cube-agent must not be duplicated from components{} map.
	count := 0
	for _, v := range got {
		if v.Component == ComponentCubeAgent {
			count++
		}
	}
	if count != 1 {
		t.Errorf("cube-agent should appear exactly once, got %d", count)
	}
}

func TestCollectDegradesWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	// No manifest, but a guest-image version file exists.
	imgDir := filepath.Join(dir, "cube-image")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "version"), []byte("img-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(dir)
	got := c.Collect()

	if _, ok := versionOf(t, got, ComponentCubelet); !ok {
		t.Errorf("cubelet self version must still be reported without a manifest")
	}
	if img, ok := versionOf(t, got, ComponentGuestImage); !ok || img.Version != "img-only" {
		t.Errorf("guest-image file should still be reported without a manifest, got %+v ok=%v", img, ok)
	}
	if _, ok := versionOf(t, got, ComponentKernel); ok {
		t.Errorf("kernel must not be reported without a manifest")
	}
}

func TestCollectKernelFromActiveOrdinarySymlink(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	mkKernelLayout(t, dir, "vmlinux-bm")

	c := NewCollector(dir)
	got := c.Collect()

	kernel, ok := versionOf(t, got, ComponentKernel)
	if !ok || kernel.Version != "5.10.0-100@sha256:ordinary" || kernel.Source != SourceFile {
		t.Fatalf("kernel must follow ordinary active symlink, got %+v ok=%v", kernel, ok)
	}
}

func TestCollectKernelFromActivePVMSymlink(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	mkKernelLayout(t, dir, "vmlinux-pvm")

	c := NewCollector(dir)
	got := c.Collect()

	kernel, ok := versionOf(t, got, ComponentKernel)
	if !ok || kernel.Version != "6.6.69-1.2.cubesandbox@sha256:pvm" || kernel.Source != SourceFile {
		t.Fatalf("kernel must follow PVM active symlink, got %+v ok=%v", kernel, ok)
	}
}

func TestCollectKernelIdentityFallsBackToDigestWhenTagUnknown(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
  "release_version": "v0.5.0",
  "components": {},
  "guest_image": {},
  "kernel": {
    "version": "unknown",
    "pvm_version": "unknown",
    "vmlinux_digest_sha256": "sha256:ordinary",
    "vmlinux_pvm_digest_sha256": "sha256:pvm"
  }
}`
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	mkKernelLayout(t, dir, "vmlinux-pvm")

	c := NewCollector(dir)
	got := c.Collect()

	kernel, ok := versionOf(t, got, ComponentKernel)
	if !ok || kernel.Version != "sha256:pvm" || kernel.Source != SourceFile {
		t.Fatalf("kernel must use digest when tag is unknown, got %+v ok=%v", kernel, ok)
	}
}

func TestCollectKernelIdentityFallbackForLegacyNonSymlink(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	kernelDir := filepath.Join(dir, "cube-kernel-scf")
	if err := os.MkdirAll(kernelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kernelDir, "vmlinux"), []byte("legacy ordinary kernel"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(dir)
	got := c.Collect()

	kernel, ok := versionOf(t, got, ComponentKernel)
	if !ok || kernel.Version != "5.10.0-100@sha256:ordinary" || kernel.Source != SourceManifest {
		t.Fatalf("legacy non-symlink kernel must fall back to ordinary identity, got %+v ok=%v", kernel, ok)
	}
}

func TestKernelArtifactIdentityFormatting(t *testing.T) {
	tests := []struct {
		name   string
		tag    string
		digest string
		want   string
	}{
		{name: "tag and digest", tag: "kernel-v1", digest: "sha256:abc", want: "kernel-v1@sha256:abc"},
		{name: "unknown tag uses digest", tag: "unknown", digest: "sha256:abc", want: "sha256:abc"},
		{name: "empty tag uses digest", tag: "", digest: "sha256:abc", want: "sha256:abc"},
		{name: "missing digest uses tag", tag: "kernel-v1", digest: "", want: "kernel-v1"},
		{name: "all missing", tag: "unknown", digest: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kernelArtifactIdentity(tt.tag, tt.digest); got != tt.want {
				t.Fatalf("kernelArtifactIdentity(%q, %q)=%q, want %q", tt.tag, tt.digest, got, tt.want)
			}
		})
	}
}

func TestKernelSymlinkReread(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	mkKernelLayout(t, dir, "vmlinux-bm")

	c := NewCollector(dir)
	if kernel, _ := versionOf(t, c.Collect(), ComponentKernel); kernel.Version != "5.10.0-100@sha256:ordinary" {
		t.Fatalf("expected ordinary kernel, got %q", kernel.Version)
	}

	time.Sleep(10 * time.Millisecond)
	mkKernelLayout(t, dir, "vmlinux-pvm")
	if kernel, _ := versionOf(t, c.Collect(), ComponentKernel); kernel.Version != "6.6.69-1.2.cubesandbox@sha256:pvm" {
		t.Fatalf("expected PVM kernel after symlink switch, got %q", kernel.Version)
	}
}

func TestGuestImageMTimeReread(t *testing.T) {
	dir := t.TempDir()
	imgDir := filepath.Join(dir, "cube-image")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verFile := filepath.Join(imgDir, "version")
	if err := os.WriteFile(verFile, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(dir)
	if img, _ := versionOf(t, c.Collect(), ComponentGuestImage); img.Version != "v1" {
		t.Fatalf("expected v1, got %q", img.Version)
	}

	// Rewrite with a newer mtime; the collector must pick up the new version.
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(verFile, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(verFile, future, future); err != nil {
		t.Fatal(err)
	}
	if img, _ := versionOf(t, c.Collect(), ComponentGuestImage); img.Version != "v2" {
		t.Errorf("expected v2 after mtime change, got %q", img.Version)
	}
}

func TestCollectCubeEgressFromVersionMarker(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	// Create the version marker file that the deploy system writes.
	markerDir := filepath.Join(dir, "cube-egress")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "version"), []byte("v0.5.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(dir)
	got := c.Collect()

	egress, ok := versionOf(t, got, ComponentCubeEgress)
	if !ok {
		t.Fatalf("cube-egress must be reported when version marker exists")
	}
	if egress.Version != "v0.5.0" {
		t.Errorf("cube-egress version should be v0.5.0, got %q", egress.Version)
	}
	if egress.Source != SourceFile {
		t.Errorf("cube-egress Source should be file, got %s", egress.Source)
	}

	// cube-egress must not appear twice (manifest skip verification).
	count := 0
	for _, v := range got {
		if v.Component == ComponentCubeEgress {
			count++
		}
	}
	if count != 1 {
		t.Errorf("cube-egress should appear exactly once, got %d", count)
	}
}

func TestCollectCubeEgressDegradesWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir)
	// Deliberately do NOT create cube-egress/version.

	c := NewCollector(dir)
	got := c.Collect()

	if _, ok := versionOf(t, got, ComponentCubeEgress); ok {
		t.Errorf("cube-egress must NOT be reported when version marker is absent")
	}
}
