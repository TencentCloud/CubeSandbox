// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

// Package mntanchor decides where Cubelet keeps the handle of its private
// mount namespace, so that a restarted Cubelet re-enters the namespace its
// running sandboxes' shims live in instead of creating a new one.
//
// Cubelet runs in its own mount namespace, a slave of the host's: mounts it
// makes (for example S3 package metadata disks) are visible to shims started
// from it, and not to the host. A handle to that namespace is bind-mounted on
// a file; on start, Cubelet re-enters the namespace through the handle when
// the handle is still live, and creates a new namespace otherwise.
//
// When Cubelet itself runs on the host, the bind lives in the host mount
// namespace and outlives Cubelet restarts. When Cubelet runs in a container,
// the same bind made from inside the container lives only in the container's
// mount namespace and disappears with it, so every container restart created
// a new namespace. Shims started before the restart then no longer see what
// the new Cubelet mounts, and anything a shim writes into such a mount point
// lands beside the mount rather than in it. Pinning the handle in the host
// mount namespace (reachable when the container shares the host PID
// namespace) gives a containerized Cubelet the same lifetime as a host one.
//
// The namespace is cloned from the container's own mount namespace, so its
// root is that container's file system with its pod's volume mounts. It is
// reused only by later containers of the same pod (same volumes, same spec):
// the pod UID is recorded beside a host-pinned handle and a handle left by
// another pod is replaced, as a container-local handle always was. Without
// a known pod UID the handle stays container-local.
package mntanchor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// PodUIDEnv names the environment variable carrying the UID of the pod
// Cubelet runs in (Kubernetes downward API, metadata.uid).
const PodUIDEnv = "CUBELET_POD_UID"

// hostRoot reaches the host's file system view, through the host's mount
// namespace, from a process that shares the host PID namespace.
const hostRoot = "/proc/1/root"

// Paths are the locations Cubelet's mount namespace setup works with.
type Paths struct {
	ShimTopdir    string
	ShimSandboxes string
	HandleDir     string
	HandleFile    string
}

// Anchor says which mount namespace holds the handle.
type Anchor struct {
	Paths
	// Host is true when the handle is kept in the host mount namespace.
	Host bool
	// Owner is the pod UID a host-pinned handle belongs to.
	Owner string
}

// Deps are the host probes Choose uses; tests replace them.
type Deps struct {
	Readlink func(path string) (string, error)
	MkdirAll func(path string) error
	SameDir  func(containerPath, hostPath string) (bool, error)
	Getenv   func(key string) string
}

// DefaultDeps probes the running system.
func DefaultDeps() Deps {
	return Deps{
		Readlink: os.Readlink,
		MkdirAll: func(path string) error { return os.MkdirAll(path, 0o755) },
		SameDir:  SameDir,
		Getenv:   os.Getenv,
	}
}

// Choose keeps the handle in the host mount namespace only when Cubelet runs
// in a mount namespace other than the host's, can reach the host's view of
// the handle directory, and that view is the very directory Cubelet sees.
// In every other case the handle stays where it always was.
//
// The handle directory is created first: on a node Cubelet never ran on it
// does not exist yet, and comparing a missing directory would keep the very
// first namespace in the container, where the next restart loses it.
func Choose(paths Paths, deps Deps) Anchor {
	local := Anchor{Paths: paths}
	owner := ""
	if deps.Getenv != nil {
		owner = deps.Getenv(PodUIDEnv)
	}
	if owner == "" || len(owner) > 128 {
		return local
	}
	self, err := deps.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return local
	}
	host, err := deps.Readlink("/proc/1/ns/mnt")
	if err != nil || self == host {
		return local
	}
	if deps.MkdirAll == nil || deps.MkdirAll(paths.HandleDir) != nil {
		return local
	}
	same, err := deps.SameDir(paths.HandleDir, hostRoot+paths.HandleDir)
	if err != nil || !same {
		return local
	}
	return Anchor{Paths: paths, Host: true, Owner: owner}
}

// OwnerPath is the host-side file naming the pod a pinned handle belongs to.
func (a Anchor) OwnerPath() string {
	return hostRoot + filepath.Join(a.HandleDir, "mnt.owner")
}

// Reusable reports whether start-up may re-enter the namespace behind the
// handle: it must be a live mount namespace and, when host-pinned, have been
// created by a container of this same pod.
func (a Anchor) Reusable(isHandle func(string) bool, readFile func(string) ([]byte, error)) bool {
	if !isHandle(a.OpenPath()) {
		return false
	}
	if !a.Host {
		return true
	}
	recorded, err := readFile(a.OwnerPath())
	return err == nil && string(bytes.TrimSpace(recorded)) == a.Owner
}

// RecordOwner writes the pod UID beside a freshly pinned host handle.
func (a Anchor) RecordOwner(writeFile func(string, []byte, os.FileMode) error) error {
	if !a.Host {
		return nil
	}
	return writeFile(a.OwnerPath(), []byte(a.Owner+"\n"), 0o644)
}

// OpenPath is the path a process opens to enter the namespace.
func (a Anchor) OpenPath() string {
	if a.Host {
		return hostRoot + a.HandleFile
	}
	return a.HandleFile
}

// SetupCommands creates and pins a fresh namespace held open by pid. Only
// the commands that create and pin the handle differ between anchors.
func (a Anchor) SetupCommands(pid int) [][]string {
	p := strconv.Itoa(pid)
	pin := func(args ...string) []string {
		if !a.Host {
			return args
		}
		return append([]string{"nsenter", "-t", "1", "-m", "--"}, args...)
	}
	return [][]string{
		{"mkdir", "-p", a.ShimTopdir},
		{"mkdir", "-p", a.ShimSandboxes},
		{"nsenter", "-t", p, "-m", "mount", "--make-rslave", "/"},
		{"nsenter", "-t", p, "-m", "mount", "--bind", "--make-shared", a.ShimTopdir, a.ShimTopdir},
		pin("mkdir", "-p", a.HandleDir),
		pin("touch", a.HandleFile),
		pin("mount", "--bind", "--make-private", a.HandleDir, a.HandleDir),
		pin("mount", "--bind", "/proc/"+p+"/ns/mnt", a.HandleFile),
		{"kill", "-9", p},
	}
}

// StaleHandleCommands drop a dead host-side handle before a new one is
// pinned. The local handle is unmounted in-process by the caller, as before.
func (a Anchor) StaleHandleCommands() [][]string {
	if !a.Host {
		return nil
	}
	pin := []string{"nsenter", "-t", "1", "-m", "--"}
	return [][]string{
		append(append([]string{}, pin...), "umount", a.HandleFile),
		append(append([]string{}, pin...), "umount", a.HandleDir),
	}
}

// IsMntNsHandle reports whether path opens a live mount namespace. An empty
// placeholder file, a missing file or a handle to another namespace type
// all mean the namespace must be created again.
func IsMntNsHandle(path string) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil || fs.Type != unix.NSFS_MAGIC {
		return false
	}
	kind, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	return err == nil && kind == unix.CLONE_NEWNS
}

// SameDir reports whether two paths name the same directory.
func SameDir(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	if sa.Mode&unix.S_IFMT != unix.S_IFDIR || sb.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, errors.New("not a directory")
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino, nil
}
