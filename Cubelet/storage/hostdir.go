// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"golang.org/x/sys/unix"
)

var hostDirBasePath = "/data/cubelet/hostdir"

const hostDirMountTimeout = 3 * time.Second

const hostDirMountBinary = "/usr/bin/mount"

var runHostDirCommand = defaultRunHostDirCommand

type HostDirBackendInfo struct {
	VolumeName string `json:"volume_name"`

	ShareDir string `json:"share_dir"`

	BindPath string `json:"bind_path"`

	ReadOnly bool `json:"read_only"`
}

func defaultRunHostDirCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s %v: %w", name, args, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		if stderr.Len() > 0 {
			return fmt.Errorf("%w: %s", err, stderr.String())
		}
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return ctx.Err()
	}
}

func bindHostDir(ctx context.Context, srcPath, bindDest string, readOnly bool) error {
	mountCtx, cancel := context.WithTimeout(ctx, hostDirMountTimeout)
	defer cancel()

	// --no-canonicalize keeps the pinned /proc/<pid>/fd/<n> source intact so
	// the kernel mounts the descriptor's target rather than re-resolving a
	// user-controlled path.
	if err := runHostDirCommand(mountCtx, hostDirMountBinary, "--no-canonicalize", "--rbind", srcPath, bindDest); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("bind mount %s -> %s timed out after %s: %w", srcPath, bindDest, hostDirMountTimeout, err)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("bind mount %s -> %s canceled: %w", srcPath, bindDest, err)
		}
		return fmt.Errorf("bind mount %s -> %s: %w", srcPath, bindDest, err)
	}

	if !readOnly {
		return nil
	}

	remountCtx, remountCancel := context.WithTimeout(ctx, hostDirMountTimeout)
	defer remountCancel()

	if err := runHostDirCommand(remountCtx, hostDirMountBinary, "-o", "remount,bind,ro", bindDest); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("remount ro %s timed out after %s: %w", bindDest, hostDirMountTimeout, err)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("remount ro %s canceled: %w", bindDest, err)
		}
		return fmt.Errorf("remount ro %s: %w", bindDest, err)
	}
	return nil
}

func (l *local) prepareHostDirVolume(ctx context.Context, opts *workflow.CreateContext,
	v *cubebox.Volume, result *StorageInfo) error {

	hdv := v.GetVolumeSource().GetHostDirVolumes()
	if hdv == nil {
		return nil
	}
	if len(hdv.GetVolumeSources()) == 0 {
		return nil
	}

	sandboxID := opts.SandboxID
	if sandboxID == "" {
		return fmt.Errorf("prepareHostDirVolume: sandbox ID is empty")
	}
	allowedPrefixes, err := resolveHostMountAllowedPrefixes(opts.ReqInfo.GetAnnotations())
	if err != nil {
		return fmt.Errorf("prepareHostDirVolume: %w", err)
	}

	if result.HostDirBackendInfos == nil {
		result.HostDirBackendInfos = make(map[string]*HostDirBackendInfo)
	}

	for _, src := range hdv.GetVolumeSources() {
		if err := validateHostDirSourceName(src.GetName()); err != nil {
			return fmt.Errorf("prepareHostDirVolume: %w", err)
		}
		if src.GetHostPath() == "" {
			return fmt.Errorf("prepareHostDirVolume: volume %q has empty host_path", src.GetName())
		}
		resolvedSource, err := resolveHostDirSource(src.GetHostPath(), allowedPrefixes)
		if err != nil {
			return fmt.Errorf("prepareHostDirVolume: volume %q: %w", src.GetName(), err)
		}

		roStr := "rw"
		readOnly := false
		for _, c := range opts.ReqInfo.GetContainers() {
			for _, vm := range c.GetVolumeMounts() {
				if vm.GetName() == v.GetName() && vm.GetReadonly() {
					roStr = "ro"
					readOnly = true
				}
			}
		}

		shareDir := filepath.Join(hostDirBasePath, sandboxID, roStr)

		bindDest := filepath.Join(shareDir, src.GetName())

		if err := os.MkdirAll(bindDest, 0755); err != nil {
			return fmt.Errorf("prepareHostDirVolume: mkdir %s: %w", bindDest, err)
		}

		key := v.GetName() + "/" + src.GetName()
		result.HostDirBackendInfos[key] = &HostDirBackendInfo{
			VolumeName: v.GetName(),
			ShareDir:   shareDir,
			BindPath:   bindDest,
			ReadOnly:   readOnly,
		}

		sourceFile, err := openHostDirSource(resolvedSource)
		if err != nil {
			return fmt.Errorf("prepareHostDirVolume: volume %q: %w", src.GetName(), err)
		}
		sourcePath := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), sourceFile.Fd())
		log.G(ctx).Infof("[hostdir] binding %s -> %s (ro=%v)", resolvedSource, bindDest, readOnly)
		if err := func() error {
			defer sourceFile.Close()
			return bindHostDir(ctx, sourcePath, bindDest, readOnly)
		}(); err != nil {
			return fmt.Errorf("prepareHostDirVolume: %w", err)
		}

		log.G(ctx).Infof("[hostdir] bound %s -> %s (ro=%v, shareDir=%s)",
			resolvedSource, bindDest, readOnly, shareDir)
	}
	return nil
}

// hostMountAllowedPrefix keeps both spellings of a configured prefix. The raw
// form marks where the operator drew the boundary, so the symlink walk below
// starts there and still permits symlinked ancestors of the prefix itself
// (for example /data -> /mnt/ssd/data). The resolved form is used to compare
// against the fully resolved host path.
type hostMountAllowedPrefix struct {
	raw      string
	resolved string
}

func resolveHostMountAllowedPrefixes(annotations map[string]string) ([]hostMountAllowedPrefix, error) {
	rawPrefixes, ok, err := cubebox.HostMountAllowedPrefixes(annotations)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("missing %s annotation", cubebox.AnnotationHostMountAllowedPrefixes)
	}
	if len(rawPrefixes) == 0 {
		return nil, fmt.Errorf("%s annotation is empty", cubebox.AnnotationHostMountAllowedPrefixes)
	}

	prefixes := make([]hostMountAllowedPrefix, 0, len(rawPrefixes))
	for _, raw := range rawPrefixes {
		cleaned := filepath.Clean(raw)
		if !filepath.IsAbs(cleaned) || cleaned == "/" {
			return nil, fmt.Errorf("invalid allowed host mount prefix %q", raw)
		}
		resolved, err := filepath.EvalSymlinks(cleaned)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("resolve allowed host mount prefix %q: %w", raw, err)
		}
		if resolved == string(filepath.Separator) {
			return nil, fmt.Errorf("allowed host mount prefix %q resolves to the filesystem root", raw)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat allowed host mount prefix %q: %w", raw, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("allowed host mount prefix %q is not a directory", raw)
		}
		prefixes = append(prefixes, hostMountAllowedPrefix{
			raw:      cleaned,
			resolved: filepath.Clean(resolved),
		})
	}
	if len(prefixes) == 0 {
		return nil, errors.New("no allowed host mount prefix exists on this node")
	}
	return prefixes, nil
}

func resolveHostDirSource(hostPath string, allowedPrefixes []hostMountAllowedPrefix) (string, error) {
	cleaned := filepath.Clean(hostPath)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("host path %q is not absolute", hostPath)
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", fmt.Errorf("resolve host path %q: %w", hostPath, err)
	}
	resolved = filepath.Clean(resolved)
	for _, prefix := range allowedPrefixes {
		if !hostDirPathWithin(prefix.resolved, resolved) {
			continue
		}
		// A symlink anywhere below the prefix is rejected even when its final
		// target stays inside the prefix. Otherwise, with the default
		// /data/shared/ allowlist, a link such as tenant-a/x -> tenant-b would
		// mount another tenant's data. The prefix itself is exempt because the
		// operator configured that path (including any symlinked ancestors).
		//
		// Walk the spelling the caller supplied below whichever form of the
		// prefix it matched. Using the resolved path here would hide a link:
		// tenant-a -> tenant-b resolves to tenant-b, which has no link left to
		// inspect.
		var base, rel string
		switch {
		case hostDirPathWithin(prefix.raw, cleaned):
			base = prefix.raw
			rel, err = filepath.Rel(prefix.raw, cleaned)
		case hostDirPathWithin(prefix.resolved, cleaned):
			base = prefix.resolved
			rel, err = filepath.Rel(prefix.resolved, cleaned)
		default:
			// The path only reaches the prefix through a spelling that is not
			// configured and does not match the resolved form. Refuse to map
			// it rather than guess which components are links.
			continue
		}
		if err != nil {
			return "", fmt.Errorf("make host path %q relative to allowed prefix %q: %w", hostPath, base, err)
		}
		if err := rejectSymlinksBelowPrefix(base, rel); err != nil {
			return "", fmt.Errorf("host path %q: %w", hostPath, err)
		}
		return resolved, nil
	}
	return "", fmt.Errorf("host path %q resolves to %q, outside allowed host mount prefixes", hostPath, resolved)
}

// rejectSymlinksBelowPrefix lstats each component of rel under base and fails
// on the first symlink. base itself is not inspected: ancestors of a
// configured prefix are allowed to be symlinks.
func rejectSymlinksBelowPrefix(base, rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	current := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return fmt.Errorf("host path escapes allowed host mount prefix %q", base)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect host path component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("host path component %q is a symlink below allowed host mount prefix %q", current, base)
		}
	}
	return nil
}

func hostDirPathWithin(prefix, path string) bool {
	rel, err := filepath.Rel(prefix, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// openHostDirSource pins the resolved directory and rejects symlinks during
// every component lookup. The returned descriptor is what Cubelet mounts, so a
// concurrent rename cannot redirect the bind to another path.
func openHostDirSource(resolvedPath string) (*os.File, error) {
	dirFlags := unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
	fd, err := unix.Open("/", dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}

	rel, err := filepath.Rel("/", resolvedPath)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("make %q relative to root: %w", resolvedPath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		unix.Close(fd)
		return nil, fmt.Errorf("resolved path %q escapes the filesystem root", resolvedPath)
	}

	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		nextFD, err := unix.Openat(fd, part, dirFlags|unix.O_NOFOLLOW, 0)
		if err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("open %q without following symlinks: %w", resolvedPath, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(nextFD, &stat); err != nil {
			unix.Close(nextFD)
			unix.Close(fd)
			return nil, fmt.Errorf("stat %q: %w", resolvedPath, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			unix.Close(nextFD)
			unix.Close(fd)
			return nil, fmt.Errorf("host path %q is not a directory", resolvedPath)
		}
		unix.Close(fd)
		fd = nextFD
	}

	return os.NewFile(uintptr(fd), resolvedPath), nil
}

// validateHostDirSourceName requires a single path segment, so a volume stays
// inside its own directory under the share dir.
func validateHostDirSourceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("volume source name %q must be a single path segment", name)
	}
	return nil
}

func (l *local) cleanupHostDirVolumes(ctx context.Context, info *StorageInfo) error {
	if info == nil || info.SandboxID == "" || len(info.HostDirBackendInfos) == 0 {
		return nil
	}

	// Resolve symlinks on the parent (hostDirBasePath and its ancestors) only,
	// not on the per-sandbox leaf component. The kernel records fully resolved
	// mountpoints in /proc/self/mountinfo, so when any ancestor of
	// hostDirBasePath is a symlink (e.g. /data -> /mnt/ssd/data),
	// IsMountPoint's string comparison would otherwise miss the mounts, leaking
	// them and letting os.RemoveAll wipe the real backing directory.
	//
	// We deliberately do NOT EvalSymlinks the leaf <sandboxID>: if that
	// component were ever replaced by a symlink, resolving it would make
	// os.RemoveAll follow the link and delete the target's contents (a
	// link-following deletion hazard). Keeping the leaf unresolved means
	// RemoveAll only unlinks the symlink itself.
	resolvedBase, err := filepath.EvalSymlinks(hostDirBasePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cleanupHostDirVolumes: resolve %s: %w", hostDirBasePath, err)
	}
	sandboxDir := filepath.Join(resolvedBase, info.SandboxID)

	if _, err := os.Lstat(sandboxDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cleanupHostDirVolumes: stat %s: %w", sandboxDir, err)
	}

	if err := filepath.WalkDir(sandboxDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == sandboxDir {
			return nil
		}
		mounted, err := utils.IsMountPoint(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("check mount point %s: %w", path, err)
		}
		if mounted {
			if err := unix.Unmount(path, unix.MNT_DETACH); err != nil &&
				!errors.Is(err, unix.EINVAL) && !os.IsNotExist(err) {
				return fmt.Errorf("unmount %s: %w", path, err)
			}
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		log.G(ctx).Warnf("cleanupHostDirVolumes: skip removeAll %s: %v", sandboxDir, err)
		return err
	}

	if err := os.RemoveAll(sandboxDir); err != nil {
		log.G(ctx).Warnf("cleanupHostDirVolumes: removeAll %s: %v", sandboxDir, err)
		return err
	}
	return nil
}
