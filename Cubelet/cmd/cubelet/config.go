// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"io"
	"os"
	"path/filepath"

	containerdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
	containerdconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/timeout"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/plugin/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pelletier/go-toml"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
	"github.com/urfave/cli/v2"
	"golang.org/x/net/context"
)

const unixSockPath = "/data/cubelet/cubelet.sock"

type Config struct {
	*srvconfig.Config

	Plugins map[string]interface{} `toml:"plugins"`
}

func (c *Config) WriteTo(w io.Writer) (int64, error) {
	return 0, toml.NewEncoder(w).Encode(c)
}

func outputConfig(ctx context.Context, cfg *srvconfig.Config) error {
	config := &Config{
		Config: cfg,
	}

	plugins, err := containerdserver.LoadPlugins(ctx, config.Config.Config)
	if err != nil {
		return err
	}
	if len(plugins) != 0 {
		config.Plugins = make(map[string]interface{})
		for _, p := range plugins {
			if p.Config == nil {
				continue
			}

			pc, err := config.Decode(ctx, p.URI(), p.Config)
			if err != nil {
				return err
			}

			config.Plugins[p.URI()] = pc
		}
	}

	if config.Timeouts == nil {
		config.Timeouts = make(map[string]string)
	}
	timeouts := timeout.All()
	for k, v := range timeouts {
		if config.Timeouts[k] == "" {
			config.Timeouts[k] = v.String()
		}
	}

	config.Config.Version = 2

	config.Config.Plugins = nil

	_, err = config.WriteTo(os.Stdout)
	return err
}

var configCommand = &cli.Command{
	Name:  "config",
	Usage: "Information on the containerd config",
	Subcommands: []*cli.Command{
		{
			Name:  "default",
			Usage: "See the output of the default config",
			Action: func(cliContext *cli.Context) error {
				ctx := cliContext.Context
				return outputConfig(ctx, defaultConfig())
			},
		},
		{
			Name:   "dump",
			Usage:  "See the output of the final main config with imported in subconfig files",
			Action: dumpConfig,
		},
		{
			Name:  "migrate",
			Usage: "Migrate the current configuration file to the latest version (does not migrate subconfig files)",

			Action: dumpConfig,
		},
	},
}

func defaultConfig() *srvconfig.Config {
	return platformAgnosticDefaultConfig()
}

func dumpConfig(cliContext *cli.Context) error {
	config := defaultConfig()
	ctx := cliContext.Context
	if err := srvconfig.LoadConfig(ctx, cliContext.String("config"), config); err != nil && !os.IsNotExist(err) {
		return err
	}

	if config.Version < version.ConfigVersion {
		plugins := registry.Graph(containerdconfig.V2DisabledFilter(config.DisabledPlugins))
		for _, p := range plugins {
			if p.ConfigMigration != nil {
				if err := p.ConfigMigration(ctx, config.Version, config.Plugins); err != nil {
					return err
				}
			}
		}
	}
	return outputConfig(ctx, config)
}

func platformAgnosticDefaultConfig() *srvconfig.Config {
	baseConfig := &srvconfig.Config{
		Config: &containerdconfig.Config{
			Version: version.ConfigVersion,
			Root:    "/data/cubelet/root",
			State:   "/data/cubelet/state",
			GRPC: containerdconfig.GRPCConfig{
				Address:        "/data/cubelet/cubelet.sock",
				MaxRecvMsgSize: defaults.DefaultMaxRecvMsgSize,
				MaxSendMsgSize: defaults.DefaultMaxSendMsgSize,
			},
			DisabledPlugins:  []string{},
			RequiredPlugins:  []string{},
			StreamProcessors: streamProcessors(),
		},
		CubeTap: srvconfig.CubeTapConfig{
			Address: unixSockPath,
		},
		PidFile:           "/run/cube-let.pid",
		DynamicConfigPath: "/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml",
		CubeLog: srvconfig.CubeLogConfig{
			Path:     srvconfig.DefaultCubeLogPath,
			FileNum:  srvconfig.DefaultCubeLogFileNum,
			FileSize: srvconfig.DefaultCubeLogFileSize,
		},
	}
	return baseConfig
}

func streamProcessors() map[string]containerdconfig.StreamProcessor {
	const (
		ctdDecoder = "ctd-decoder"
		basename   = "io.containerd.ocicrypt.decoder.v1"
	)
	decryptionKeysPath := filepath.Join(defaults.DefaultConfigDir, "ocicrypt", "keys")
	ctdDecoderArgs := []string{
		"--decryption-keys-path", decryptionKeysPath,
	}
	ctdDecoderEnv := []string{
		"OCICRYPT_KEYPROVIDER_CONFIG=" + filepath.Join(defaults.DefaultConfigDir, "ocicrypt", "ocicrypt_keyprovider.conf"),
	}
	return map[string]containerdconfig.StreamProcessor{
		basename + ".tar.gzip": {
			Accepts: []string{images.MediaTypeImageLayerGzipEncrypted},
			Returns: ocispec.MediaTypeImageLayerGzip,
			Path:    ctdDecoder,
			Args:    ctdDecoderArgs,
			Env:     ctdDecoderEnv,
		},
		basename + ".tar": {
			Accepts: []string{images.MediaTypeImageLayerEncrypted},
			Returns: ocispec.MediaTypeImageLayer,
			Path:    ctdDecoder,
			Args:    ctdDecoderArgs,
			Env:     ctdDecoderEnv,
		},
	}
}
