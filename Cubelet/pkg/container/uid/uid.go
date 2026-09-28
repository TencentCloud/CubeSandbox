// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package uid

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/pkg/oci"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"

	customopts "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/opts"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const (
	defaultUID = 0
	defaultGID = 0
)

func GenOpt(ctx context.Context, c *cubebox.ContainerConfig, imageConfig *imagespec.ImageConfig) []oci.SpecOpts {
	var specOpts []oci.SpecOpts
	securityContext := c.GetSecurityContext()
	if securityContext != nil {

		userstr, err := generateUserString(
			securityContext.GetRunAsUsername(),
			securityContext.GetRunAsUser(),
			securityContext.GetRunAsGroup())
		if err != nil {
			return nil
		}
		if userstr == "" {
			userstr = imageConfig.User
		}
		if userstr != "" {
			specOpts = append(specOpts, oci.WithUser(userstr))
		}

		userstr = "0"
		if securityContext.GetRunAsUsername() != "" {
			userstr = securityContext.GetRunAsUsername()
		} else if securityContext.GetRunAsUser() != nil {
			userstr = strconv.FormatInt(securityContext.GetRunAsUser().GetValue(), 10)
		} else if imageConfig.User != "" {
			parts := strings.Split(imageConfig.User, ":")
			userstr = parts[0]
		}
		specOpts = append(specOpts, customopts.WithAdditionalGIDs(userstr))

		return specOpts
	}
	return append(specOpts, oci.WithUIDGID(defaultUID, defaultGID))
}

func generateUserString(username string, uid, gid *cubebox.Int64Value) (string, error) {
	var userstr, groupstr string
	if uid != nil {
		userstr = strconv.FormatInt(uid.GetValue(), 10)
	}
	if username != "" {
		userstr = username
	}
	if gid != nil {
		groupstr = strconv.FormatInt(gid.GetValue(), 10)
	}
	if userstr == "" {
		if groupstr != "" {
			return "", fmt.Errorf("user group %q is specified without user", groupstr)
		}
		return "", nil
	}
	if groupstr != "" {
		userstr = userstr + ":" + groupstr
	}
	return userstr, nil
}
