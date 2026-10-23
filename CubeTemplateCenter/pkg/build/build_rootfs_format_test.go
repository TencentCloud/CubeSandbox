// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
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

	// Non-canonical erofs values ("EROFS", " erofs ") MUST dispatch to BuildErofs,
	// not BuildExt4. Before sharing NormalizeRootfsFormat, TC compared with a
	// raw == "erofs" and these fell through to BuildExt4 while CubeMaster
	// normalized them to erofs for distribution — shipping an ext4-built artifact
	// as StorageMedia=erofs that fails to mount. This is the regression guard for
	// that asymmetry: the build dispatch must agree with CubeMaster's
	// canonicalization.
	for _, nonCanonical := range []string{"EROFS", " erofs ", "\tErofs\t"} {
		ext4Called, erofsCalled = false, false
		if _, err := buildRootfsImage(context.Background(), nonCanonical, src, opts); err != nil {
			t.Fatalf("non-canonical erofs dispatch %q: %v", nonCanonical, err)
		}
		if !erofsCalled || ext4Called {
			t.Fatalf("non-canonical erofs %q must dispatch to BuildErofs (not ext4): ext4=%v erofs=%v",
				nonCanonical, ext4Called, erofsCalled)
		}
	}
}

func TestRootfsFormatLabel(t *testing.T) {
	// rootfsFormatLabel now routes through the shared NormalizeRootfsFormat, so
	// the BUILT payload carries a CANONICAL format — never a raw non-canonical
	// input. This is the fix for the TC/CubeMaster asymmetry: previously an
	// unknown value surfaced verbatim and CubeMaster then normalized it to erofs,
	// shipping an ext4-built artifact as StorageMedia=erofs. Now an unknown
	// value normalizes to ext4 (the historical default) on both sides.
	cases := []struct {
		in, want string
	}{
		{config.RootfsFormatExt4, "ext4"},
		{"", "ext4"},
		{config.RootfsFormatErofs, "erofs"},
		// Non-canonical erofs values normalize to "erofs" (trim + lower), so the
		// BUILT payload agrees with what CubeMaster records for distribution.
		{"EROFS", "erofs"},
		{"  erofs ", "erofs"},
		// Unknown values normalize to "ext4" (never surface verbatim), so a
		// misconfigured format never ships as a wrong StorageMedia downstream.
		{"future-format", "ext4"},
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
		// Non-canonical erofs values report BUILDING_EROFS (canonicalized), so the
		// phase TC reports agrees with the format it actually builds — not the
		// raw input that would have fallen through to BUILDING_EXT4 before.
		{"EROFS", templatecenter.JobPhaseBuildingErofs},
		{"  erofs ", templatecenter.JobPhaseBuildingErofs},
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
	patches.ApplyMethod(reflect.TypeOf(&Reporter{}), "Report", func(_ *Reporter, _ context.Context, _ string, fields map[string]any) error {
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
	// A non-canonical erofs value in the BUILT payload is normalized to "erofs"
	// (NOT surfaced verbatim), so CubeMaster and TC agree on the format. Before
	// sharing NormalizeRootfsFormat, rootfsFormatLabel passed "EROFS" through
	// verbatim and CubeMaster then normalized it to erofs for distribution while
	// TC built ext4 — the ext4/erofs mismatch that fails at mount. This asserts
	// the payload is already canonical on the TC side.
	report("EROFS")
	if got := captured["rootfs_format"]; got != "erofs" {
		t.Fatalf("rootfs_format for non-canonical EROFS = %v, want %q (canonicalized, not verbatim)", got, "erofs")
	}
	// The payload still carries the other artifact fields untouched.
	if captured["artifact_id"] != "rfs-1" || captured["ext4_sha256"] != "sha" {
		t.Fatalf("payload missing core artifact fields: %#v", captured)
	}
}

// TestEnsureBuildPreflightDispatchesByFormat verifies ensureBuildPreflight runs
// the shared image.EnsureArtifactBuildPreflight for every format, then gates the
// format-specific preflight: image.EnsureExt4BuildPreflight for ext4 (mkfs.ext4
// + loop-mount) and image.EnsureErofsBuildPreflight for erofs (mkfs.erofs). So a
// pure-erofs host is not asked for the ext4 toolchain, and vice versa. All three
// preflights are patched out, so this is a pure dispatch test that needs no
// mkfs.ext4 / mkfs.erofs on the host.
func TestEnsureBuildPreflightDispatchesByFormat(t *testing.T) {
	artifactCalled := false
	ext4Called := false
	erofsCalled := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(image.EnsureArtifactBuildPreflight, func(ctx context.Context) error {
		artifactCalled = true
		return nil
	})
	patches.ApplyFunc(image.EnsureExt4BuildPreflight, func(ctx context.Context) error {
		ext4Called = true
		return nil
	})
	patches.ApplyFunc(image.EnsureErofsBuildPreflight, func(ctx context.Context) error {
		erofsCalled = true
		return nil
	})

	// ext4: shared preflight + ext4-specific (mkfs.ext4/loop-mount). NOT erofs.
	artifactCalled, ext4Called, erofsCalled = false, false, false
	if err := ensureBuildPreflight(context.Background(), config.RootfsFormatExt4); err != nil {
		t.Fatalf("ext4 preflight: %v", err)
	}
	if !artifactCalled || !ext4Called || erofsCalled {
		t.Fatalf("ext4 must call shared+EnsureExt4BuildPreflight only: artifact=%v ext4=%v erofs=%v",
			artifactCalled, ext4Called, erofsCalled)
	}

	// Empty format defaults to ext4: shared + ext4-specific. A legacy request
	// snapshot that predates RootfsFormat is NOT asked for mkfs.erofs.
	artifactCalled, ext4Called, erofsCalled = false, false, false
	if err := ensureBuildPreflight(context.Background(), ""); err != nil {
		t.Fatalf("empty preflight: %v", err)
	}
	if !artifactCalled || !ext4Called || erofsCalled {
		t.Fatalf("empty must call shared+EnsureExt4BuildPreflight only: artifact=%v ext4=%v erofs=%v",
			artifactCalled, ext4Called, erofsCalled)
	}

	// erofs: shared preflight + erofs-specific (mkfs.erofs). NOT the ext4
	// toolchain — a pure-erofs host without mkfs.ext4 must not be rejected here.
	artifactCalled, ext4Called, erofsCalled = false, false, false
	if err := ensureBuildPreflight(context.Background(), config.RootfsFormatErofs); err != nil {
		t.Fatalf("erofs preflight: %v", err)
	}
	if !artifactCalled || !erofsCalled || ext4Called {
		t.Fatalf("erofs must call shared+EnsureErofsBuildPreflight only (NOT ext4): artifact=%v ext4=%v erofs=%v",
			artifactCalled, ext4Called, erofsCalled)
	}

	// Non-canonical erofs ("EROFS", " erofs ") must STILL run the erofs preflight
	// (canonicalized), not the ext4 one. Before sharing NormalizeRootfsFormat
	// the raw == "erofs" compare missed these and skipped the erofs check.
	for _, nonCanonical := range []string{"EROFS", "  erofs "} {
		artifactCalled, ext4Called, erofsCalled = false, false, false
		if err := ensureBuildPreflight(context.Background(), nonCanonical); err != nil {
			t.Fatalf("non-canonical erofs preflight %q: %v", nonCanonical, err)
		}
		if !artifactCalled || !erofsCalled || ext4Called {
			t.Fatalf("non-canonical erofs %q must call shared+erofs only (NOT ext4): artifact=%v ext4=%v erofs=%v",
				nonCanonical, artifactCalled, ext4Called, erofsCalled)
		}
	}

	// An unknown format falls back to ext4: shared + ext4-specific (no erofs),
	// matching buildRootfsImage's ext4 fallback.
	artifactCalled, ext4Called, erofsCalled = false, false, false
	if err := ensureBuildPreflight(context.Background(), "future-format"); err != nil {
		t.Fatalf("unknown preflight: %v", err)
	}
	if !artifactCalled || !ext4Called || erofsCalled {
		t.Fatalf("unknown must call shared+EnsureExt4BuildPreflight only: artifact=%v ext4=%v erofs=%v",
			artifactCalled, ext4Called, erofsCalled)
	}
}

// TestEnsureBuildPreflightShortCircuitsOnSharedPreflightFailure verifies that
// when the shared preflight (export tools / truncate / cp) fails, neither the
// ext4-specific nor the erofs-specific check runs and the shared error
// propagates — so a missing shared tool is reported once, not masked by a
// subsequent format-specific error.
func TestEnsureBuildPreflightShortCircuitsOnSharedPreflightFailure(t *testing.T) {
	ext4Called := false
	erofsCalled := false
	sharedErr := errors.New("shared preflight boom")
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(image.EnsureArtifactBuildPreflight, func(ctx context.Context) error {
		return sharedErr
	})
	patches.ApplyFunc(image.EnsureExt4BuildPreflight, func(ctx context.Context) error {
		ext4Called = true
		return nil
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
	if ext4Called {
		t.Fatal("ext4 preflight must not run when the shared preflight already failed")
	}
}
