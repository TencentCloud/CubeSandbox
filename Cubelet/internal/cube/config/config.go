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

package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/containerd/log"
	"github.com/pelletier/go-toml/v2"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubelet/pkg/cri/streaming"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	"github.com/containerd/containerd/v2/pkg/deprecation"
	"github.com/containerd/containerd/v2/plugins"
)

const (
	defaultImagePullProgressTimeoutDuration = 5 * time.Minute
)

type SandboxControllerMode string

const (
	ModePodSandbox SandboxControllerMode = "podsandbox"

	ModeShim SandboxControllerMode = "shim"

	DefaultSandboxImage = "registry.k8s.io/pause:3.10"

	IOTypeFifo = "fifo"

	IOTypeStreaming = "streaming"
)

type Runtime struct {
	Type string `toml:"runtime_type" json:"runtimeType"`

	Path string `toml:"runtime_path" json:"runtimePath"`

	ConfigPath string `toml:"cfg_path" json:"cfgPath"`

	PodAnnotations []string `toml:"pod_annotations" json:"PodAnnotations"`

	ContainerAnnotations []string `toml:"container_annotations" json:"ContainerAnnotations"`

	Options map[string]interface{} `toml:"options" json:"options"`

	PrivilegedWithoutHostDevices bool `toml:"privileged_without_host_devices" json:"privileged_without_host_devices"`

	PrivilegedWithoutHostDevicesAllDevicesAllowed bool `toml:"privileged_without_host_devices_all_devices_allowed" json:"privileged_without_host_devices_all_devices_allowed"`

	BaseRuntimeSpec string `toml:"base_runtime_spec" json:"baseRuntimeSpec"`

	NetworkPluginConfDir string `toml:"cni_conf_dir" json:"cniConfDir"`

	NetworkPluginMaxConfNum int `toml:"cni_max_conf_num" json:"cniMaxConfNum"`

	Snapshotter string `toml:"snapshotter" json:"snapshotter"`

	Sandboxer string `toml:"sandboxer" json:"sandboxer"`

	IOType string `toml:"io_type" json:"io_type"`
}

type ContainerdConfig struct {
	DefaultRuntimeName string `toml:"default_runtime_name" json:"defaultRuntimeName"`

	Runtimes map[string]Runtime `toml:"runtimes" json:"runtimes"`

	IgnoreBlockIONotEnabledErrors bool `toml:"ignore_blockio_not_enabled_errors" json:"ignoreBlockIONotEnabledErrors"`

	IgnoreRdtNotEnabledErrors bool `toml:"ignore_rdt_not_enabled_errors" json:"ignoreRdtNotEnabledErrors"`
}

type CniConfig struct {
	NetworkPluginBinDir string `toml:"bin_dir" json:"binDir"`

	NetworkPluginConfDir string `toml:"conf_dir" json:"confDir"`

	NetworkPluginMaxConfNum int `toml:"max_conf_num" json:"maxConfNum"`

	NetworkPluginSetupSerially bool `toml:"setup_serially" json:"setupSerially"`

	NetworkPluginConfTemplate string `toml:"conf_template" json:"confTemplate"`

	IPPreference string `toml:"ip_pref" json:"ipPref"`

	UseInternalLoopback bool `toml:"use_internal_loopback" json:"useInternalLoopback"`
}

type Mirror struct {
	Endpoints []string `toml:"endpoint" json:"endpoint"`
}

type AuthConfig struct {
	Username string `toml:"username" json:"username"`

	Password string `toml:"password" json:"password"`

	Auth string `toml:"auth" json:"auth"`

	IdentityToken string `toml:"identitytoken" json:"identitytoken"`
}

type Registry struct {
	ConfigPath string `toml:"config_path" json:"configPath"`

	Mirrors map[string]Mirror `toml:"mirrors" json:"mirrors"`

	Configs map[string]RegistryConfig `toml:"configs" json:"configs"`

	Auths map[string]AuthConfig `toml:"auths" json:"auths"`

	Headers map[string][]string `toml:"headers" json:"headers"`
}

type RegistryConfig struct {
	Auth *AuthConfig `toml:"auth" json:"auth"`
}

type ImageDecryption struct {
	KeyModel string `toml:"key_model" json:"keyModel"`
}

type ImagePlatform struct {
	Platform string `toml:"platform" json:"platform"`

	Snapshotter string `toml:"snapshotter" json:"snapshotter"`
}

type ImageConfig struct {
	Snapshotter string `toml:"snapshotter" json:"snapshotter"`

	DisableSnapshotAnnotations bool `toml:"disable_snapshot_annotations" json:"disableSnapshotAnnotations"`

	DiscardUnpackedLayers bool `toml:"discard_unpacked_layers" json:"discardUnpackedLayers"`

	PinnedImages map[string]string `toml:"pinned_images" json:"pinned_images"`

	RuntimePlatforms map[string]ImagePlatform `toml:"runtime_platforms" json:"runtimePlatforms"`

	Registry Registry `toml:"registry" json:"registry"`

	ImageDecryption `toml:"image_decryption" json:"imageDecryption"`

	MaxConcurrentDownloads int `toml:"max_concurrent_downloads" json:"maxConcurrentDownloads"`

	ImagePullProgressTimeout string `toml:"image_pull_progress_timeout" json:"imagePullProgressTimeout"`

	ImagePullWithSyncFs bool `toml:"image_pull_with_sync_fs" json:"imagePullWithSyncFs"`

	StatsCollectPeriod int `toml:"stats_collect_period" json:"statsCollectPeriod"`
}

type RuntimeConfig struct {
	ContainerdConfig `toml:"containerd" json:"containerd"`

	CniConfig `toml:"cni" json:"cni"`

	EnableSelinux bool `toml:"enable_selinux" json:"enableSelinux"`

	SelinuxCategoryRange int `toml:"selinux_category_range" json:"selinuxCategoryRange"`

	MaxContainerLogLineSize int `toml:"max_container_log_line_size" json:"maxContainerLogSize"`

	DisableApparmor bool `toml:"disable_apparmor" json:"disableApparmor"`

	RestrictOOMScoreAdj bool `toml:"restrict_oom_score_adj" json:"restrictOOMScoreAdj"`

	DisableProcMount bool `toml:"disable_proc_mount" json:"disableProcMount"`

	UnsetSeccompProfile string `toml:"unset_seccomp_profile" json:"unsetSeccompProfile"`

	TolerateMissingHugetlbController bool `toml:"tolerate_missing_hugetlb_controller" json:"tolerateMissingHugetlbController"`

	DisableHugetlbController bool `toml:"disable_hugetlb_controller" json:"disableHugetlbController"`

	DeviceOwnershipFromSecurityContext bool `toml:"device_ownership_from_security_context" json:"device_ownership_from_security_context"`

	IgnoreImageDefinedVolumes bool `toml:"ignore_image_defined_volumes" json:"ignoreImageDefinedVolumes"`

	NetNSMountsUnderStateDir bool `toml:"netns_mounts_under_state_dir" json:"netnsMountsUnderStateDir"`

	EnableUnprivilegedPorts bool `toml:"enable_unprivileged_ports" json:"enableUnprivilegedPorts"`

	EnableUnprivilegedICMP bool `toml:"enable_unprivileged_icmp" json:"enableUnprivilegedICMP"`

	EnableCDI bool `toml:"enable_cdi" json:"enableCDI"`

	CDISpecDirs []string `toml:"cdi_spec_dirs" json:"cdiSpecDirs"`

	DrainExecSyncIOTimeout string `toml:"drain_exec_sync_io_timeout" json:"drainExecSyncIOTimeout"`

	IgnoreDeprecationWarnings []string `toml:"ignore_deprecation_warnings" json:"ignoreDeprecationWarnings"`
}

type X509KeyPairStreaming struct {
	TLSCertFile string `toml:"tls_cert_file" json:"tlsCertFile"`

	TLSKeyFile string `toml:"tls_key_file" json:"tlsKeyFile"`
}

type Config struct {
	RuntimeConfig

	ContainerdRootDir string `json:"containerdRootDir"`

	ContainerdEndpoint string `json:"containerdEndpoint"`

	RootDir string `json:"rootDir"`

	StateDir string `json:"stateDir"`
}

type ServerConfig struct {
	DisableTCPService bool `toml:"disable_tcp_service" json:"disableTCPService"`

	StreamServerAddress string `toml:"stream_server_address" json:"streamServerAddress"`

	StreamServerPort string `toml:"stream_server_port" json:"streamServerPort"`

	StreamIdleTimeout string `toml:"stream_idle_timeout" json:"streamIdleTimeout"`

	EnableTLSStreaming bool `toml:"enable_tls_streaming" json:"enableTLSStreaming"`

	X509KeyPairStreaming `toml:"x509_key_pair_streaming" json:"x509KeyPairStreaming"`
}

const (
	RuntimeUntrusted = "untrusted"

	RuntimeDefault = "default"

	KeyModelNode = "node"
)

func ValidateImageConfig(ctx context.Context, c *ImageConfig) ([]deprecation.Warning, error) {
	var warnings []deprecation.Warning

	useConfigPath := c.Registry.ConfigPath != ""
	if len(c.Registry.Mirrors) > 0 {
		if useConfigPath {
			return warnings, errors.New("`mirrors` cannot be set when `config_path` is provided")
		}
		warnings = append(warnings, deprecation.CRIRegistryMirrors)
		log.G(ctx).Warning("`mirrors` is deprecated, please use `config_path` instead")
	}

	if len(c.Registry.Configs) != 0 {
		warnings = append(warnings, deprecation.CRIRegistryConfigs)
		log.G(ctx).Warning("`configs` is deprecated, please use `config_path` instead")
	}

	if len(c.Registry.Auths) != 0 {
		if c.Registry.Configs == nil {
			c.Registry.Configs = make(map[string]RegistryConfig)
		}
		for endpoint, auth := range c.Registry.Auths {
			auth := auth
			u, err := url.Parse(endpoint)
			if err != nil {
				return warnings, fmt.Errorf("failed to parse registry url %q from `registry.auths`: %w", endpoint, err)
			}
			if u.Scheme != "" {

				endpoint = u.Host
			}
			config := c.Registry.Configs[endpoint]
			config.Auth = &auth
			c.Registry.Configs[endpoint] = config
		}
		warnings = append(warnings, deprecation.CRIRegistryAuths)
		log.G(ctx).Warning("`auths` is deprecated, please use `ImagePullSecrets` instead")
	}

	if c.ImagePullProgressTimeout != "" {
		if _, err := time.ParseDuration(c.ImagePullProgressTimeout); err != nil {
			return warnings, fmt.Errorf("invalid image pull progress timeout: %w", err)
		}
	}

	return warnings, nil
}

func ValidateRuntimeConfig(ctx context.Context, c *RuntimeConfig) ([]deprecation.Warning, error) {
	var warnings []deprecation.Warning
	if c.ContainerdConfig.Runtimes == nil {
		c.ContainerdConfig.Runtimes = make(map[string]Runtime)
	}

	if c.ContainerdConfig.DefaultRuntimeName == "" {
		return warnings, errors.New("`default_runtime_name` is empty")
	}
	if _, ok := c.ContainerdConfig.Runtimes[c.ContainerdConfig.DefaultRuntimeName]; !ok {
		return warnings, fmt.Errorf("no corresponding runtime configured in `containerd.runtimes` for `containerd` `default_runtime_name = \"%s\"", c.ContainerdConfig.DefaultRuntimeName)
	}

	for k, r := range c.ContainerdConfig.Runtimes {
		if !r.PrivilegedWithoutHostDevices && r.PrivilegedWithoutHostDevicesAllDevicesAllowed {
			return warnings, errors.New("`privileged_without_host_devices_all_devices_allowed` requires `privileged_without_host_devices` to be enabled")
		}

		if len(r.Sandboxer) == 0 {
			r.Sandboxer = string(ModePodSandbox)
			c.ContainerdConfig.Runtimes[k] = r
		}

		if len(r.IOType) == 0 {
			r.IOType = IOTypeFifo
		}
		if r.IOType != IOTypeStreaming && r.IOType != IOTypeFifo {
			return warnings, errors.New("`io_type` can only be `streaming` or `named_pipe`")
		}
	}

	if c.DrainExecSyncIOTimeout != "" {
		if _, err := time.ParseDuration(c.DrainExecSyncIOTimeout); err != nil {
			return warnings, fmt.Errorf("invalid `drain_exec_sync_io_timeout`: %w", err)
		}
	}
	if err := ValidateEnableUnprivileged(ctx, c); err != nil {
		return warnings, err
	}
	return warnings, nil
}

func ValidateServerConfig(ctx context.Context, c *ServerConfig) ([]deprecation.Warning, error) {
	var warnings []deprecation.Warning

	if c.StreamIdleTimeout != "" {
		if _, err := time.ParseDuration(c.StreamIdleTimeout); err != nil {
			return warnings, fmt.Errorf("invalid stream idle timeout: %w", err)
		}
	}
	return warnings, nil
}

func hostAccessingSandbox(config *runtime.PodSandboxConfig) bool {
	securityContext := config.GetLinux().GetSecurityContext()

	namespaceOptions := securityContext.GetNamespaceOptions()
	if namespaceOptions.GetNetwork() == runtime.NamespaceMode_NODE ||
		namespaceOptions.GetPid() == runtime.NamespaceMode_NODE ||
		namespaceOptions.GetIpc() == runtime.NamespaceMode_NODE {
		return true
	}

	return false
}

func GenerateRuntimeOptions(r Runtime) (interface{}, error) {
	if r.Options == nil {
		return nil, nil
	}

	b, err := toml.Marshal(r.Options)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TOML blob for runtime %q: %w", r.Type, err)
	}

	options := getRuntimeOptionsType(r.Type)
	if err := toml.Unmarshal(b, options); err != nil {
		return nil, err
	}

	if runtimeOpts, ok := options.(*runtimeoptions.Options); ok && runtimeOpts.ConfigPath == "" {
		runtimeOpts.ConfigBody = b
	}

	return options, nil
}

func getRuntimeOptionsType(t string) interface{} {
	switch t {
	case plugins.RuntimeRuncV2:
		return &runcoptions.Options{}
	case plugins.RuntimeRunhcsV1:
		return &runhcsoptions.Options{}
	default:
		return &runtimeoptions.Options{}
	}
}

func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		DisableTCPService:   true,
		StreamServerAddress: "127.0.0.1",
		StreamServerPort:    "0",
		StreamIdleTimeout:   streaming.DefaultConfig.StreamIdleTimeout.String(),
		EnableTLSStreaming:  false,
		X509KeyPairStreaming: X509KeyPairStreaming{
			TLSKeyFile:  "",
			TLSCertFile: "",
		},
	}
}
