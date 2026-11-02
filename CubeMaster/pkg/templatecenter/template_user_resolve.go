// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// maxTemplatePasswdBytes caps how much of the image's /etc/passwd is read.
// The file comes from an untrusted image, so never slurp it unbounded.
const maxTemplatePasswdBytes = 4 << 20

// ErrTemplateUserNotFound is returned when a template asks for a username
// that the image's /etc/passwd does not define.
var ErrTemplateUserNotFound = errors.New("template user not found in image")

// ErrTemplatePasswdInvalid is returned when the image's /etc/passwd cannot be
// used at all: it is not a regular file, or it is larger than the read cap.
// Only a username lookup fails on it; a numeric uid is resolved as if the
// file were missing.
var ErrTemplatePasswdInvalid = errors.New("invalid /etc/passwd in image")

// errTemplatePasswdMissing marks an image without /etc/passwd. It is only an
// error for usernames; a numeric uid does not need the file.
var errTemplatePasswdMissing = errors.New("image has no /etc/passwd")

// IsTemplateUserImageError reports whether err from
// ResolveTemplateUserFromRootfs depends only on the image content, so building
// the same image again cannot succeed. Other errors (rootfs open or read I/O
// failures, malformed requests) are not image errors.
func IsTemplateUserImageError(err error) bool {
	return errors.Is(err, ErrTemplateUserNotFound) || errors.Is(err, ErrTemplatePasswdInvalid)
}

// ResolvedTemplateUser is the numeric identity a template's --user resolved to
// against the image rootfs at build time. Cubelet only receives these numbers,
// so it never has to look the user up again when the sandbox is created.
type ResolvedTemplateUser struct {
	UID int64 `json:"uid"`
	GID int64 `json:"gid"`
	// InPasswd reports whether the uid has an /etc/passwd entry. A numeric uid
	// without one is allowed (Docker semantics); callers may log a warning.
	InPasswd bool `json:"in_passwd"`
}

// templateUserSecurityContext returns the security context carrying a
// template user, or nil when the request does not set one. A nil result means
// the build must behave exactly as it did before --user existed.
func templateUserSecurityContext(req *types.CreateTemplateFromImageReq) *types.ContainerSecurityContext {
	if req == nil || req.ContainerOverrides == nil {
		return nil
	}
	sc := req.ContainerOverrides.SecurityContext
	if sc == nil || (sc.RunAsUser == nil && sc.RunAsUsername == "") {
		return nil
	}
	return sc
}

// ResolveTemplateUserFromRootfs resolves the template user against the
// exported image rootfs. It returns (nil, nil) when the request sets no user.
// A username must exist in the image's /etc/passwd; a numeric uid is accepted
// even when it has no entry, in which case its gid defaults to 0.
func ResolveTemplateUserFromRootfs(rootfsDir string, req *types.CreateTemplateFromImageReq) (*ResolvedTemplateUser, error) {
	sc := templateUserSecurityContext(req)
	if sc == nil {
		return nil, nil
	}
	if err := validateTemplateContainerUser(req.ContainerOverrides); err != nil {
		return nil, err
	}

	entries, readErr := readTemplatePasswd(rootfsDir)
	if readErr != nil && !errors.Is(readErr, errTemplatePasswdMissing) && !errors.Is(readErr, ErrTemplatePasswdInvalid) {
		// Host I/O failure: neither a username nor a numeric uid can be
		// resolved reliably, and a retry may succeed.
		return nil, readErr
	}
	resolved := &ResolvedTemplateUser{}
	var entry *templatePasswdEntry
	if sc.RunAsUsername != "" {
		if readErr != nil {
			return nil, fmt.Errorf("%w: cannot resolve user %q: %w", ErrTemplateUserNotFound, sc.RunAsUsername, readErr)
		}
		for i := range entries {
			if entries[i].name == sc.RunAsUsername {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			return nil, fmt.Errorf("%w: user %q is not defined in /etc/passwd", ErrTemplateUserNotFound, sc.RunAsUsername)
		}
		resolved.UID = entry.uid
	} else {
		resolved.UID = sc.RunAsUser.Value
		for i := range entries {
			if entries[i].uid == resolved.UID {
				entry = &entries[i]
				break
			}
		}
	}
	resolved.InPasswd = entry != nil
	switch {
	case sc.RunAsGroup != nil:
		resolved.GID = sc.RunAsGroup.Value
	case entry != nil:
		resolved.GID = entry.gid
	}
	return resolved, nil
}

type templatePasswdEntry struct {
	name string
	uid  int64
	gid  int64
}

// readTemplatePasswd reads etc/passwd inside rootfsDir through os.Root, so a
// symlink planted in the image cannot make the build read host files.
// A missing file returns errTemplatePasswdMissing; a file that is not regular
// or too large returns ErrTemplatePasswdInvalid. Any other error is an I/O
// failure on the build host and is returned wrapped as is.
func readTemplatePasswd(rootfsDir string) ([]templatePasswdEntry, error) {
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return nil, fmt.Errorf("open rootfs: %w", err)
	}
	defer root.Close()
	// Stat before Open: the file comes from the image, and opening a FIFO
	// would block the build until a writer shows up.
	fi, err := root.Stat("etc/passwd")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errTemplatePasswdMissing
		}
		if isTemplatePasswdPathError(err) {
			return nil, fmt.Errorf("%w: %w", ErrTemplatePasswdInvalid, err)
		}
		return nil, fmt.Errorf("stat /etc/passwd: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file (mode %s)", ErrTemplatePasswdInvalid, fi.Mode().Type())
	}
	f, err := root.Open("etc/passwd")
	if err != nil {
		return nil, fmt.Errorf("open /etc/passwd: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTemplatePasswdBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read /etc/passwd: %w", err)
	}
	if len(data) > maxTemplatePasswdBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrTemplatePasswdInvalid, maxTemplatePasswdBytes)
	}
	return parseTemplatePasswd(data), nil
}

// parseTemplatePasswd keeps the first entry for each name, like getpwnam.
// Malformed lines are skipped rather than failing the build.
func parseTemplatePasswd(data []byte) []templatePasswdEntry {
	var out []templatePasswdEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), maxTemplatePasswdBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] == "" {
			continue
		}
		uid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}
		gid, err := strconv.ParseUint(fields[3], 10, 32)
		if err != nil {
			continue
		}
		out = append(out, templatePasswdEntry{name: fields[0], uid: int64(uid), gid: int64(gid)})
	}
	return out
}

// withResolvedTemplateUser returns req with its user replaced by the numeric
// identity resolved at build time. The input request is not modified. When
// the request has no user or nothing was resolved, req is returned as is.
func withResolvedTemplateUser(req *types.CreateTemplateFromImageReq, resolved *ResolvedTemplateUser) *types.CreateTemplateFromImageReq {
	sc := templateUserSecurityContext(req)
	if sc == nil || resolved == nil {
		return req
	}
	scCopy := *sc
	scCopy.RunAsUsername = ""
	scCopy.RunAsUser = &types.Int64Value{Value: resolved.UID}
	scCopy.RunAsGroup = &types.Int64Value{Value: resolved.GID}
	overrides := *req.ContainerOverrides
	overrides.SecurityContext = &scCopy
	reqCopy := *req
	reqCopy.ContainerOverrides = &overrides
	return &reqCopy
}

// resolvedTemplateUserFromArtifact recovers the resolved identity from an
// artifact's generated create request. Reused and adopted artifacts skip the
// rootfs export, but share the template spec fingerprint (which includes the
// user), so the numbers stored on the artifact are the ones to reuse.
func resolvedTemplateUserFromArtifact(artifact *models.RootfsArtifact) *ResolvedTemplateUser {
	if artifact == nil || strings.TrimSpace(artifact.GeneratedRequestJSON) == "" {
		return nil
	}
	var generated types.CreateCubeSandboxReq
	if err := json.Unmarshal([]byte(artifact.GeneratedRequestJSON), &generated); err != nil {
		return nil
	}
	if len(generated.Containers) == 0 || generated.Containers[0] == nil {
		return nil
	}
	sc := generated.Containers[0].SecurityContext
	if sc == nil || sc.RunAsUsername != "" || sc.RunAsUser == nil || sc.RunAsGroup == nil {
		return nil
	}
	return &ResolvedTemplateUser{UID: sc.RunAsUser.Value, GID: sc.RunAsGroup.Value}
}

// DecodeResolvedTemplateUser parses the resolved_user value of a BUILT
// callback payload. Older CubeTemplateCenter builds omit it; nil is returned.
func DecodeResolvedTemplateUser(v any) *ResolvedTemplateUser {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out ResolvedTemplateUser
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if validateTemplateUserID(out.UID) != nil || validateTemplateUserID(out.GID) != nil {
		return nil
	}
	return &out
}

// isTemplatePasswdPathError reports whether a stat error on etc/passwd comes
// from how the image lays out the path rather than from the build host: a
// symlink loop, a non-directory path component, or a symlink that os.Root
// refuses because it escapes the rootfs. os.Root reports the escape with a
// plain error, not an errno, so any non-errno failure is treated the same.
func isTemplatePasswdPathError(err error) bool {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return true
	}
	var errno syscall.Errno
	return !errors.As(err, &errno)
}
