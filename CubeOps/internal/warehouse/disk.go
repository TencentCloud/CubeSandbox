// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	dirPerm           = 0o755
	filePerm          = 0o644
	maxTarEntryBytes  = 8 << 30 // 8 GiB per file inside a streamed blob
	maxExtractedBytes = 16 << 30
)

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ValidateInstalledTree checks the required files for a component version.
func ValidateInstalledTree(dir, component string) error {
	switch component {
	case ComponentShim:
		for _, rel := range []string{
			filepath.Join("bin", "containerd-shim-cube-rs"),
			filepath.Join("bin", "cube-runtime"),
		} {
			if !fileExists(filepath.Join(dir, rel)) {
				return fmt.Errorf("missing %s", rel)
			}
		}
	case ComponentImage:
		if !fileExists(filepath.Join(dir, "cube-guest-image-cpu.img")) {
			return fmt.Errorf("missing cube-guest-image-cpu.img")
		}
	case ComponentAgent:
		if !fileExists(filepath.Join(dir, "cube-agent.ext4")) {
			return fmt.Errorf("missing cube-agent.ext4")
		}
	case ComponentKernel:
		if !fileExists(filepath.Join(dir, "vmlinux")) &&
			!fileExists(filepath.Join(dir, "vmlinux-bm")) &&
			!fileExists(filepath.Join(dir, "vmlinux-pvm")) {
			return fmt.Errorf("missing vmlinux")
		}
	default:
		return fmt.Errorf("unsupported component %s", component)
	}
	return nil
}

// WriteTarGz streams dir as a gzip-compressed tar to w. Paths are relative
// to dir; symlinks are stored as links (not followed). Headers are
// deterministic so identical trees produce identical archives.
func WriteTarGz(w io.Writer, dir string) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
			return fmt.Errorf("refusing to archive path %q", rel)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		header.ModTime = time.Unix(0, 0).UTC()
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.Format = tar.FormatPAX
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = target
			header.Typeflag = tar.TypeSymlink
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > maxTarEntryBytes {
			return fmt.Errorf("file %s too large", rel)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeTw := tw.Close()
	closeGz := gz.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeTw != nil {
		return closeTw
	}
	return closeGz
}

// ExtractTarGz unpacks a tar.gz into destDir with path-traversal guards.
func ExtractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	return extractTar(gz, destDir)
}

func extractTar(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	var written int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, dirPerm); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > maxTarEntryBytes {
				return fmt.Errorf("tar entry %s too large", hdr.Name)
			}
			written += hdr.Size
			if written > maxExtractedBytes {
				return fmt.Errorf("extracted archive exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			n, copyErr := io.CopyN(f, tr, hdr.Size)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
			if n != hdr.Size {
				return fmt.Errorf("short write for %s", hdr.Name)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
				return err
			}
			if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "..") {
				return fmt.Errorf("refusing symlink %s -> %s", hdr.Name, hdr.Linkname)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// skip other types (hard links, devices)
		}
	}
}

func safeJoin(root, name string) (string, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "/")
	if name == "" || name == "." {
		return root, nil
	}
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

func copyRegularFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := st.Mode() & 0o777
	if mode == 0 {
		mode = filePerm
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr != nil {
		_ = out.Close()
		return copyErr
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func relocateTree(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyTree(src, dst)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, dirPerm)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || strings.Contains(link, "..") {
				return fmt.Errorf("refusing symlink %s -> %s", path, link)
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyRegularFile(path, target)
		default:
			return nil
		}
	})
}
