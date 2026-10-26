// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

const (
	maxEntryBytes     = 8 << 30
	maxExtractedBytes = 16 << 30
)

func destDirExists(baseDir, name, version string) bool {
	dst := templatetypes.VersionedComponentDir(baseDir, name, templatetypes.InventoryVersionKey(version))
	st, err := os.Stat(dst)
	return err == nil && st.IsDir()
}

// InstallBlob extracts a tar.gz blob into component_versions/<name>/<version>/.
// expectedChecksum is the sha256 of the compressed stream (sha256:<hex> or hex).
// sizeBytes, when > 0, bounds the read and is compared to bytes actually consumed.
func InstallBlob(ctx context.Context, baseDir, name, version string, r io.Reader, expectedChecksum string, sizeBytes int64) error {
	version = templatetypes.InventoryVersionKey(version)
	dst := templatetypes.VersionedComponentDir(baseDir, name, version)
	if destDirExists(baseDir, name, version) {
		return nil
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-"+version+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	h := sha256.New()
	src := r
	if sizeBytes > 0 {
		src = io.LimitReader(r, sizeBytes)
	}
	counted := &countingReader{r: io.TeeReader(src, h)}
	if err := extractTarGz(ctx, counted, tmp); err != nil {
		return fmt.Errorf("%w: extract: %v", ErrDownloadFailed, err)
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return fmt.Errorf("%w: drain: %v", ErrDownloadFailed, err)
	}
	if sizeBytes > 0 && counted.n != sizeBytes {
		return fmt.Errorf("%w: size mismatch got %d want %d", ErrDownloadFailed, counted.n, sizeBytes)
	}
	if expectedChecksum != "" {
		got := hex.EncodeToString(h.Sum(nil))
		want := strings.TrimPrefix(strings.TrimSpace(expectedChecksum), "sha256:")
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("%w: checksum mismatch", ErrDownloadFailed)
		}
	}
	if err := validateTree(tmp, name); err != nil {
		return fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		if st, statErr := os.Stat(dst); statErr == nil && st.IsDir() {
			return nil
		}
		return fmt.Errorf("%w: install: %v", ErrDownloadFailed, err)
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func validateTree(dir, name string) error {
	switch name {
	case templatetypes.CubeComponentCubeShim:
		for _, rel := range []string{
			templatetypes.RelativePathCubeShim,
			templatetypes.RelativePathCubeRuntime,
		} {
			if !regularFile(filepath.Join(dir, rel)) {
				return fmt.Errorf("missing %s", rel)
			}
		}
	case templatetypes.CubeComponentCubeImage:
		if !regularFile(filepath.Join(dir, templatetypes.RelativePathCubeImage)) {
			return fmt.Errorf("missing %s", templatetypes.RelativePathCubeImage)
		}
	case templatetypes.CubeComponentCubeAgent:
		if !regularFile(filepath.Join(dir, templatetypes.RelativePathCubeAgent)) {
			return fmt.Errorf("missing %s", templatetypes.RelativePathCubeAgent)
		}
	case templatetypes.CubeComponentCubeKernel:
		if !regularFile(filepath.Join(dir, "vmlinux")) &&
			!regularFile(filepath.Join(dir, "vmlinux-bm")) &&
			!regularFile(filepath.Join(dir, "vmlinux-pvm")) {
			return fmt.Errorf("missing vmlinux")
		}
	default:
		return fmt.Errorf("unsupported component %s", name)
	}
	return nil
}

func regularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func extractTarGz(ctx context.Context, r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > maxEntryBytes {
				return fmt.Errorf("entry %s too large", hdr.Name)
			}
			written += hdr.Size
			if written > maxExtractedBytes {
				return fmt.Errorf("archive exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, tr, hdr.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "..") {
				return fmt.Errorf("refusing symlink %s", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func safeJoin(root, name string) (string, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "/")
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("refusing path %q", name)
	}
	cleaned := filepath.Join(root, filepath.FromSlash(name))
	rel, err := filepath.Rel(root, cleaned)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("refusing path %q", name)
	}
	return cleaned, nil
}
