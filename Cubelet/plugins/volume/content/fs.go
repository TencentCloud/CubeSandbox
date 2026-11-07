// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

const (
	maxPathLen     = 4096
	maxListEntries = 100000
	maxListDepth   = 32
	timeLayout     = "2006-01-02T15:04:05.000000Z"
)

var (
	errBadPath    = errors.New("path must be an absolute volume path without '.', '..', NUL or backslash")
	errNotRegular = errors.New("not a regular file")
	errNotDir     = errors.New("not a directory")
)

// entryStat is the E2B VolumeEntryStat wire shape.
type entryStat struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Atime  string `json:"atime"`
	Mtime  string `json:"mtime"`
	Ctime  string `json:"ctime"`
	Target string `json:"target,omitempty"`
}

// cleanVolumePath validates a client path and returns its canonical volume
// path plus the root-relative form used with os.Root. This is hygiene only:
// containment against symlinks and races is enforced by os.Root itself.
func cleanVolumePath(p string) (volPath, rel string, err error) {
	if p == "" || p[0] != '/' || len(p) > maxPathLen || strings.ContainsAny(p, "\x00\\") {
		return "", "", errBadPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", "", errBadPath
		}
	}
	volPath = path.Clean(p)
	if volPath == "/" {
		return "/", ".", nil
	}
	return volPath, strings.TrimPrefix(volPath, "/"), nil
}

func entryType(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "file"
	default:
		return "unknown"
	}
}

// unixMode converts Go's FileMode to the permission bits E2B clients expect.
func unixMode(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

// goMode converts client mode bits to a FileMode. setuid/setgid are refused:
// volume content is writable by token holders and later mounted elsewhere.
func goMode(mode uint32) (fs.FileMode, error) {
	if mode&^uint32(0o7777) != 0 {
		return 0, errors.New("mode has bits outside 07777")
	}
	if mode&0o6000 != 0 {
		return 0, errors.New("setuid and setgid bits are not allowed")
	}
	m := fs.FileMode(mode & 0o777)
	if mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// describe builds the wire stat for info. root is used only to read symlink
// targets, which are reported solely when relative so host paths do not leak.
func describe(root *os.Root, volPath, rel string, info fs.FileInfo) entryStat {
	atime, ctime, uid, gid := statTimesOwner(info)
	name := path.Base(volPath)
	st := entryStat{
		Name: name, Type: entryType(info.Mode()), Path: volPath, Size: info.Size(),
		Mode: unixMode(info.Mode()), UID: uid, GID: gid,
		Atime: formatTime(atime), Mtime: formatTime(info.ModTime()), Ctime: formatTime(ctime),
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if target, err := root.Readlink(rel); err == nil && !path.IsAbs(target) {
			st.Target = target
		}
	}
	return st
}

func isEscape(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

func isErrno(err error, errnos ...syscall.Errno) bool {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return false
	}
	for _, want := range errnos {
		if e == want {
			return true
		}
	}
	return false
}
