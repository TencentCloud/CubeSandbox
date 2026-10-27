// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// ParseTemplateUser parses a Docker-style user spec into the security context
// stored on a template. Accepted forms are uid, uid:gid, username, and
// username:gid. The group must be numeric because the guest runtime only
// records a numeric RunAsGroup. The template build stays privileged so the
// guest can still set up devices; only the container process user changes.
func ParseTemplateUser(spec string) (*types.ContainerSecurityContext, error) {
	userPart, groupPart, hasGroup, err := splitTemplateUser(spec)
	if err != nil {
		return nil, err
	}
	sc := &types.ContainerSecurityContext{Privileged: true}
	if isDecimalID(userPart) {
		uid, err := parseTemplateUserID(userPart)
		if err != nil {
			return nil, fmt.Errorf("invalid user %q: %w", spec, err)
		}
		sc.RunAsUser = &types.Int64Value{Value: uid}
	} else if err := validateTemplateUsername(userPart); err != nil {
		return nil, fmt.Errorf("invalid user %q: %w", spec, err)
	} else {
		sc.RunAsUsername = userPart
	}
	if !hasGroup {
		return sc, nil
	}
	if !isDecimalID(groupPart) {
		return nil, fmt.Errorf("invalid user %q: group must be a numeric gid", spec)
	}
	gid, err := parseTemplateUserID(groupPart)
	if err != nil {
		return nil, fmt.Errorf("invalid user %q: %w", spec, err)
	}
	sc.RunAsGroup = &types.Int64Value{Value: gid}
	return sc, nil
}

func validateTemplateContainerUser(overrides *types.ContainerOverrides) error {
	if overrides == nil || overrides.SecurityContext == nil {
		return nil
	}
	sc := overrides.SecurityContext
	if sc.RunAsGroup != nil && sc.RunAsUser == nil && sc.RunAsUsername == "" {
		return fmt.Errorf("container user group is set without a user")
	}
	if sc.RunAsUser != nil {
		if err := validateTemplateUserID(sc.RunAsUser.Value); err != nil {
			return fmt.Errorf("invalid run_as_user: %w", err)
		}
	}
	if sc.RunAsGroup != nil {
		if err := validateTemplateUserID(sc.RunAsGroup.Value); err != nil {
			return fmt.Errorf("invalid run_as_group: %w", err)
		}
	}
	if sc.RunAsUsername != "" {
		if err := validateTemplateUsername(sc.RunAsUsername); err != nil {
			return fmt.Errorf("invalid run_as_username: %w", err)
		}
	}
	return nil
}

func splitTemplateUser(spec string) (userPart, groupPart string, hasGroup bool, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", false, fmt.Errorf("user is empty")
	}
	userPart, groupPart, hasGroup = strings.Cut(spec, ":")
	if !hasGroup {
		return userPart, "", false, nil
	}
	if userPart == "" || groupPart == "" || strings.Contains(groupPart, ":") {
		return "", "", false, fmt.Errorf("invalid user %q: expected user or user:gid", spec)
	}
	return userPart, groupPart, true, nil
}

func isDecimalID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseTemplateUserID(s string) (int64, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("id %q is out of range", s)
	}
	return int64(v), nil
}

func validateTemplateUserID(v int64) error {
	if v < 0 || uint64(v) > 0xffffffff {
		return fmt.Errorf("id %d is out of range", v)
	}
	return nil
}

func validateTemplateUsername(name string) error {
	if name == "" || len(name) > 32 {
		return fmt.Errorf("username must be 1-32 characters")
	}
	for i, r := range name {
		if i == 0 && !unicode.IsLetter(r) && r != '_' {
			return fmt.Errorf("username %q must start with a letter or underscore", name)
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' && r != '-' {
			return fmt.Errorf("username %q has an unsupported character", name)
		}
	}
	return nil
}
