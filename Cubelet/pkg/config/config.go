// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package config provides the configuration for the cubelet
package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/hotswap"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"k8s.io/apimachinery/pkg/api/resource"
)

var cfg *Config

var networkAgentOverride struct {
	enable   bool
	endpoint string
	set      bool
}

type MetaServerConfig struct {
	MetaServerEndpoint  string `yaml:"meta_server_endpoint,omitempty"`
	NodeStatusMaxImages int32  `yaml:"node_status_max_images,omitempty"`
}

type Config struct {
	Common           *CommonConf       `yaml:"common"`
	Tenant           *TenantManager    `yaml:"tenant"`
	MetaServerConfig *MetaServerConfig `yaml:"meta_server_config"`
	HostConf         *HostConf         `yaml:"host"`
}

type HostConf struct {
	Quota          HostConfigQuota `yaml:"quota"`
	GC             HostConfigGC    `yaml:"gc"`
	SchedulerLabel string          `yaml:"scheduler_label"`
}

type HostConfigQuota struct {
	Cpu                   int    `yaml:"mcpu_limit"`
	Mem                   string `yaml:"mem_limit"`
	MvmLimit              int    `yaml:"mvm_limit"`
	CreationConcurrentNum int    `yaml:"creation_concurrent_num"`

	// PausedResourceReleaseRatio is the paused-resource scheduling dial.
	// Pausing a sandbox snapshots it to disk and shuts the MicroVM down, so its
	// host CPU/RAM is already reclaimed; this ratio decides how much of that
	// quota is released back to the scheduler vs kept reserved as resume
	// headroom. It is in [0,1] (out-of-range values are clamped) and is applied
	// symmetrically to CPU and memory:
	//   0.0 (default) -- release nothing: paused sandboxes keep their full
	//                    quota, so resume is guaranteed. Identical to the legacy
	//                    behaviour (the safe zero-value default).
	//   1.0           -- release everything: maximum density, resume is purely
	//                    best-effort and rejected when the node can no longer
	//                    fit the sandbox (see UpdateWithResume admission).
	//   0 < r < 1     -- release a fraction r and reserve (1-r) as headroom. A
	//                    pause-heavy node then (a) keeps room for its own
	//                    resumes and (b), because the reserved quota still shows
	//                    up in QuotaCpuUsage/QuotaMemUsage, is naturally
	//                    deprioritised by the cpu/mem quota scoring factors, so
	//                    the scheduler stops piling new sandboxes onto nodes
	//                    that already hold many paused ones.
	// Disk is intentionally still counted (the pause snapshot occupies storage)
	// and paused sandboxes still count toward MvmNum, regardless of the ratio.
	PausedResourceReleaseRatio float64 `yaml:"paused_resource_release_ratio"`
}

type HostConfigGC struct {
	CodeExpirationTime  string `yaml:"code_expiration_time"`
	ImageExpirationTime string `yaml:"image_expiration_time"`
}

type CommonConf struct {
	CommonTimeout         time.Duration `yaml:"common_timeout"`
	LogLevel              string        `yaml:"log_level"`
	EnableNetworkAgent    bool          `yaml:"enable_network_agent"`
	NetworkAgentEndpoint  string        `yaml:"network_agent_endpoint"`
	DescribeAsyncInterval time.Duration `yaml:"describe_asynchronous"`
	EnablePFMode          bool          `yaml:"enable_pf_mode"`
	DescribeBDFInterval   time.Duration `yaml:"describe_bdf"`
	DescribeBDFTimeout    time.Duration `yaml:"describe_bdf_timeout"`
	GetBDFByUuidCmd       string        `yaml:"get_bdf_by_uuid_cmd"`
	GetBDFByIfNameCmd     string        `yaml:"get_bdf_by_ifname_cmd"`
	CommandTimeout        time.Duration `yaml:"command_timeout"`

	DisableHostCgroup bool `yaml:"disable_host_cgroup"`

	DisableVmCgroup bool `yaml:"disable_vm_cgroup"`

	EnableSandboxExecCmdBeforeExist bool          `yaml:"enable_sandbox_exec_cmd_before_exist"`
	SandboxExecCmdTimeOut           time.Duration `yaml:"sandbox_exec_cmd_time_out"`
	SandboxExecCmdBeforeExist       []string      `yaml:"sandbox_exec_cmd_before_exist"`
	SandboxExecCmdBeforeExistLogOut bool          `yaml:"sandbox_exec_cmd_before_exist_log_out"`
	SandboxExecCmdOutMaxLines       int           `yaml:"sandbox_exec_cmd_out_max_lines"`
	SandboxExecCmdMatchLine         string        `yaml:"sandbox_exec_cmd_match_line"`
	SandboxExecCmdAfterMatch        []string      `yaml:"sandbox_exec_cmd_after_match"`
	SandboxExecCmdBase64AfterMatch  string        `yaml:"sandbox_exec_base64_cmd_after_match"`
	SandboxExecCmdAfterMatchLogOut  bool          `yaml:"sandbox_exec_cmd_after_match_log_out"`

	CgroupDisableMemoryReparentFile string `yaml:"cgroup_disable_memory_reparent_file"`
	CgroupDisableCpusetList         string `yaml:"cgroup_disable_cpuset_list"`

	DisableHostNetfile bool `yaml:"disable_host_netfile"`

	DefaultDNSServers []string      `yaml:"default_dns_servers"`
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`

	DisableCubeBoxTemplateBaseFormatPoolOfNumberVer bool `yaml:"disable_cube_box_template_base_format_pool_of_number_ver"`
}

func Init(configPath string, useDefault bool) (*Config, error) {
	var data interface{}
	watcher, err := hotswap.NewWatcher(configPath, 10, &Config{})
	if err != nil {
		return nil, err
	}
	watcher.AppendWatcher(&listener{})
	data, err = watcher.Init()
	if err != nil {

		if os.IsNotExist(err) && useDefault {
			fmt.Printf("%s\n", fmt.Errorf("[warn]config file not exist:%w", err))
			data = &Config{}
		} else {
			return nil, err
		}
	}
	newCfg, err := preHandle(data.(*Config))
	if err != nil {
		return nil, fmt.Errorf("preHandle config fail:%v", err)
	}
	err = validate(newCfg)
	if err != nil {
		return nil, fmt.Errorf("validate config fail:%v", err)
	}
	cfg = newCfg
	fmt.Printf("cfg:%+v\n", utils.InterfaceToString(cfg))
	return newCfg, nil
}

func SetNetworkAgentOverride(enable bool, endpoint string) {
	networkAgentOverride.enable = enable
	networkAgentOverride.endpoint = endpoint
	networkAgentOverride.set = true
}

func validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if cfg.HostConf != nil {
		if cfg.HostConf.Quota.Mem != "" {
			if _, err := resource.ParseQuantity(cfg.HostConf.Quota.Mem); err != nil {
				return fmt.Errorf("invalid host.quota.mem_limit: %w", err)
			}
		}
		if cfg.HostConf.Quota.CreationConcurrentNum < 0 {
			return fmt.Errorf("invalid host.quota.creation_concurrent_num: must be >= 0")
		}
		if cfg.HostConf.GC.CodeExpirationTime != "" {
			t, err := time.ParseDuration(cfg.HostConf.GC.CodeExpirationTime)
			if err != nil || t <= 0 {
				return fmt.Errorf("invalid host.gc.code_expiration_time")
			}
		}
		if cfg.HostConf.GC.ImageExpirationTime != "" {
			t, err := time.ParseDuration(cfg.HostConf.GC.ImageExpirationTime)
			if err != nil || t <= 0 {
				return fmt.Errorf("invalid host.gc.image_expiration_time")
			}
		}
	}
	if cfg.Common != nil {
		for _, dns := range cfg.Common.DefaultDNSServers {
			if net.ParseIP(dns) == nil {
				return fmt.Errorf("invalid common.default_dns_servers entry: %q", dns)
			}
		}
	}
	return nil
}

func preHandle(config *Config) (*Config, error) {
	if config == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if config.Common == nil {
		config.Common = &CommonConf{}
	}

	if networkAgentOverride.set {
		config.Common.EnableNetworkAgent = networkAgentOverride.enable
		if networkAgentOverride.endpoint != "" {
			config.Common.NetworkAgentEndpoint = networkAgentOverride.endpoint
		}
	}

	if config.Common.NetworkAgentEndpoint == "" {
		config.Common.NetworkAgentEndpoint = "grpc+unix:///run/cube/network-agent-grpc.sock"
	}
	if config.HostConf == nil {
		config.HostConf = &HostConf{}
	}
	if config.HostConf.SchedulerLabel == "" {
		config.HostConf.SchedulerLabel = "default-cluster"
	}
	if config.HostConf.GC.CodeExpirationTime == "" {
		config.HostConf.GC.CodeExpirationTime = "72h"
	}
	if config.HostConf.GC.ImageExpirationTime == "" {
		config.HostConf.GC.ImageExpirationTime = "24h"
	}
	if config.Common.CommonTimeout == time.Duration(0) {
		config.Common.CommonTimeout = 10 * time.Second
	}
	if config.Common.DescribeAsyncInterval == time.Duration(0) {
		config.Common.DescribeAsyncInterval = 100 * time.Millisecond
	}

	if config.Common.SandboxExecCmdOutMaxLines == 0 {
		config.Common.SandboxExecCmdOutMaxLines = 2000
	}

	if config.Common.SandboxExecCmdTimeOut == time.Duration(0) {
		config.Common.SandboxExecCmdTimeOut = 1 * time.Second
	}

	if config.Common.DescribeBDFInterval == time.Duration(0) {

		config.Common.DescribeBDFInterval = 10 * time.Millisecond
	}
	if config.Common.DescribeBDFTimeout == time.Duration(0) {
		config.Common.DescribeBDFTimeout = 10 * time.Second
	}
	if config.Common.CommandTimeout == time.Duration(0) {
		config.Common.CommandTimeout = time.Second
	}

	if config.Common.GetBDFByUuidCmd == "" {
		config.Common.GetBDFByUuidCmd = "/usr/local/services/AdamPlugins-1.0/snhost_snic_sdk/cath/bm/tools/get_bdf_by_uuid"
	}

	if config.Common.GetBDFByIfNameCmd == "" {
		config.Common.GetBDFByIfNameCmd = "/usr/local/services/AdamPlugins-1.0/snhost_snic_sdk/cath/bm/tools/get_bdf_by_ifname"
	}
	if len(config.Common.DefaultDNSServers) > 0 {
		normalized := make([]string, 0, len(config.Common.DefaultDNSServers))
		for _, dns := range config.Common.DefaultDNSServers {
			dns = strings.TrimSpace(dns)
			if dns == "" {
				continue
			}
			normalized = append(normalized, dns)
		}
		config.Common.DefaultDNSServers = normalized
	}

	if config.Tenant == nil {
		config.Tenant = &TenantManager{
			Tenants:   make(map[string]*TenantConf),
			DefaultNS: namespaces.Default,
		}
	}
	if config.Tenant.DefaultNS == "" {
		config.Tenant.DefaultNS = namespaces.Default
	}
	for id, tenant := range config.Tenant.Tenants {
		if tenant.Namespace == "" {
			tenant.Namespace = id
		}
	}
	if config.Tenant.ZiyanUinListStr != "" {
		uniArr := strings.Split(config.Tenant.ZiyanUinListStr, ",")
		for _, uni := range uniArr {
			config.Tenant.Tenants[uni] = &TenantConf{
				Namespace: "ziyan",
			}
		}
	}

	if config.MetaServerConfig == nil {
		config.MetaServerConfig = &MetaServerConfig{}
	}
	if config.MetaServerConfig.MetaServerEndpoint == "" {
		config.MetaServerConfig.MetaServerEndpoint = "cube-meta-server.cube.com"
	}
	if config.MetaServerConfig.NodeStatusMaxImages == 0 {
		config.MetaServerConfig.NodeStatusMaxImages = 40000
	}

	if config.Common.ReconcileInterval == 0 {
		config.Common.ReconcileInterval = time.Minute * 5
	}
	return config, nil
}

//go:noinline
func GetConfig() *Config {
	return cfg
}

//go:noinline
func GetCommon() *CommonConf {
	return cfg.Common
}

func defaultHostConf() *HostConf {
	return &HostConf{
		SchedulerLabel: "default-cluster",
		GC: HostConfigGC{
			CodeExpirationTime:  "72h",
			ImageExpirationTime: "24h",
		},
	}
}

//go:noinline
func GetHostConf() *HostConf {
	if cfg == nil || cfg.HostConf == nil {
		return defaultHostConf()
	}
	return cfg.HostConf
}

//go:noinline
func GetPoolSizeForInit(defaultSize int) int {
	hostConf := GetHostConf()
	if hostConf != nil && hostConf.Quota.MvmLimit > 0 {
		return hostConf.Quota.MvmLimit
	}
	return defaultSize
}

type listener struct {
}

func (l *listener) OnEvent(data interface{}) {
	conf, err := preHandle(data.(*Config))
	if err != nil {
		fmt.Printf("preHandle Config:%v fail:%v\n", data, err)
		return
	}
	err = validate(conf)
	if err != nil {
		fmt.Printf("validate Config:%v fail:%v\n", data, err)
		return
	}
	cfg = conf
	fmt.Printf("cfg:%+v\n", utils.InterfaceToString(cfg))
	notify(conf)
}

func notify(config *Config) {
	for _, l := range listeners {
		l.OnEvent(config)
	}
}

type Watcher interface {
	OnEvent(data *Config)
}

var listeners []Watcher
var listenerMutex sync.RWMutex

func AppendConfigWatcher(listener Watcher) {
	listenerMutex.Lock()
	defer listenerMutex.Unlock()
	listeners = append(listeners, listener)
}

type TenantManager struct {
	Tenants                map[string]*TenantConf `yaml:"tenants"`
	UseRespectiveNamespace bool                   `yaml:"useRespectiveNamespace"`
	DefaultNS              string                 `yaml:"defaultNS"`

	ZiyanUinListStr string `yaml:"ziyanUinListStr"`
}

type TenantConf struct {
	Namespace string `toml:"namespace"`
}

func GetTenantNamespaceFromMap(annos map[string]string) string {
	_ = annos
	return cfg.Tenant.DefaultNS
}
