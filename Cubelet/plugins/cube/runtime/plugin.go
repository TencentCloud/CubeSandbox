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

package runtime

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/klog/v2"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/plugins/services/warning"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func init() {
	config := cubeconfig.DefaultRuntimeConfig()

	registry.Register(&plugin.Registration{
		Type:   constants.CubeServicePlugin,
		ID:     "runtime",
		Config: &config,
		Requires: []plugin.Type{
			plugins.WarningPlugin,
		},
		InitFn:          initCRIRuntime,
		ConfigMigration: configMigration,
	})
}

func configMigration(ctx context.Context, configVersion int, pluginConfigs map[string]interface{}) error {
	if configVersion >= version.ConfigVersion {
		return nil
	}
	original, ok := pluginConfigs[string(plugins.GRPCPlugin)+".cri"]
	if !ok {
		return nil
	}
	src, ok := original.(map[string]interface{})
	if !ok {
		return nil
	}
	// Migrate into this plugin's own registration key
	// (constants.CubeServicePlugin + ".runtime"), not containerd's built-in
	// io.containerd.cri.v1.runtime, which this plugin never reads.
	dstKey := string(constants.CubeServicePlugin) + ".runtime"
	var dst map[string]interface{}
	if updated, ok := pluginConfigs[dstKey].(map[string]interface{}); ok {
		dst = updated
	} else {
		dst = map[string]interface{}{}
	}

	migrateConfig(dst, src)
	pluginConfigs[dstKey] = dst
	return nil
}

func migrateConfig(dst, src map[string]interface{}) {
	// Keys owned by the images plugin (migrated by its own ConfigMigration) or
	// by the stream server are dropped here so the runtime config keeps only
	// runtime-relevant settings. Everything else carries over unless the
	// destination already defines it.
	for k, v := range src {
		switch k {
		case "containerd":
			continue
		case
			"sandbox_image",
			"registry",
			"image_decryption",
			"max_concurrent_downloads",
			"image_pull_progress_timeout",
			"image_pull_with_sync_fs",
			"stats_collect_period":
			continue
		case
			"disable_tcp_service",
			"stream_server_address",
			"stream_server_port",
			"stream_idle_timeout",
			"enable_tls_streaming",
			"x509_key_pair_streaming":
			continue
		default:
			if _, ok := dst[k]; !ok {
				dst[k] = v
			}
		}
	}

	containerdConf, ok := src["containerd"].(map[string]interface{})
	if !ok {
		return
	}
	newContainerdConf, ok := dst["containerd"].(map[string]interface{})
	if !ok {
		newContainerdConf = map[string]interface{}{}
	}
	for k, v := range containerdConf {
		switch k {
		case "snapshotter", "disable_snapshot_annotations", "discard_unpacked_layers":
			continue
		default:
			if _, ok := newContainerdConf[k]; !ok {
				newContainerdConf[k] = v
			}
		}
	}
	dst["containerd"] = newContainerdConf

	runtimes, ok := newContainerdConf["runtimes"].(map[string]interface{})
	if !ok {
		return
	}
	for _, v := range runtimes {
		runtimeConf, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if sandboxMode, ok := runtimeConf["sandbox_mode"]; ok {
			if _, ok := runtimeConf["sandboxer"]; !ok {
				runtimeConf["sandboxer"] = sandboxMode
				delete(runtimeConf, "sandbox_mode")
			}
		}
	}
}

func initCRIRuntime(ic *plugin.InitContext) (interface{}, error) {
	ic.Meta.Platforms = []imagespec.Platform{platforms.DefaultSpec()}
	ic.Meta.Exports = map[string]string{"CubeVersion": constants.CubeVersion}
	ctx := ic.Context
	pluginConfig := ic.Config.(*cubeconfig.RuntimeConfig)
	if warnings, err := cubeconfig.ValidateRuntimeConfig(ctx, pluginConfig); err != nil {
		return nil, fmt.Errorf("invalid plugin config: %w", err)
	} else if len(warnings) > 0 {
		ws, err := ic.GetSingle(plugins.WarningPlugin)
		if err != nil {
			return nil, err
		}
		warn := ws.(warning.Service)
		for _, w := range warnings {
			warn.Emit(ctx, w)
		}
	}

	containerdRootDir := filepath.Dir(ic.Properties[plugins.PropertyRootDir])
	rootDir := filepath.Join(containerdRootDir, "io.containerd.grpc.v1.cube")
	containerdStateDir := filepath.Dir(ic.Properties[plugins.PropertyStateDir])
	stateDir := filepath.Join(containerdStateDir, "io.containerd.grpc.v1.cube")
	c := cubeconfig.Config{
		RuntimeConfig:      *pluginConfig,
		ContainerdRootDir:  containerdRootDir,
		ContainerdEndpoint: ic.Properties[plugins.PropertyGRPCAddress],
		RootDir:            rootDir,
		StateDir:           stateDir,
	}

	cfg, _ := json.Marshal(c)
	log.G(ctx).WithFields(log.Fields{"config": string(cfg)}).Info("starting cri plugin")

	if err := setGLogLevel(); err != nil {
		return nil, fmt.Errorf("failed to set glog level: %w", err)
	}

	ociSpec, err := loadBaseOCISpecs(&c)
	if err != nil {
		return nil, fmt.Errorf("failed to create load basic oci spec: %w", err)
	}

	return &runtime{
		config:       c,
		baseOCISpecs: ociSpec,
	}, nil
}

type runtime struct {
	config cubeconfig.Config

	baseOCISpecs map[string]*oci.Spec
}

func (r *runtime) Config() cubeconfig.Config {
	return r.config
}

func (r *runtime) LoadOCISpec(filename string) (*oci.Spec, error) {
	spec, ok := r.baseOCISpecs[filename]
	if !ok {

		return nil, errdefs.ErrNotFound
	}
	return spec, nil
}

func loadBaseOCISpecs(config *cubeconfig.Config) (map[string]*oci.Spec, error) {
	specs := map[string]*oci.Spec{}
	for _, cfg := range config.Runtimes {
		if cfg.BaseRuntimeSpec == "" {
			continue
		}

		if _, ok := specs[cfg.BaseRuntimeSpec]; ok {
			continue
		}

		spec, err := loadOCISpec(cfg.BaseRuntimeSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to load base OCI spec from file: %s: %w", cfg.BaseRuntimeSpec, err)
		}

		specs[cfg.BaseRuntimeSpec] = spec
	}

	return specs, nil
}

func loadOCISpec(filename string) (*oci.Spec, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open base OCI spec: %s: %w", filename, err)
	}
	defer file.Close()

	spec := oci.Spec{}
	if err := json.NewDecoder(file).Decode(&spec); err != nil {
		return nil, fmt.Errorf("failed to parse base OCI spec file: %w", err)
	}

	return &spec, nil
}

func setGLogLevel() error {
	l := log.GetLevel()
	fs := flag.NewFlagSet("klog", flag.PanicOnError)
	klog.InitFlags(fs)
	if err := fs.Set("logtostderr", "true"); err != nil {
		return err
	}
	switch l {
	case log.TraceLevel:
		return fs.Set("v", "5")
	case log.DebugLevel:
		return fs.Set("v", "4")
	case log.InfoLevel:
		return fs.Set("v", "2")
	default:

	}
	return nil
}
