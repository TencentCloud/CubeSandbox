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

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// maxTemplatePasswdBytes caps how much of the image's /etc/passwd is read.
// The file comes from an untrusted image, so never slurp it unbounded.
const maxTemplatePasswdBytes = 4 << 20

// ErrTemplateUserNotFound is returned when a template asks for a username
// that the image's /etc/passwd does not define.
var ErrTemplateUserNotFound = errors.New("template user not found in image")

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
	resolved := &ResolvedTemplateUser{}
	var entry *templatePasswdEntry
	if sc.RunAsUsername != "" {
		if readErr != nil {
			return nil, fmt.Errorf("%w: cannot resolve user %q: %v", ErrTemplateUserNotFound, sc.RunAsUsername, readErr)
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
func readTemplatePasswd(rootfsDir string) ([]templatePasswdEntry, error) {
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return nil, fmt.Errorf("open rootfs: %w", err)
	}
	defer root.Close()
	f, err := root.Open("etc/passwd")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("image has no /etc/passwd")
		}
		return nil, fmt.Errorf("open /etc/passwd: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTemplatePasswdBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read /etc/passwd: %w", err)
	}
	if len(data) > maxTemplatePasswdBytes {
		return nil, fmt.Errorf("/etc/passwd is larger than %d bytes", maxTemplatePasswdBytes)
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
