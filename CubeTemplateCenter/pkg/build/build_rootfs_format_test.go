// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
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
