// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	volpkg "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	volcontent "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/content"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/volumecontent"
)

const (
	envVolumeContentAddr      = "CUBE_VOLUME_CONTENT_ADDR"
	envVolumeContentTLSCert   = "CUBE_VOLUME_CONTENT_TLS_CERT"
	envVolumeContentTLSKey    = "CUBE_VOLUME_CONTENT_TLS_KEY"
	envVolumeContentMaxUpload = "CUBE_VOLUME_CONTENT_MAX_UPLOAD_BYTES"
)

var startVolumeContentOnce sync.Once

// cubeMasterHTTPAddr returns the CubeMaster HTTP address from the node config,
// with the same default Cubelet uses for its other CubeMaster calls. It is read
// per use so it reflects the configuration loaded after plugin init.
func cubeMasterHTTPAddr() string {
	if c := config.GetConfig(); c != nil && c.MetaServerConfig != nil {
		if addr := strings.TrimSpace(c.MetaServerConfig.CubeMasterHTTPAddr); addr != "" {
			return addr
		}
	}
	return "127.0.0.1:8089"
}

// startVolumeContentServer serves the E2B /volumecontent protocol from this
// node. It is off unless CUBE_VOLUME_CONTENT_ADDR is set, and it refuses to
// start without a valid token configuration so it can never run unauthenticated.
// Bearer tokens travel on this listener: bind it to a trusted interface and
// terminate TLS on it (CUBE_VOLUME_CONTENT_TLS_*) or in a fronting proxy.
func startVolumeContentServer(cfg *Config) {
	addr := strings.TrimSpace(os.Getenv(envVolumeContentAddr))
	if addr == "" {
		return
	}
	startVolumeContentOnce.Do(func() {
		tokens, err := volumecontent.ConfigFromEnv(os.Getenv)
		if err == nil && tokens == nil {
			err = errors.New(volumecontent.EnvSecret + " is not set")
		}
		if err != nil {
			CubeLog.Errorf("[volume_content] disabled: %v", err)
			return
		}

		baseDir := defaultVolumePluginBaseDir
		if cfg != nil && cfg.VolumePluginBaseDir != "" {
			baseDir = filepath.Clean(cfg.VolumePluginBaseDir)
		}
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			CubeLog.Errorf("[volume_content] disabled: create base dir %s: %v", baseDir, err)
			return
		}

		maxUpload := int64(volcontent.DefaultMaxUpload)
		if raw := strings.TrimSpace(os.Getenv(envVolumeContentMaxUpload)); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				CubeLog.Errorf("[volume_content] disabled: invalid %s", envVolumeContentMaxUpload)
				return
			}
			maxUpload = n
		}

		certFile, keyFile := os.Getenv(envVolumeContentTLSCert), os.Getenv(envVolumeContentTLSKey)
		if (certFile == "") != (keyFile == "") {
			CubeLog.Errorf("[volume_content] disabled: %s and %s must be set together",
				envVolumeContentTLSCert, envVolumeContentTLSKey)
			return
		}

		refs := volcontent.NewMasterRefs(cubeMasterHTTPAddr)
		refs.Logf = CubeLog.Errorf
		handler, err := volcontent.NewHandler(volcontent.Options{
			Tokens: tokens,
			Leaser: &volcontent.ManagerLeaser{
				Mgr: volpkg.Global(), Namespace: constants.CubeDefaultNamespace, BaseDir: baseDir,
			},
			Refs:      refs,
			MaxUpload: maxUpload,
			Logf:      CubeLog.Errorf,
		})
		if err != nil {
			CubeLog.Errorf("[volume_content] disabled: %v", err)
			return
		}
		srv := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		}
		go func() {
			var err error
			if certFile != "" {
				err = srv.ListenAndServeTLS(certFile, keyFile)
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				CubeLog.Errorf("[volume_content] server on %s stopped: %v", addr, err)
			}
		}()
		CubeLog.Infof("[volume_content] serving /volumecontent on %s (tls=%t, base_dir=%s)", addr, certFile != "", baseDir)
	})
}
