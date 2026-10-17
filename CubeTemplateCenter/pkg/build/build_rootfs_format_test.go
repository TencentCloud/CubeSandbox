// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

// TestBuildRootfsImageDispatch verifies runBuildLocked's format dispatch:
// ext4 (and the empty legacy default) route to image.BuildExt4, while erofs
// routes to image.BuildErofs. The builders are patched out so this is a pure
// dispatch test that does not require mkfs.ext4 / mkfs.erofs on the host.
func TestBuildRootfsImageDispatch(t *testing.T) {
	ext4Called := false
	erofsCalled := false

	wantExt4 := image.BuildResult{Ext4Path: "/tmp/x.ext4", SHA256: "ext4-sha", SizeBytes: 111}
	wantErofs := image.BuildResult{Ext4Path: "/tmp/x.erofs", SHA256: "erofs-sha", SizeBytes: 222}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(image.BuildExt4, func(ctx context.Context, source *image.PreparedSource, opts image.BuildOptions) (image.BuildResult, error) {
		ext4Called = true
		return wantExt4, nil
	})
	patches.ApplyFunc(image.BuildErofs, func(ctx context.Context, source *image.PreparedSource, opts image.BuildOptions) (image.BuildResult, error) {
		erofsCalled = true
		return wantErofs, nil
	})

	src := &image.PreparedSource{LocalRef: "docker.io/library/nginx:latest", ExportMode: image.ExportModeNative}
	opts := image.BuildOptions{ArtifactID: "rfs-1"}

	// Explicit ext4 -> BuildExt4.
	got, err := buildRootfsImage(context.Background(), config.RootfsFormatExt4, src, opts)
	if err != nil {
		t.Fatalf("ext4 dispatch: %v", err)
	}
	if !ext4Called || erofsCalled {
		t.Fatalf("ext4 format must dispatch to BuildExt4 only: ext4=%v erofs=%v", ext4Called, erofsCalled)
	}
	if got != wantExt4 {
		t.Fatalf("ext4 result mismatch: %#v", got)
	}

	// Empty format defaults to ext4 (matches CubeMaster's normalize behavior
	// for legacy / older request snapshots that predate RootfsFormat).
	ext4Called, erofsCalled = false, false
	got, err = buildRootfsImage(context.Background(), "", src, opts)
	if err != nil {
		t.Fatalf("empty dispatch: %v", err)
	}
	if !ext4Called || erofsCalled {
		t.Fatalf("empty format must default to BuildExt4: ext4=%v erofs=%v", ext4Called, erofsCalled)
	}
	if got != wantExt4 {
		t.Fatalf("empty result mismatch: %#v", got)
	}

	// Explicit erofs -> BuildErofs.
	ext4Called, erofsCalled = false, false
	got, err = buildRootfsImage(context.Background(), config.RootfsFormatErofs, src, opts)
	if err != nil {
		t.Fatalf("erofs dispatch: %v", err)
	}
	if !erofsCalled || ext4Called {
		t.Fatalf("erofs format must dispatch to BuildErofs only: erofs=%v ext4=%v", erofsCalled, ext4Called)
	}
	if got != wantErofs {
		t.Fatalf("erofs result mismatch: %#v", got)
	}

	// An unknown format falls back to ext4 rather than crashing, so a
	// future/mismatched value still produces a usable (ext4) artifact.
	ext4Called, erofsCalled = false, false
	if _, err := buildRootfsImage(context.Background(), "future-format", src, opts); err != nil {
		t.Fatalf("unknown dispatch: %v", err)
	}
	if !ext4Called || erofsCalled {
		t.Fatalf("unknown format must fall back to BuildExt4: ext4=%v erofs=%v", ext4Called, erofsCalled)
	}
}

func TestRootfsFormatLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{config.RootfsFormatExt4, "ext4"},
		{"", "ext4"},
		{config.RootfsFormatErofs, "erofs"},
		{"future-format", "future-format"},
	}
	for _, c := range cases {
		if got := rootfsFormatLabel(c.in); got != c.want {
			t.Fatalf("rootfsFormatLabel(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(rootfsFormatLabel(c.in), " ") {
			t.Fatalf("label must not contain spaces: %q", rootfsFormatLabel(c.in))
		}
	}
}

// TestBuildingPhaseForRootfsFormat verifies the reported build phase carries
// the rootfs packaging format (BUILDING_EXT4 / BUILDING_EROFS) instead of a
// hardcoded BUILDING_EXT4, with empty/unknown falling back to ext4.
func TestBuildingPhaseForRootfsFormat(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{config.RootfsFormatExt4, templatecenter.JobPhaseBuildingExt4},
		{"", templatecenter.JobPhaseBuildingExt4},
		{config.RootfsFormatErofs, templatecenter.JobPhaseBuildingErofs},
		{"unknown-format", templatecenter.JobPhaseBuildingExt4},
	}
	for _, c := range cases {
		if got := buildingPhaseForRootfsFormat(c.in); got != c.want {
			t.Fatalf("buildingPhaseForRootfsFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReportArtifactBuiltIncludesRootfsFormat verifies the BUILT callback payload
// carries rootfs_format, normalized via rootfsFormatLabel so CubeMaster receives
// the format the artifact was built in. The Reporter.Report HTTP call is patched
// out so this captures the exact payload without a CubeMaster endpoint.
func TestReportArtifactBuiltIncludesRootfsFormat(t *testing.T) {
	var captured map[string]any
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(&Reporter{}, "Report", func(_ *Reporter, _ context.Context, _ string, fields map[string]any) error {
		captured = fields
		return nil
	})

	src := &image.PreparedSource{Digest: "sha256:src", ConfigJSON: "{}", MasterNodeIP: "http://master:8080"}
	result := image.BuildResult{Ext4Path: "/data/rfs-1.ext4", SHA256: "sha", SizeBytes: 4096}
	// reportArtifactBuilt only touches the logger on the failure path; the
	// patched Report succeeds, so a zero Entry (nil inner logger) is safe here.
	logger := &cubelog.Entry{}

	report := func(format string) {
		captured = nil
		if err := reportArtifactBuilt(context.Background(), "job-1", &result, "rfs-1", "fp", src, &Reporter{}, cube_egress_ca.Result{}, "", false, nil, "", format, logger); err != nil {
			t.Fatalf("reportArtifactBuilt(%q) failed: %v", format, err)
		}
	}

	// erofs is reported verbatim.
	report(config.RootfsFormatErofs)
	if got := captured["rootfs_format"]; got != "erofs" {
		t.Fatalf("rootfs_format for erofs = %v, want %q", got, "erofs")
	}
	// ext4 is reported verbatim.
	report(config.RootfsFormatExt4)
	if got := captured["rootfs_format"]; got != "ext4" {
		t.Fatalf("rootfs_format for ext4 = %v, want %q", got, "ext4")
	}
	// An empty format (legacy request snapshot) is normalized to ext4 so an
	// older job still reports a usable format.
	report("")
	if got := captured["rootfs_format"]; got != "ext4" {
		t.Fatalf("rootfs_format for empty = %v, want %q (ext4 default)", got, "ext4")
	}
	// The payload still carries the other artifact fields untouched.
	if captured["artifact_id"] != "rfs-1" || captured["ext4_sha256"] != "sha" {
		t.Fatalf("payload missing core artifact fields: %#v", captured)
	}
}

// TestEnsureBuildPreflightDispatchesByFormat verifies ensureBuildPreflight runs
// the shared image.EnsureArtifactBuildPreflight for every format and
// additionally runs image.EnsureErofsBuildPreflight only for erofs, so the erofs
// path fails fast at PULLING when mkfs.erofs is missing instead of deep inside
// BUILDING_EROFS. Both preflights are patched out, so this is a pure dispatch
// test that needs no mkfs.ext4 / mkfs.erofs on the host.
func TestEnsureBuildPreflightDispatchesByFormat(t *testing.T) {
	artifactCalled := false
	erofsCalled := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(image.EnsureArtifactBuildPreflight, func(ctx context.Context) error {
		artifactCalled = true
		return nil
	})
	patches.ApplyFunc(image.EnsureErofsBuildPreflight, func(ctx context.Context) error {
		erofsCalled = true
		return nil
	})

	// ext4: only the shared preflight.
	artifactCalled, erofsCalled = false, false
	if err := ensureBuildPreflight(context.Background(), config.RootfsFormatExt4); err != nil {
		t.Fatalf("ext4 preflight: %v", err)
	}
	if !artifactCalled || erofsCalled {
		t.Fatalf("ext4 must call only EnsureArtifactBuildPreflight: artifact=%v erofs=%v", artifactCalled, erofsCalled)
	}

	// Empty format defaults to ext4: only the shared preflight, so a legacy
	// request snapshot that predates RootfsFormat is not asked for mkfs.erofs.
	artifactCalled, erofsCalled = false, false
	if err := ensureBuildPreflight(context.Background(), ""); err != nil {
		t.Fatalf("empty preflight: %v", err)
	}
	if !artifactCalled || erofsCalled {
		t.Fatalf("empty must call only EnsureArtifactBuildPreflight: artifact=%v erofs=%v", artifactCalled, erofsCalled)
	}

	// erofs: both the shared preflight and the erofs-specific one.
	artifactCalled, erofsCalled = false, false
	if err := ensureBuildPreflight(context.Background(), config.RootfsFormatErofs); err != nil {
		t.Fatalf("erofs preflight: %v", err)
	}
	if !artifactCalled || !erofsCalled {
		t.Fatalf("erofs must call both preflights: artifact=%v erofs=%v", artifactCalled, erofsCalled)
	}

	// An unknown format falls back to the shared preflight only (no erofs
	// check), matching buildRootfsImage's ext4 fallback.
	artifactCalled, erofsCalled = false, false
	if err := ensureBuildPreflight(context.Background(), "future-format"); err != nil {
		t.Fatalf("unknown preflight: %v", err)
	}
	if !artifactCalled || erofsCalled {
		t.Fatalf("unknown must call only EnsureArtifactBuildPreflight: artifact=%v erofs=%v", artifactCalled, erofsCalled)
	}
}

// TestEnsureBuildPreflightShortCircuitsOnSharedPreflightFailure verifies that
// when the shared preflight fails, the erofs-specific check is skipped and the
// shared error propagates — so a missing mkfs.ext4 is reported once, not masked
// by a subsequent mkfs.erofs error.
func TestEnsureBuildPreflightShortCircuitsOnSharedPreflightFailure(t *testing.T) {
	erofsCalled := false
	sharedErr := errors.New("shared preflight boom")
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(image.EnsureArtifactBuildPreflight, func(ctx context.Context) error {
		return sharedErr
	})
	patches.ApplyFunc(image.EnsureErofsBuildPreflight, func(ctx context.Context) error {
		erofsCalled = true
		return nil
	})

	err := ensureBuildPreflight(context.Background(), config.RootfsFormatErofs)
	if !errors.Is(err, sharedErr) {
		t.Fatalf("expected shared preflight error to propagate, got: %v", err)
	}
	if erofsCalled {
		t.Fatal("erofs preflight must not run when the shared preflight already failed")
	}
}
