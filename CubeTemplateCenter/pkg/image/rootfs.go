package image

import (
	"context"
	"fmt"

	images "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// EnsureRootfsBuildPreflight checks the tools for the selected filesystem before
// pulling the source image. Filesystem selection is independent of OCI export.
func EnsureRootfsBuildPreflight(ctx context.Context, format string) error {
	format, err := images.ResolveRootfsFormat(format)
	if err != nil {
		return err
	}
	switch format {
	case images.ImageStorageMediaType_ext4.String():
		return ensureExt4BuildPreflight(ctx)
	default:
		return fmt.Errorf("unsupported rootfs build format %q", format)
	}
}

// BuildRootfs exports the source and invokes PostRootfsExport before building
// the selected immutable filesystem. An omitted format retains EXT4 behavior.
func BuildRootfs(ctx context.Context, source *PreparedSource, opts BuildOptions) (BuildResult, error) {
	format, err := images.ResolveRootfsFormat(opts.RootfsFormat)
	if err != nil {
		return BuildResult{}, err
	}
	var result BuildResult
	switch format {
	case images.ImageStorageMediaType_ext4.String():
		result, err = buildExt4(ctx, source, opts)
	default:
		return BuildResult{}, fmt.Errorf("unsupported rootfs build format %q", format)
	}
	if err != nil {
		return BuildResult{}, err
	}
	result.RootfsFormat = format
	return result, nil
}
