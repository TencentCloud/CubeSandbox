// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

// newInventoryArtifact writes <root>/<component>/<version>/<file> plus the
// version file the shim uses to identify an artifact.
func newInventoryArtifact(t *testing.T, root, component, version, file string) string {
	t.Helper()
	dir := filepath.Join(root, component, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveColdBootGuestArtifactsUsesLocalInventory(t *testing.T) {
	root := t.TempDir()
	kernel := newInventoryArtifact(t, root, templatetypes.CubeComponentCubeKernel, "k1", templatetypes.RelativePathCubeKernel)
	image := newInventoryArtifact(t, root, templatetypes.CubeComponentCubeImage, "i1", templatetypes.RelativePathCubeImage)
	agent := newInventoryArtifact(t, root, templatetypes.CubeComponentCubeAgent, "a1", templatetypes.RelativePathCubeAgent)

	plugin := &cubeboxInstancePlugin{
		config:           &cubeboxInstancePluginConfig{BasePath: filepath.Join(root, "toolbox")},
		versionedBaseDir: root,
	}
	template := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			templatetypes.CubeComponentCubeKernel: {Component: templatetypes.MachineComponent{Version: "k1"}},
			templatetypes.CubeComponentCubeImage:  {Component: templatetypes.MachineComponent{Version: "i1"}},
			templatetypes.CubeComponentCubeAgent:  {Component: templatetypes.MachineComponent{Version: "a1"}},
		},
	}

	gotKernel, gotImage, gotAgent, err := plugin.resolveColdBootGuestArtifacts(template, t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotKernel != kernel || gotImage != image || gotAgent != agent {
		t.Fatalf("paths = %q %q %q, want inventory paths", gotKernel, gotImage, gotAgent)
	}
}

func TestResolveColdBootGuestArtifactsRejectsVersionMismatch(t *testing.T) {
	root := t.TempDir()
	newInventoryArtifact(t, root, templatetypes.CubeComponentCubeKernel, "k1", templatetypes.RelativePathCubeKernel)
	plugin := &cubeboxInstancePlugin{
		config:           &cubeboxInstancePluginConfig{BasePath: filepath.Join(root, "toolbox")},
		versionedBaseDir: root,
	}
	template := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			// The inventory holds k1; the snapshot was taken from k2. Booting
			// with k1 would silently run a different guest kernel.
			templatetypes.CubeComponentCubeKernel: {Component: templatetypes.MachineComponent{Version: "k2"}},
			templatetypes.CubeComponentCubeImage:  {Component: templatetypes.MachineComponent{Version: "i1"}},
			templatetypes.CubeComponentCubeAgent:  {Component: templatetypes.MachineComponent{Version: "a1"}},
		},
	}

	_, _, _, err := plugin.resolveColdBootGuestArtifacts(template, t.TempDir())
	if err == nil {
		t.Fatal("expected version mismatch error")
	}
	if !strings.Contains(err.Error(), snapshotmode.ErrFilesystemOnlyGuestRuntime) {
		t.Fatalf("error %q must carry the stable identifier", err)
	}
}

func TestResolveColdBootGuestArtifactsFallsBackToSnapshotMetadata(t *testing.T) {
	root := t.TempDir()
	kernel := newInventoryArtifact(t, root, templatetypes.CubeComponentCubeKernel, "k1", templatetypes.RelativePathCubeKernel)
	image := newInventoryArtifact(t, root, templatetypes.CubeComponentCubeImage, "i1", templatetypes.RelativePathCubeImage)
	agent := newInventoryArtifact(t, root, templatetypes.CubeComponentCubeAgent, "a1", templatetypes.RelativePathCubeAgent)

	specDir := t.TempDir()
	metadata := `{"kernel_version":"k1","image_version":"i1","agent_version":"a1","ch_version":"x"}`
	if err := os.WriteFile(filepath.Join(specDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}

	plugin := &cubeboxInstancePlugin{
		config:           &cubeboxInstancePluginConfig{BasePath: filepath.Join(root, "toolbox")},
		versionedBaseDir: root,
	}
	// The recovered run template carries no component versions; the package
	// metadata is the only identity source.
	gotKernel, gotImage, gotAgent, err := plugin.resolveColdBootGuestArtifacts(&templatetypes.LocalRunTemplate{}, specDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotKernel != kernel || gotImage != image || gotAgent != agent {
		t.Fatalf("paths = %q %q %q, want inventory paths", gotKernel, gotImage, gotAgent)
	}
}

// TestResolveColdBootGuestArtifactsMatchesDigestInventoryKey covers the shape
// real installs have: the inventory directory is named after the short key
// ("sha256-10c21b61578a") while the version file next to the artifact keeps the
// raw digest ("sha256:10c21b..."). Run templates record the key, so comparing
// the two literally would reject every digest-versioned artifact and no
// filesystem-only snapshot could ever cold boot.
func TestResolveColdBootGuestArtifactsMatchesDigestInventoryKey(t *testing.T) {
	const (
		key = "sha256-10c21b61578a"
		raw = "sha256:10c21b61578a0b1c814da7d8b27526c9d81f34f542e55cb93c8274676e16e4a0"
	)
	root := t.TempDir()
	write := func(component, file string) string {
		t.Helper()
		// Directory named by key, version file holding the raw digest.
		dir := filepath.Join(root, component, key)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, []byte("artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "version"), []byte(raw+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	kernel := write(templatetypes.CubeComponentCubeKernel, templatetypes.RelativePathCubeKernel)
	image := write(templatetypes.CubeComponentCubeImage, templatetypes.RelativePathCubeImage)
	agent := write(templatetypes.CubeComponentCubeAgent, templatetypes.RelativePathCubeAgent)

	plugin := &cubeboxInstancePlugin{
		config:           &cubeboxInstancePluginConfig{BasePath: filepath.Join(root, "toolbox")},
		versionedBaseDir: root,
	}
	template := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			templatetypes.CubeComponentCubeKernel: {Component: templatetypes.MachineComponent{Version: key}},
			templatetypes.CubeComponentCubeImage:  {Component: templatetypes.MachineComponent{Version: key}},
			templatetypes.CubeComponentCubeAgent:  {Component: templatetypes.MachineComponent{Version: key}},
		},
	}

	gotKernel, gotImage, gotAgent, err := plugin.resolveColdBootGuestArtifacts(template, t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotKernel != kernel || gotImage != image || gotAgent != agent {
		t.Fatalf("paths = %q %q %q, want inventory paths", gotKernel, gotImage, gotAgent)
	}
}

func TestColdBootWritableLayerPrefersTheRecordedLayer(t *testing.T) {
	specDir := t.TempDir()
	// A sandbox that was itself cold-booted captures the layer of the sandbox
	// that created it while its own guest id differs. Only the recorded layer
	// points at the directory that holds the files.
	metadata := `{"rootfs_writable_layer":"disk/tpl-1e0d677b60a0499c80f49e55_0",` +
		`"app_snapshot_container_id":"sandbox-that-cold-booted"}`
	if err := os.WriteFile(filepath.Join(specDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := coldBootWritableLayer(specDir)
	if err != nil {
		t.Fatalf("coldBootWritableLayer: %v", err)
	}
	if got != "disk/tpl-1e0d677b60a0499c80f49e55_0" {
		t.Fatalf("layer = %q, want the recorded layer", got)
	}
}

func TestColdBootWritableLayerFallsBackToTheContainerID(t *testing.T) {
	// Packages written before the layer name was recorded only carry the
	// captured container id, which is the layer for a first-generation sandbox.
	specDir := t.TempDir()
	metadata := `{"app_snapshot_container_id":"tpl-1e0d677b60a0499c80f49e55_0"}`
	if err := os.WriteFile(filepath.Join(specDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := coldBootWritableLayer(specDir)
	if err != nil {
		t.Fatalf("coldBootWritableLayer: %v", err)
	}
	if got != "disk/tpl-1e0d677b60a0499c80f49e55_0" {
		t.Fatalf("layer = %q, want the captured container id built into a layer", got)
	}
}

func TestColdBootWritableLayerRejectsAPathOutsideTheDiskDirectory(t *testing.T) {
	for name, layer := range map[string]string{
		"parent":     "disk/..",
		"nested":     "disk/a/b",
		"absolute":   "/data/cubelet/disk/x",
		"no_prefix":  "tpl-1_0",
		"empty_name": "disk/",
		"backslash":  `disk/a\b`,
		"dot":        "disk/.",
	} {
		t.Run(name, func(t *testing.T) {
			specDir := t.TempDir()
			metadata := `{"rootfs_writable_layer":"` + strings.ReplaceAll(layer, `\`, `\\`) + `"}`
			if err := os.WriteFile(filepath.Join(specDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := coldBootWritableLayer(specDir)
			if err == nil || !strings.Contains(err.Error(), snapshotmode.ErrFilesystemOnlyGuestRuntime) {
				t.Fatalf("layer %q: err = %v, want the stable identifier", layer, err)
			}
		})
	}
}

// The fallback derives the layer from the package's container id, so it is the
// same untrusted input the recorded layer is validated as.
func TestColdBootContainerIDRejectsAPathOutsideTheDiskDirectory(t *testing.T) {
	for name, containerID := range map[string]string{
		"parent":    "../other/disk",
		"nested":    "a/b",
		"absolute":  "/data/cubelet/disk/x",
		"dot":       ".",
		"backslash": `a\b`,
	} {
		t.Run(name, func(t *testing.T) {
			specDir := t.TempDir()
			metadata := `{"app_snapshot_container_id":"` + strings.ReplaceAll(containerID, `\`, `\\`) + `"}`
			if err := os.WriteFile(filepath.Join(specDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := coldBootWritableLayer(specDir)
			if err == nil || !strings.Contains(err.Error(), snapshotmode.ErrFilesystemOnlyGuestRuntime) {
				t.Fatalf("container id %q: err = %v, want the stable identifier", containerID, err)
			}
		})
	}
}

func TestColdBootContainerIDRejectsAnUnrecordedLayout(t *testing.T) {
	for name, body := range map[string]string{
		"missing_field": `{"kernel_version":"k1"}`,
		"blank_field":   `{"app_snapshot_container_id":"  "}`,
		"malformed":     `{not-json`,
	} {
		t.Run(name, func(t *testing.T) {
			specDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(specDir, "metadata.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := coldBootWritableLayer(specDir)
			if err == nil {
				t.Fatal("expected an error: booting without the writable layer would drop captured files")
			}
			if !strings.Contains(err.Error(), snapshotmode.ErrFilesystemOnlyGuestRuntime) {
				t.Fatalf("error %q must carry the stable identifier", err)
			}
		})
	}
}
