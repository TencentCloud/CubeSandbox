// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/volumecontent"
)

// volumeContentConfigFn resolves the volume-content token configuration from
// the environment once. A nil config means the /volumecontent data plane is
// not enabled and volume responses keep the plugin-provided token unchanged.
// Tests override it.
var volumeContentConfigFn = sync.OnceValues(func() (*volumecontent.Config, error) {
	return volumecontent.ConfigFromEnv(os.Getenv)
})

// applyVolumeContentToken replaces item.Token with a short-lived credential
// scoped to this volume, as E2B's control plane does on create and get. It is
// not applied to list responses, which never carry tokens.
func applyVolumeContentToken(item *VolumeItem, r *models.VolumeRecord) error {
	cfg, err := volumeContentConfigFn()
	if err != nil {
		return fmt.Errorf("volume content token is misconfigured: %w", err)
	}
	if cfg == nil {
		return nil
	}
	token, err := cfg.Mint(r.VolumeID, r.Driver, r.PrivateData, volumecontent.PermReadWrite, time.Now())
	if err != nil {
		return fmt.Errorf("mint volume content token: %w", err)
	}
	item.Token = token
	item.Endpoint = contentEndpoint(cfg.Audience)
	return nil
}

// contentEndpoint returns the token audience as the content service endpoint
// when it is a plain http(s) origin. The audience is the service's own origin
// (Cubelet verifies against it), so no separate endpoint setting is needed.
func contentEndpoint(audience string) string {
	u, err := url.Parse(audience)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}
