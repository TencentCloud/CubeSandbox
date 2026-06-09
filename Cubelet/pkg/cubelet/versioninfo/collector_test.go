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
    "cube-api": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cubelet": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "containerd-shim-cube-rs": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cube-runtime": {"version": "v0.5.0", "commit": "abc", "build_time": "t"},
    "cube-agent": {"version": "v0.5.0", "commit": "abc", "build_time": "t"}
  },
  "guest_image": {"version": "cube-image/2026.01", "agent_version": "agent-1.2.3"},
  "kernel": {"version": "5.10.0-100"}
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
	if !ok || kernel.Version != "5.10.0-100" {
		t.Errorf("kernel must come from kernel.version, got %+v ok=%v", kernel, ok)
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
