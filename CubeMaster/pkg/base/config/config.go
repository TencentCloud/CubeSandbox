// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package config provides the configuration for the cube master
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/hotswap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	volumeplugin "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"k8s.io/apimachinery/pkg/api/resource"
)

// cfg 全局配置实例（进程内单例，通过 GetConfig 读取）
var cfg *Config

// Config 全局配置根结构：由 YAML 配置文件加载，包含 master 各模块的配置块。
// 支持热更新：文件变化时自动重载（hotswap watcher + listener）
type Config struct {
	Common           *CommonConf           `yaml:"common"`             // 通用配置（HTTP 端口、同步周期、超时等）
	AuthConf         *AuthConf             `yaml:"auth"`               // 接口鉴权配置
	Log              *log.Conf             `yaml:"log"`                // 日志配置
	CubeletConf      *CubeletConf          `yaml:"cubelet_conf"`       // 与 cubelet 交互相关（超时、重试、并发限制、缓冲队列）
	InstanceDBConfig *DBConfig             `yaml:"instance_db_config"` // 数据库配置
	RedisConf        *RedisConf            `yaml:"redis"`              // Redis 配置（节点元数据/指标存储）
	ExtraConf        *ExtraConf            `yaml:"extra_conf"`         // 扩展配置（QoS、宿主机挂载白名单）
	Scheduler        *WrapperSchedulerConf `yaml:"scheduler"`          // 调度器配置（过滤/评分插件、亲和性、并发限制等）
	ReqTemplateConf  *ReqTemplateConf      `yaml:"req_template_conf"`  // 请求模板配置
	HookWhitelist    *HookWhitelist        `yaml:"hook_whitelist"`     // 应用钩子白名单（创建沙箱前后执行的钩子）
	CubeEgressConf   *CubeEgressConf       `yaml:"cube_egress_conf"`   // CubeEgress CA 注入配置
	CubeProxyConf    *CubeProxyConf        `yaml:"cube_proxy_conf"`    // CubeProxy 路由缓存失效配置

	// SoftDeletePurge configures the scheduled hard-purge of soft-deleted
	// (tombstoned) database rows (issue #973). nil leaves the purger at its
	// safe defaults (7-day retention, hourly, enabled). See CubeDB/tombstone.
	// SoftDeletePurge 配置软删除（墓碑）行的定时硬清理；nil 时使用安全默认值（保留 7 天、每小时、启用）
	SoftDeletePurge *SoftDeletePurgeConf `yaml:"soft_delete_purge"`

	// VolumePlugins lists external Controller Hook Plugin configurations.
	// Types: binary (fork CLI) or rpc (gRPC VolumeControllerService).
	//
	// Example:
	//   volume_plugins:
	//     - name: cos-rpc
	//       type: rpc
	//       socket_path: /run/cube-volume-cos-rpc.sock
	// VolumePlugins 外部存储卷插件配置列表（binary 或 rpc 两种类型）
	VolumePlugins []volumeplugin.Config `yaml:"volume_plugins"`
}

// CommonConf 通用配置：HTTP 服务、数据同步周期、mock 调试、缓存清理等
type CommonConf struct {
	MockUpdateAction       bool          `yaml:"mock_update_action"`        // mock 模式：更新动作
	DebugDumpHttpBody      bool          `yaml:"debug_dump_http_body"`      // 调试：打印 HTTP 请求体
	MockDebug              bool          `yaml:"mock_debug"`                // 调试模式（评分插件会输出调试日志文件）
	MockNodeNum            int           `yaml:"mock_node_num"`             // mock 节点数量
	MockCreateDirect       bool          `yaml:"mock_create_direct"`        // mock 模式：跳过调度直接创建
	MockCreateDirectHandle bool          `yaml:"mock_create_direct_handle"` // mock 模式：直接创建处理器
	MockHttpDirect         bool          `yaml:"mock_http_direct"`          // mock 模式：HTTP 直连
	MockCreateSleep        time.Duration `yaml:"mock_create_sleep"`         // mock 模式：创建前睡眠时间
	MockPercents           []float64     `yaml:"mock_percents"`             // mock 模式：按百分比模拟
	CubeDestroyCheckFilter bool          `yaml:"cube_destroy_check_filter"` // 销毁前检查过滤
	Debug                  Debug         `toml:"debug"`                     // 调试地址
	HttpPort               int           `yaml:"http_port"`                 // HTTP 服务端口（默认 8089）
	// HttpBind is the HTTP listen address. Empty means 0.0.0.0 (all
	// interfaces); set to 127.0.0.1 to keep the API loopback-only.
	// HttpBind HTTP 监听地址，默认 0.0.0.0；设为 127.0.0.1 则仅本机可访问
	HttpBind                  string `yaml:"http_bind"`
	WriteTimeout              int    `yaml:"http_writetimeout"`            // HTTP 写超时（秒）
	ReadTimeout               int    `yaml:"http_readtimeout"`             // HTTP 读超时（秒）
	IdleTimeout               int    `yaml:"http_idletimeout"`             // HTTP 空闲超时（秒）
	GraceFullStopTimeoutInSec int    `yaml:"gracefull_stop_timeout_insec"` // 优雅停机超时（秒）
	// CubeOps node-management base URL.
	CubeOpsAddr string `yaml:"cube_ops_addr"`
	// CubeOpsBootRetries: additional LoadNodes attempts (0 = single-shot).
	// Bridges the systemd startup window.
	CubeOpsBootRetries              int               `yaml:"cube_ops_boot_retries"`
	CubeOpsBootBackoff              time.Duration     `yaml:"cube_ops_boot_backoff"`
	SyncMetaDataInterval            time.Duration     `yaml:"sync_meta_data_interval"`             // 节点元数据同步周期
	SyncMetricDataInterval          time.Duration     `yaml:"sync_metric_data_interval"`           // 节点指标同步周期
	CleanSandboxCacheInterval       time.Duration     `yaml:"clean_sandbox_cache_interval"`        // 沙箱缓存清理周期
	EnabledListRunningSandboxCache  bool              `yaml:"enabled_list_running_sandbox_cache"`  // 启用运行中沙箱列表缓存
	AsyncTaskQueueSize              int               `yaml:"async_task_queue_size"`               // 异步任务队列容量
	AsyncTaskWorkerNum              int               `yaml:"async_task_worker_num"`               // 异步任务 worker 数
	HeadlessServiceName             string            `yaml:"headless_service_name"`               // headless 服务名
	DefaultHeadlessServiceNodesNum  int64             `yaml:"default_headless_service_nodes_num"`  // headless 服务默认节点数
	ListFilterOutLables             map[string]string `yaml:"list_filter_out_lables"`              // 列表查询时过滤掉的标签
	CollectMetricInterval           time.Duration     `yaml:"collect_metric_interval"`             // 指标采集周期（local.go 的 collectMetric 用）
	ReportLocalCreateNum            bool              `yaml:"report_local_create_num"`             // 是否上报单节点创建并发数
	ReportStdevMetric               bool              `yaml:"report_stdev_metric"`                 // 是否上报集群资源分布标准差
	GwCacheExpiredTime              time.Duration     `yaml:"gw_cache_expired_time"`               // 网关缓存过期时间
	GwCacheEnable                   bool              `yaml:"gw_cache_enable"`                     // 启用网关缓存
	ReportGWRedisGetMetric          bool              `yaml:"report_gw_redis_get_metric"`          // 上报网关 Redis 读取指标
	EnableGetStatusFromCubelet      bool              `yaml:"enable_get_status_from_cubelet"`      // 从 cubelet 获取状态
	DisableHardDelete               bool              `yaml:"disable_hard_delete"`                 // 禁用硬删除（只做软删除）
	CollectSandboxMemoryWhitelist   []string          `yaml:"collect_sandbox_memory_whitelist"`    // 采集沙箱内存的白名单
	EnableAllCollectSandboxMemory   bool              `yaml:"enable_all_collect_sandbox_memory"`   // 采集所有沙箱内存
	FilterErrMsgErrorCode           map[int]bool      `yaml:"filter_err_msg_error_code"`           // 过滤的错误码
	DescribeInstancesWhiteList      map[string]bool   `yaml:"describe_instances_white_list"`       // 查询实例白名单
	DescribeTaskExpireTime          int               `yaml:"describe_task_expire_time"`           // 查询任务过期时间
	EnablePrivateIpQuery            bool              `yaml:"enable_private_ip_query"`             // 启用私网 IP 查询
	DbMaxRetryCount                 int               `yaml:"db_max_retry_count"`                  // 数据库最大重试次数
	DbRetryInterval                 time.Duration     `yaml:"db_retry_interval"`                   // 数据库重试间隔
	EnableCheckComNetIDParam        bool              `yaml:"enable_check_com_net_id_param"`       // 校验网络参数
	EnableDescribeInstanceFromRedis bool              `yaml:"enable_describe_instance_from_redis"` // 从 Redis 查询实例
	MaxNICQueue                     int               `yaml:"max_nic_queue"`                       // 最大网卡队列数
	DisableCreateImageCluster       map[string]bool   `yaml:"disable_create_image_cluster"`        // 禁用的镜像集群
	EnableAGSColdStartSwitch        bool              `yaml:"enable_ags_cold_start_switch"`        // 启用 AGS 冷启动开关
}

// AuthConf 接口鉴权配置：启用签名校验（云 API 风格签名）
type AuthConf struct {
	Enable                   bool                         `yaml:"enable"`                      // 是否启用鉴权
	SignatureExpireTimeInsec int64                        `yaml:"signature_expire_time_insec"` // 签名过期时间（秒）
	SecretKeyMap             map[string]map[string]string `yaml:"secret_key_map"`              // 密钥表
}

// Debug 调试配置
type Debug struct {
	Address string `toml:"address"`
}

// DBConfig 数据库（DAO）配置：支持多种驱动（默认 mysql）
type DBConfig struct {
	// Driver selects the dao engine ("mysql", future: "postgres", ...).
	// Empty defaults to "mysql" for backwards compatibility with v0.2.2
	// configurations that pre-date the multi-driver dao layer.
	// Driver 数据库驱动类型，默认 "mysql"
	Driver string `yaml:"driver"`

	Addr                   string `yaml:"addr"`                       // 数据库地址
	User                   string `yaml:"user"`                       // 用户名
	Pwd                    string `yaml:"pwd"`                        // 密码
	DBName                 string `yaml:"db_name"`                    // 库名
	ConnTimeout            int    `yaml:"conn_timeout"`               // 连接超时（秒）
	ReadTimeout            int    `yaml:"read_timeout"`               // 读超时（秒）
	WriteTimeout           int    `yaml:"write_timeout"`              // 写超时（秒）
	MaxIdleConns           int    `yaml:"max_idle_conns"`             // 最大空闲连接数
	MaxOpenConns           int    `yaml:"max_open_conns"`             // 最大打开连接数
	MaxConnLifeTimeSeconds int    `yaml:"max_conn_life_time_seconds"` // 连接最大存活时间（秒）

	// MigrationLockTimeoutSeconds bounds the cluster-wide GET_LOCK wait
	// at startup. Defaults to 60 seconds when zero.
	// MigrationLockTimeoutSeconds 启动时集群级 GET_LOCK 等待上限（默认 60 秒）
	MigrationLockTimeoutSeconds int `yaml:"migration_lock_timeout_seconds"`
}

// ExtraConf 扩展配置：QoS（块/文件/网络）与宿主机目录挂载白名单
type ExtraConf struct {
	BlkQos     string            `yaml:"blk_qos"`      // 块设备 QoS（JSON 字符串）
	BlkQosMap  map[string]string `yaml:"blk_qos_map"`  // 块设备 QoS 映射表
	FsQos      string            `yaml:"fs_qos"`       // 文件系统 QoS
	FsQosMap   map[string]string `yaml:"fs_qos_map"`   // 文件系统 QoS 映射表
	NetQosList string            `yaml:"net_qos_list"` // 网络 QoS 列表

	// AllowedHostMountPrefixes restricts which host directories can be
	// bind-mounted into sandboxes. Each entry must be an absolute path
	// (trailing "/" is optional and will be appended automatically).
	// A hostPath is allowed if it is under at least one prefix.
	// Default (when empty): ["/data/shared/"].
	// AllowedHostMountPrefixes 允许 bind-mount 进沙箱的宿主机目录前缀白名单（默认 ["/data/shared/"]）
	AllowedHostMountPrefixes []string `yaml:"allowed_host_mount_prefixes"`
}

// CubeProxyConf controls how CubeMaster invalidates CubeProxy local routing
// caches after Resume rewrites Redis SandboxIP / port mappings.
// CubeProxyConf 控制 CubeMaster 在 Resume 重写 Redis 中沙箱 IP/端口映射后，
// 如何让 CubeProxy 的本地路由缓存失效
type CubeProxyConf struct {
	// AdminPort is the per-node CubeProxy admin listen port used when the
	// Redis registry is empty (default 8082).
	// AdminPort 每节点 CubeProxy 的 admin 监听端口（默认 8082）
	AdminPort int `yaml:"admin_port"`
	// AdminToken is sent as X-Cube-Admin-Token when non-empty (must match
	// CubeProxy $cube_admin_token).
	// AdminToken 非空时作为 X-Cube-Admin-Token 请求头发送（需与 CubeProxy 的 $cube_admin_token 一致）
	AdminToken string `yaml:"admin_token"`
	// AdminURLs optionally lists static admin base URLs (e.g. http://ip:8082).
	// When set, InvalidateBackendCache broadcasts to these instead of reading
	// the Redis CubeProxy registry.
	// AdminURLs 可选：静态 admin 地址列表；设置后缓存失效广播直接发往这些地址，而不读 Redis 注册表
	AdminURLs []string `yaml:"admin_urls"`
	// HeartbeatTTLMs is how fresh a registry heartbeat must be to treat a
	// replica as live (default 15000).
	// HeartbeatTTLMs 注册表心跳新鲜度阈值（默认 15000ms），超时视为副本失活
	HeartbeatTTLMs int64 `yaml:"heartbeat_ttl_ms"`
}

// RedisConf Redis 配置：节点元数据、指标、注册表等存储
type RedisConf struct {
	Password    string `yaml:"password"`     // 密码
	MaxActive   int    `yaml:"max_active"`   // 最大活跃连接数
	MaxIdle     int    `yaml:"max_idle"`     // 最大空闲连接数
	IdleTimeout int    `yaml:"idle_timeout"` // 空闲连接超时
	DbNo        int    `yaml:"db_no"`        // 库编号

	Nodes    string `yaml:"nodes"`     // 节点地址（逗号分隔）
	MaxRetry int    `yaml:"max_retry"` // 最大重试次数

	// MasterName enables Redis Sentinel mode when non-empty. SentinelNodes
	// must list one or more sentinel endpoints (host:port, comma-separated).
	// MasterName 非空时启用 Redis Sentinel 高可用模式；SentinelNodes 需列出哨兵端点
	MasterName string `yaml:"master_name"`

	// SentinelNodes lists sentinel endpoints used when MasterName is set.
	// SentinelNodes 哨兵节点列表（MasterName 设置时使用）
	SentinelNodes string `yaml:"sentinel_nodes"`

	// SentinelPassword authenticates to sentinel instances. Empty means no
	// AUTH against sentinel (master Password is still used for the Redis master).
	// SentinelPassword 哨兵实例的认证密码；空表示不对哨兵做 AUTH（master 仍用 Password）
	SentinelPassword string `yaml:"sentinel_password"`

	// NodeMetricTTLSec is the safety TTL (seconds) for node-metric keys so an
	// offline node's entry auto-expires; refreshed on every heartbeat write.
	// A value <= 0 disables the TTL.
	// NodeMetricTTLSec 节点指标键的安全 TTL（秒）：离线节点自动过期；每次心跳写入时刷新。<=0 表示禁用
	NodeMetricTTLSec int `yaml:"node_metric_ttl_sec"`
	// SandboxProxyTTLSec is an OPTIONAL safety TTL (seconds) for sandbox proxy
	// routing keys. It defaults to 0 (disabled) because the route key has no
	// refresh path; enabling it is only safe if the TTL exceeds the maximum
	// sandbox lifetime, otherwise a live route would expire and break routing.
	// Normal teardown removes the key via DEL.
	// SandboxProxyTTLSec 沙箱代理路由键的可选安全 TTL（秒）；默认 0（禁用）。
	// 路由键没有刷新路径，只有 TTL 超过沙箱最大生命周期才可启用，否则会在路由存活时过期导致断路由
	SandboxProxyTTLSec int `yaml:"sandbox_proxy_ttl_sec"`
}

// SchedulerConf 调度器配置：过滤/评分插件、亲和性、并发限制、资源超卖等。
// 这是整个调度系统最核心的配置块
type SchedulerConf struct {
	Overhead                         *OverheadConf                     `yaml:"overhead"`                             // 虚拟化开销（VM/宿主机内存 CPU 开销）
	NodeMaxMvmNum                    int64                             `yaml:"node_max_mvm_num"`                     // 单节点最大 MVM 数（默认 3000）
	NodeMaxMvmNumReserveNumPercent   float64                           `yaml:"node_max_mvm_num_reserve_num_percent"` // MVM 数保留比例
	NodeMaxMemReservedInMB           int64                             `yaml:"node_max_mem_reserved_in_mb"`          // 节点保留内存（MB）
	NodeMaxCpuUtil                   float64                           `yaml:"node_max_cpu_util"`                    // 节点 CPU 利用率阈值（默认 80，cpuFilter 用）
	PreSelectNum                     int                               `yaml:"pre_select_num"`                       // 预过滤候选数（-1 = 不限制）
	PrioritySelectNum                int                               `yaml:"priority_select_num"`                  // 最终加权随机选择的候选数（-1 = 不限制）
	LeastSelectName                  string                            `yaml:"least_select_name"`                    // 选择算法名（random/sw/rw/rrw，默认 random）
	MetricUpdateTimeout              time.Duration                     `yaml:"metric_update_timeout"`                // 全局指标超时（默认 1h）
	LocalMetricUpdateTimeout         time.Duration                     `yaml:"local_metric_update_timeout"`          // 本地指标超时
	Filter                           *SchedulerFilterConf              `yaml:"filter"`                               // 过滤插件配置（启用哪些）
	Score                            *SchedulerScoreConf               `yaml:"score"`                                // 评分插件配置（启用哪些 + 权重）
	PostScore                        *PostScoreConf                    `yaml:"postscore"`                            // 评分后处理配置（白名单）
	DisableCircuitFilter             bool                              `yaml:"disable_circuit_filter"`               // 禁用熔断过滤（prefilter 的 FilterOut 检查）
	InBackoffMode                    bool                              `yaml:"in_backoff_mode"`                      // 是否处于兜底模式
	AffinityConf                     map[string]AffinityConf           `yaml:"affinityconf"`                         // 亲和性配置（按服务名）
	NodeMaxMvmNumConf                map[string]NodeMaxMvmNumConf      `yaml:"node_max_mvm_num_conf"`                // 按实例类型的 MVM 上限
	EnableRunInstanceHostIps         bool                              `yaml:"enable_run_instance_host_ips"`         // 启用运行实例宿主 IP
	MaxMvmCPU                        string                            `yaml:"max_mvm_cpu"`                          // MVM 最大 CPU（字符串形式）
	maxCpu                           resource.Quantity                 // 解析后的最大 CPU
	MaxMvmMemory                     string                            `yaml:"max_mvm_memory"` // MVM 最大内存
	maxMem                           resource.Quantity                 // 解析后的最大内存
	DiskUsageMaxPercent              float64                           `yaml:"disk_usage_max_percent"`               // 磁盘使用率阈值（默认 80，diskFilter 用）
	LargeSizeAffinityConf            map[string]LargeSizeAffinityConf  `yaml:"large_size_affinity_conf"`             // 大规格亲和性配置
	NodeMaxMemReservedConf           map[string]NodeMaxMemReservedConf `yaml:"node_max_mem_reserved_conf"`           // 按实例类型的保留内存
	DisableBackoffFilterInstanceType map[string]bool                   `yaml:"disable_backoff_filter_instance_type"` // 禁用兜底过滤的实例类型
	ThirtpartyFilterInstanceType     map[string]bool                   `yaml:"thirtparty_filter_instance_type"`      // 第三方过滤生效的实例类型
	InstanceTypeConf                 map[string]InstanceTypeConf       `yaml:"instance_type_conf"`                   // 实例类型配置（OSS 集群标签）
	NodeAffinitySelectorAllowedKeys  []string                          `yaml:"node_affinity_selector_allowed_keys"`  // 亲和性选择器允许的标签键（白名单）
	// ProfileRouteLabelKeys is the allow-list of request labels that may be
	// used by scheduler profile routes or sent to external scheduler plugins.
	// Route labels not present in this list make the profile configuration
	// invalid at scheduler startup.
	ProfileRouteLabelKeys []string               `yaml:"profile_route_label_keys"`
	Profiles              []SchedulerProfileConf `yaml:"profiles"`

	// IgnoreRedisAllocation, when true, makes the scheduler ignore the
	// per-node allocated CPU/Mem usage recorded in Redis (treat allocated as
	// 0). A pointer is used so an unset value can default to false while still
	// allowing operators to explicitly enable it. Defaults to false.
	// IgnoreRedisAllocation 为 true 时，调度忽略 Redis 中记录的节点已分配 CPU/内存（视为 0）。
	// 用指针类型以便区分"未设置"和"显式设为 false"
	IgnoreRedisAllocation *bool `yaml:"ignore_redis_allocation"`
	// OvercommitRatio is the global CPU/Mem overcommit ratio applied to the
	// node-reported quota during scheduling. Defaults to CPU=3, Mem=2.
	// OvercommitRatio 全局 CPU/内存超卖比例（对节点上报配额做放大），默认 CPU=3、Mem=2
	OvercommitRatio *OvercommitRatioConf `yaml:"overcommit_ratio"`
	// OvercommitRatioByType overrides OvercommitRatio for specific instance
	// types and takes precedence over the global ratio.
	// OvercommitRatioByType 按实例类型覆盖全局超卖比例，优先级高于全局配置
	OvercommitRatioByType map[string]OvercommitRatioConf `yaml:"overcommit_ratio_conf"`
}

// defaultNodeAffinitySelectorAllowedKeys 默认允许的亲和性选择器标签键白名单
var defaultNodeAffinitySelectorAllowedKeys = []string{
	constants.AffinityKeyZone,
	constants.AffinityKeyClusterID,
	constants.AffinityKeyCPUType,
	constants.AffinityKeyMemorySize,
	constants.AffinityKeyCPUCores,
	constants.AffinityKeyInstanceType,
}

// reservedLabelNamespaces defines label key prefixes that are owned by the
// system. Labels whose namespace (the part before '/') matches any entry, or
// whose whole key (when no '/' is present) matches any entry, are reserved and
// cannot be created or deleted via the admin API.
// reservedLabelNamespaces 系统保留的标签键命名空间前缀：
// 这些前缀下的标签（或整键匹配的标签）属于系统，不能通过 admin API 创建/删除
var reservedLabelNamespaces = []string{
	"kubernetes.io",
	"beta.kubernetes.io",
	"cube.cloud.tencentcloud.com",
}

// IsReservedLabelKey reports whether k is a system-reserved label key
// that cannot be created or deleted via the admin API.
// It checks the namespace prefix of the key (the part before '/'),
// or the whole key if no '/' is present.
// IsReservedLabelKey 判断标签键 k 是否为系统保留键（不能通过 admin API 创建/删除）：
// 检查键的命名空间前缀（'/' 之前的部分），无 '/' 时检查整个键
func IsReservedLabelKey(k string) bool {
	ns := k
	if prefix, _, ok := strings.Cut(k, "/"); ok {
		ns = prefix
	}
	for _, reserved := range reservedLabelNamespaces {
		if ns == reserved || strings.HasSuffix(ns, "."+reserved) {
			return true
		}
	}
	return false
}

// OvercommitRatioConf describes the CPU/Mem overcommit multipliers applied to
// a node's reported quota when computing schedulable capacity.
// OvercommitRatioConf 超卖倍率配置：计算可调度容量时对节点上报配额乘的系数
type OvercommitRatioConf struct {
	CPURatio float64 `yaml:"cpu_ratio"` // CPU 超卖倍率
	MemRatio float64 `yaml:"mem_ratio"` // 内存超卖倍率
}

const (
	defaultCPUOvercommitRatio = 3.0 // 默认 CPU 超卖倍率
	defaultMemOvercommitRatio = 2.0 // 默认内存超卖倍率
)

// GetEffectiveOvercommitRatio returns the overcommit ratio for the given
// instance type, falling back to the global ratio and then to the built-in
// defaults (CPU=3, Mem=2).
// GetEffectiveOvercommitRatio 获取指定实例类型的有效超卖倍率：
// 优先实例类型配置，其次全局配置，最后内置默认值（CPU=3、Mem=2）
func (s *SchedulerConf) GetEffectiveOvercommitRatio(instanceType string) OvercommitRatioConf {
	if s.OvercommitRatioByType != nil {
		if v, ok := s.OvercommitRatioByType[instanceType]; ok {
			return v.sanitized()
		}
	}
	if s.OvercommitRatio != nil {
		return s.OvercommitRatio.sanitized()
	}
	return OvercommitRatioConf{CPURatio: defaultCPUOvercommitRatio, MemRatio: defaultMemOvercommitRatio}
}

// sanitized guarantees non-positive, NaN, or infinite ratios fall back to the
// defaults so a malformed config never shrinks a node's schedulable capacity to
// zero or produces a garbage (NaN/Inf) capacity when multiplied with the quota.
// sanitized 保证非法倍率（非正数/NaN/无穷）回退到默认值，
// 防止畸形配置把节点可调度容量算成 0 或 NaN/Inf
func (c OvercommitRatioConf) sanitized() OvercommitRatioConf {
	out := c
	if !isValidRatio(out.CPURatio) {
		out.CPURatio = defaultCPUOvercommitRatio
	}
	if !isValidRatio(out.MemRatio) {
		out.MemRatio = defaultMemOvercommitRatio
	}
	return out
}

// isValidRatio reports whether r is a usable overcommit multiplier: it must be
// a finite, positive number. NaN and ±Inf (e.g. ".nan"/".inf" in YAML) are
// rejected so they never propagate into capacity arithmetic.
// isValidRatio 判断 r 是否为可用的超卖倍率：必须是有限的正数。
// 拒绝 NaN 和 ±Inf（如 YAML 中的 ".nan"/".inf"），避免污染容量计算
func isValidRatio(r float64) bool {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return false
	}
	return r > 0
}

// ShouldIgnoreRedisAllocation reports whether the scheduler must ignore the
// allocated CPU/Mem usage recorded in Redis. Defaults to false when unset.
// ShouldIgnoreRedisAllocation 调度器是否忽略 Redis 中记录的已分配用量（未设置时默认 false）
func (s *SchedulerConf) ShouldIgnoreRedisAllocation() bool {
	if s.IgnoreRedisAllocation == nil {
		return false
	}
	return *s.IgnoreRedisAllocation
}

// EffectiveQuotaCpu returns the schedulable CPU capacity (milli-cores) for a
// node after applying the configured overcommit ratio to its reported quota.
// EffectiveQuotaCpu 返回节点可调度的 CPU 容量（毫核）：上报配额 × 超卖倍率。
// 这就是"配额账"的放大版——超卖倍率允许账面承诺超过物理配额
func (s *SchedulerConf) EffectiveQuotaCpu(instanceType string, quotaCpu int64) int64 {
	ratio := s.GetEffectiveOvercommitRatio(instanceType)
	return floatToInt64Clamped(float64(quotaCpu) * ratio.CPURatio)
}

// EffectiveQuotaMem returns the schedulable memory capacity (MB) for a node
// after applying the configured overcommit ratio to its reported quota.
// EffectiveQuotaMem 返回节点可调度的内存容量（MB）：上报配额 × 超卖倍率
func (s *SchedulerConf) EffectiveQuotaMem(instanceType string, quotaMem int64) int64 {
	ratio := s.GetEffectiveOvercommitRatio(instanceType)
	return floatToInt64Clamped(float64(quotaMem) * ratio.MemRatio)
}

// floatToInt64Clamped safely converts a float64 to int64. Converting an
// out-of-range or non-finite float64 to int64 is implementation-defined in Go
// and yields a garbage value, so NaN maps to 0 and values beyond the int64
// range (including ±Inf) are clamped to math.MaxInt64 / math.MinInt64. This
// guards capacity computation against quota * ratio overflowing int64.
// floatToInt64Clamped 安全地把 float64 转 int64：Go 中越界/非有限值的转换行为未定义，
// 故 NaN 映射为 0，超界值（含 ±Inf）钳制到 MaxInt64/MinInt64，防止 配额×倍率 溢出
func floatToInt64Clamped(f float64) int64 {
	if math.IsNaN(f) {
		return 0
	}
	// float64(math.MaxInt64) rounds up to 2^63, so use >= to treat the
	// boundary and any larger value (incl. +Inf) as overflow.
	// 注意 float64(MaxInt64) 会进位到 2^63，所以用 >= 判定溢出边界
	if f >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if f <= float64(math.MinInt64) {
		return math.MinInt64
	}
	return int64(f)
}

// EffectiveAllocated returns the allocated usage the scheduler should account
// for, which is 0 when Redis allocation records are ignored.
// EffectiveAllocated 返回调度记账用的已分配用量；启用 IgnoreRedisAllocation 时为 0
func (s *SchedulerConf) EffectiveAllocated(usage int64) int64 {
	if s.ShouldIgnoreRedisAllocation() {
		return 0
	}
	return usage
}

// WrapperSchedulerConf 调度器配置的包装：内嵌 SchedulerConf 并附带运行时派生字段
type WrapperSchedulerConf struct {
	SchedulerConf           `yaml:",inline"`  // 内嵌调度器配置（YAML 平铺）
	labelRefInstanceTypeMap map[string]string // 集群标签 → 实例类型 的映射（预计算，供查询）
}

// InstanceTypeConf 实例类型配置：该实例类型对应的 OSS 集群标签
type InstanceTypeConf struct {
	OssClusterLabels map[string]any `yaml:"oss_cluster_labels"`
}

// LargeSizeAffinityConf 大规格沙箱亲和性配置：大规格请求的节点选择偏好
type LargeSizeAffinityConf struct {
	Enable               bool           `yaml:"enable"`                  // 是否启用
	MemoryLowerWaterMark string         `yaml:"memory_lower_water_mark"` // 内存低水位线
	CpuLowerWaterMark    string         `yaml:"cpu_lower_water_mark"`    // CPU 低水位线
	Operator             string         `yaml:"operator"`                // 比较操作符（Gt/Lt）
	ClusterLabels        map[string]any `yaml:"cluster_labels"`          // 集群标签
}

// NodeMaxMvmNumConf 按实例类型的单节点 MVM 上限配置
type NodeMaxMvmNumConf struct {
	MvmNum                  int64   `yaml:"mvm_num"`                     // MVM 上限
	MvmNumReserveNumPercent float64 `yaml:"mvm_num_reserve_num_percent"` // 保留比例
}

// NodeMaxMemReservedConf 按实例类型的节点保留内存配置
type NodeMaxMemReservedConf struct {
	MaxMemReservedInMB        int64   `yaml:"max_mem_reserved_in_mb"`         // 保留内存（MB）
	MaxMemReservedInMBPercent float64 `yaml:"max_mem_reserved_in_mb_percent"` // 保留内存比例
}

// AffinityConf 亲和性配置（按服务名）：是否启用、是否禁用 cgroup 等
type AffinityConf struct {
	Enable            bool           `yaml:"enable"`              // 是否启用
	DisableVmCgroup   bool           `yaml:"disable_vm_cgroup"`   // 禁用 VM cgroup
	DisableHostCgroup bool           `yaml:"disable_host_cgroup"` // 禁用宿主机 cgroup
	ClusterLabels     map[string]any `yaml:"cluster_labels"`      // 集群标签
}

// GetAffinityConf 按服务名获取亲和性配置（未配置时返回默认值）
func (s *SchedulerConf) GetAffinityConf(serviceName string) AffinityConf {
	if s.AffinityConf == nil {
		return AffinityConf{
			Enable:            false,
			DisableVmCgroup:   false,
			DisableHostCgroup: false,
		}
	}
	return s.AffinityConf[serviceName]
}

// GetLargeSizeAffinityConf 按服务名获取大规格亲和性配置（未配置时返回默认值）
func (s *SchedulerConf) GetLargeSizeAffinityConf(serviceName string) LargeSizeAffinityConf {
	if s.LargeSizeAffinityConf == nil {
		return LargeSizeAffinityConf{
			Enable: false,
		}
	}
	return s.LargeSizeAffinityConf[serviceName]
}

// DefaultNodeAffinitySelectorAllowedKeySet 返回默认允许的亲和性标签键集合
func DefaultNodeAffinitySelectorAllowedKeySet() map[string]struct{} {
	allowed := make(map[string]struct{}, len(defaultNodeAffinitySelectorAllowedKeys))
	for _, key := range defaultNodeAffinitySelectorAllowedKeys {
		allowed[key] = struct{}{}
	}
	return allowed
}

// NodeAffinitySelectorAllowedKeySet 返回有效的亲和性标签键白名单（默认 + 配置追加）
func (s *SchedulerConf) NodeAffinitySelectorAllowedKeySet() map[string]struct{} {
	allowed := DefaultNodeAffinitySelectorAllowedKeySet()
	if s != nil {
		for _, key := range s.NodeAffinitySelectorAllowedKeys {
			allowed[key] = struct{}{}
		}
	}
	return allowed
}

// MaxMvmCPURes 返回解析后的 MVM 最大 CPU 资源量（惰性解析）
func (s *SchedulerConf) MaxMvmCPURes() resource.Quantity {
	if s.maxCpu.IsZero() {
		return resource.MustParse(s.MaxMvmCPU)
	}
	return s.maxCpu
}

// MaxMvmMemoryRes 返回解析后的 MVM 最大内存资源量（惰性解析）
func (s *SchedulerConf) MaxMvmMemoryRes() resource.Quantity {
	if s.maxMem.IsZero() {
		return resource.MustParse(s.MaxMvmMemory)
	}
	return s.maxMem
}

// GetNodeMaxMvmNumConf 获取指定实例类型的 MVM 上限配置（未配置时回退全局值）
func (s *SchedulerConf) GetNodeMaxMvmNumConf(instanceType string) NodeMaxMvmNumConf {
	if s.NodeMaxMvmNumConf == nil {
		return NodeMaxMvmNumConf{
			MvmNum:                  s.NodeMaxMvmNum,
			MvmNumReserveNumPercent: s.NodeMaxMvmNumReserveNumPercent,
		}
	}
	if v, ok := s.NodeMaxMvmNumConf[instanceType]; !ok {
		return NodeMaxMvmNumConf{
			MvmNum:                  s.NodeMaxMvmNum,
			MvmNumReserveNumPercent: s.NodeMaxMvmNumReserveNumPercent,
		}
	} else {
		return v
	}
}

// GetNodeMaxMemReservedConf 获取指定实例类型的保留内存配置（未配置时回退全局值）
func (s *SchedulerConf) GetNodeMaxMemReservedConf(instanceType string) NodeMaxMemReservedConf {
	if s.NodeMaxMemReservedConf == nil {
		return NodeMaxMemReservedConf{
			MaxMemReservedInMB: s.NodeMaxMemReservedInMB,
		}
	}
	if v, ok := s.NodeMaxMemReservedConf[instanceType]; !ok {
		return NodeMaxMemReservedConf{
			MaxMemReservedInMB: s.NodeMaxMemReservedInMB,
		}
	} else {
		return v
	}

}

// GetEffectiveNodeMaxMemReservedInMB 计算指定实例类型生效的节点保留内存（MB）：
// 优先显式配置，其次按配额百分比计算，最后回退全局值；
// 若计算结果超过配额则强制压到配额的 10%（防止保留内存吃掉全部配额）
func (s *SchedulerConf) GetEffectiveNodeMaxMemReservedInMB(instanceType string, quotaMemMB int64) int64 {
	conf := s.GetNodeMaxMemReservedConf(instanceType)
	reservedMB := conf.MaxMemReservedInMB
	if reservedMB <= 0 && conf.MaxMemReservedInMBPercent > 0 && quotaMemMB > 0 {
		reservedMB = int64(math.Ceil(float64(quotaMemMB) * conf.MaxMemReservedInMBPercent))
	}
	if reservedMB <= 0 {
		reservedMB = s.NodeMaxMemReservedInMB
	}
	if quotaMemMB > 0 && reservedMB >= quotaMemMB {

		reservedMB = int64(math.Ceil(float64(quotaMemMB) * 0.1))
	}
	if reservedMB < 0 {
		return 0
	}
	return reservedMB
}

// SchedulerFilterConf 过滤插件配置：启用的过滤器名列表
type SchedulerFilterConf struct {
	EnableFilters []string `yaml:"enable_filters"`
}

// SchedulerProfileConf selects and configures one scheduling pipeline. The
// first matching non-default profile wins; an optional default profile is used
// when no route matches. With no profiles configured, the scheduler compiles a
// legacy profile from Filter, Score, PostScore and PrioritySelectNum.
type SchedulerProfileConf struct {
	Name      string                       `yaml:"name"`
	Default   bool                         `yaml:"default"`
	Route     SchedulerProfileRouteConf    `yaml:"route"`
	Filters   []SchedulerProfilePluginConf `yaml:"filters"`
	Scores    []SchedulerProfilePluginConf `yaml:"scores"`
	Selection SchedulerSelectionConf       `yaml:"selection"`
	Failure   SchedulerFailureConf         `yaml:"failure"`
}

type SchedulerProfileRouteConf struct {
	InstanceTypes []string          `yaml:"instance_types"`
	Labels        map[string]string `yaml:"labels"`
}

// SchedulerProfilePluginConf is shared by built-in Go, CEL expression and
// external gRPC plugins. Type defaults to "go". External plugins use a Unix
// socket by default, while tests and development may use a host:port target.
type SchedulerProfilePluginConf struct {
	Name                   string         `yaml:"name"`
	Type                   string         `yaml:"type"`
	Enabled                *bool          `yaml:"enabled"`
	Weight                 float64        `yaml:"weight"`
	DefaultScore           float64        `yaml:"default_score"`
	Expr                   string         `yaml:"expr"`
	SocketPath             string         `yaml:"socket_path"`
	Timeout                time.Duration  `yaml:"timeout"`
	CircuitBreakerFailures int            `yaml:"circuit_breaker_failures"`
	CircuitBreakerCooldown time.Duration  `yaml:"circuit_breaker_cooldown"`
	Args                   map[string]any `yaml:"args"`
}

type SchedulerSelectionConf struct {
	TopN   int    `yaml:"top_n"`
	Method string `yaml:"method"`
}

type SchedulerFailureConf struct {
	Filter      string `yaml:"filter"`
	Score       string `yaml:"score"`
	NoCandidate string `yaml:"no_candidate"`
}

// ControlledProfileLabels returns a defensive copy containing only labels
// explicitly allowed for profile routing. This prevents arbitrary user labels
// from changing scheduler policy or leaking to external plugins.
func (s *SchedulerConf) ControlledProfileLabels(labels map[string]string) map[string]string {
	if s == nil || len(labels) == 0 || len(s.ProfileRouteLabelKeys) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(s.ProfileRouteLabelKeys))
	for _, key := range s.ProfileRouteLabelKeys {
		if key = strings.TrimSpace(key); key != "" {
			allowed[key] = struct{}{}
		}
	}
	out := make(map[string]string, len(allowed))
	for key := range allowed {
		if value, ok := labels[key]; ok {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PostScoreConf 评分后处理配置：白名单加减分与权重因子
type PostScoreConf struct {
	Disable              bool               `yaml:"disable"`             // 是否禁用
	ParamFactor          float64            `yaml:"param_factor"`        // 参数因子（默认 0.015，放大权重）
	ResourceWeights      map[string]float64 `yaml:"resource_weights"`    // 各因子的权重表
	ActiveWhiteList      []string           `yaml:"active_white_list"`   // 活跃白名单（配置形式，列表）
	ActiveWhiteListMap   map[string]bool    `yaml:"-"`                   // 活跃白名单（运行时 map 形式，由 preHandle 转换）
	NegativeWhiteList    []string           `yaml:"negative_white_list"` // 负面白名单（配置形式）
	NegativeWhiteListMap map[string]bool    `yaml:"-"`                   // 负面白名单（运行时 map 形式）
}

// SchedulerScoreConf 评分插件配置：启用的评分器、权重表、各插件参数
type SchedulerScoreConf struct {
	EnableScorers   []string           `yaml:"enable_scorers"`   // 启用的评分器名列表
	ResourceWeights map[string]float64 `yaml:"resource_weights"` // 各权重因子的权重
	ScorePluginConf ScorePluginConf    `yaml:"plugin_conf"`      // 各评分插件的参数
}

// ScorePluginConf 各评分插件的参数集合（按插件名）
type ScorePluginConf struct {
	MultiFactorWeightedAverage *MultiFactorWeightedAverage `yaml:"multi_factor_weighted_average"` // 多因子加权平均（异步预计算）
	RealTimeWeightedAverage    *RealTimeWeightedAverage    `yaml:"real_time_weighted_average"`    // 实时加权平均
	AffinityScore              *AffinityScore              `yaml:"affinity_score"`                // 亲和性偏好评分
	ImageScore                 *ImageScore                 `yaml:"image_score"`                   // 镜像本地化评分
	TemplateScore              *TemplateScore              `yaml:"template_score"`                // 模板评分
}

// MultiFactorWeightedAverage 多因子加权平均插件配置（异步预计算）
type MultiFactorWeightedAverage struct {
	ScoreInterval       time.Duration `yaml:"score_interval"`        // 分数刷新周期
	Weight              float64       `yaml:"weight"`                // 插件权重
	EnableWeightFactors []string      `yaml:"enable_weight_factors"` // 启用的权重因子
	Disable             bool          `yaml:"disable"`               // 是否禁用
}

// RealTimeWeightedAverage 实时加权平均插件配置
type RealTimeWeightedAverage struct {
	Weight              float64  `yaml:"weight"`                // 插件权重
	EnableWeightFactors []string `yaml:"enable_weight_factors"` // 启用的权重因子
	Disable             bool     `yaml:"disable"`               // 是否禁用
}

// AffinityScore 亲和性偏好评分插件配置
type AffinityScore struct {
	Weight              float64  `yaml:"weight"`                // 插件权重
	EnableWeightFactors []string `yaml:"enable_weight_factors"` // 启用的权重因子
	Disable             bool     `yaml:"disable"`               // 是否禁用
}

// ImageScore 镜像本地化评分插件配置
type ImageScore struct {
	Weight              float64  `yaml:"weight"`                // 插件权重
	EnableWeightFactors []string `yaml:"enable_weight_factors"` // 启用的权重因子
	Disable             bool     `yaml:"disable"`               // 是否禁用
}

// TemplateScore 模板评分插件配置
type TemplateScore struct {
	Weight              float64  `yaml:"weight"`                // 插件权重
	EnableWeightFactors []string `yaml:"enable_weight_factors"` // 启用的权重因子
	Disable             bool     `yaml:"disable"`               // 是否禁用
}

// CubeletConf 与 cubelet 交互的配置：RPC 超时、重试策略、缓冲队列与并发限制。
// 其中 BufferQueueMinJob / CreateConcurrentLimit / DestroyConcurentLimit
// 正是 local.go 中缓冲队列与动态限流用的配置
type CubeletConf struct {
	Grpc                    *GrpcConf `yaml:"grpc"`                       // gRPC 服务配置
	CommonTimeoutInsec      int       `yaml:"common_timeout_insec"`       // 通用超时（秒）
	CreateImageTimeoutInSec int       `yaml:"create_image_timeout_insec"` // 创建镜像超时（秒）
	// Server default idle TTL when the client omits timeout. See docs/guide/lifecycle.md.
	// DefaultTimeoutInsec 客户端未指定超时时的服务端默认空闲 TTL
	DefaultTimeoutInsec int `yaml:"default_timeout_insec"`
	// Create RPC / scheduling deadline; decoupled from idle TTL.
	// CreateTimeoutInsec 创建 RPC / 调度截止时间（与空闲 TTL 解耦）
	CreateTimeoutInsec    int                  `yaml:"create_timeout_insec"`
	AsyncFlows            map[string]asyncFlow `yaml:"async_flows"`              // 异步流程配置（并发/重试）
	RetryCode             []string             `yaml:"retry_code"`               // 可重试错误码
	LoopRetryCode         []string             `yaml:"loop_retry_code"`          // 循环重试错误码
	ReuseRetryCode        []string             `yaml:"reuse_retry_code"`         // 复用重试错误码
	CircuitBreakCode      []string             `yaml:"circuit_break_code"`       // 熔断错误码
	ExcludeLoopRetryCode  []string             `yaml:"exclude_loop_retry_code"`  // 排除循环重试的错误码
	BackoffRetryCode      []string             `yaml:"backoff_retry_code"`       // 退避重试错误码
	MaxRetries            int64                `yaml:"max_retries"`              // 最大重试次数（默认 5）
	LoopMaxRetries        int64                `yaml:"loop_max_retries"`         // 循环最大重试次数（默认 100）
	BufferQueueMinJob     int64                `yaml:"buffer_queue_min_job"`     // 缓冲队列最小容量（默认 10，local.go 的 newTask 用）
	CreateConcurrentLimit int64                `yaml:"create_concurrent_limit"`  // 创建并发上限（默认 100，monitorLimit 的下限）
	DestroyConcurentLimit int64                `yaml:"destroy_concurent_limit"`  // 销毁并发上限（默认 50，monitorLimit 用）
	ExposedPortList       []string             `yaml:"exposed_port_list"`        // 暴露端口列表
	EnableExposedPort     bool                 `yaml:"enable_exposed_port"`      // 启用暴露端口
	DisableRedisProxyPort bool                 `yaml:"disable_redis_proxy_port"` // 禁用 Redis 代理端口
	MaxDelayInSecond      int64                `yaml:"max_delay_in_second"`      // 最大延迟（秒）
	BackoffRetryDelay     time.Duration        `yaml:"backoff_retry_delay"`      // 退避重试延迟（默认 5ms）
}

// GrpcConf gRPC 服务配置（与 cubelet 通信）
type GrpcConf struct {
	GrpcPort                     int `yaml:"grpc_port"`                         // gRPC 端口（默认 9999）
	CleanConnTaskIntervalInMin   int `yaml:"clean_conn_task_interval_in_min"`   // 连接清理任务周期（分钟）
	CleanConnTaskRoutinePoolSize int `yaml:"clean_conn_task_routine_pool_size"` // 连接清理协程池大小
	ConnExpireTimeInSec          int `yaml:"conn_expire_time_insec"`            // 连接过期时间（秒）
}

// asyncFlow 异步流程配置：并发上限与重试次数
type asyncFlow struct {
	MaxConcurrent  int64 `yaml:"concurrent"`       // 最大并发数
	MaxRetries     int64 `yaml:"max_retries"`      // 最大重试次数
	LoopMaxRetries int64 `yaml:"loop_max_retries"` // 循环最大重试次数
}

// OverheadConf 虚拟化开销配置：VM 与宿主机在 CPU/内存上的额外消耗
type OverheadConf struct {
	VmMemoryOverheadBase        string `yaml:"vm_memory_overhead_base"`        // VM 内存开销基数（默认 42Mi）
	VmMemoryOverheadCoefficient int64  `yaml:"vm_memory_overhead_coefficient"` // VM 内存开销系数（默认 64）
	HostMemoryOverheadBase      string `yaml:"host_memory_overhead_base"`      // 宿主机内存开销基数（默认 24Mi）
	CubeMsgMemoryOverhead       string `yaml:"cube_msg_memory_overhead"`       // CubeMsg 内存开销（默认 16Mi）
	VmCpuOverhead               string `yaml:"vm_cpu_overhead"`                // VM CPU 开销（默认 0）
	HostCpuOverhead             string `yaml:"host_cpu_overhead"`              // 宿主机 CPU 开销（默认 0.3）
}

// ReqTemplateConf 请求模板配置
type ReqTemplateConf struct {
	CubeBoxReqTemplate string         `yaml:"cube_box_req_template"` // CubeBox 请求模板（JSON）
	WhitelistReqTag    map[string]any `yaml:"whitelist_req_tag"`     // 请求标签白名单
}

// CubeEgressConf controls how CubeMaster bakes the CubeEgress root CA
// into freshly-built sandbox rootfs templates so workloads inside the
// sandbox trust the MITM certificates CubeEgress signs at request
// time. See design/cube-egress-ca-bake.md.
// CubeEgressConf 控制 CubeMaster 如何把 CubeEgress 根 CA 烘焙进新构建的沙箱 rootfs 模板，
// 使沙箱内的工作负载信任 CubeEgress 在请求时签发的 MITM 证书
type CubeEgressConf struct {
	// CAPath is the host-side filesystem location of the CubeEgress
	// root certificate (PEM). Empty disables the bake silently —
	// preserves dev/test setups where CubeEgress isn't deployed. The
	// production deployment path drops the CA here as part of
	// up-cube-egress.sh; CubeMaster reads from the same file so the
	// data plane and the bake stay in lock-step on rotation.
	// CAPath 宿主机上 CubeEgress 根证书（PEM）的路径；为空时静默禁用烘焙（保留开发/测试环境）
	CAPath string `yaml:"ca_path"`

	// Required, when true, turns soft skips into hard errors:
	//   - missing CAPath file → fail the template build
	//   - zero bundle/anchor targets matched → fail the template build
	// Production deployments should set this to true so a misdeploy
	// where the CA file is absent fails loudly instead of producing a
	// silently-broken template.
	// Required 为 true 时，把软跳过变成硬错误：CA 文件缺失 / 无匹配目标 → 模板构建失败。
	// 生产环境应设为 true，避免"CA 缺失却静默产出坏模板"
	Required bool `yaml:"required"`
}

// SoftDeletePurgeConf configures the scheduled hard-purge of soft-deleted rows.
// All fields are optional; zero/nil values fall back to safe defaults enforced
// by CubeDB/tombstone.Config.Sanitized (7-day retention, hourly interval). The
// purger is DISABLED when the block or `enable` is absent — the purge is
// irreversible, so it must be opted into explicitly. It only touches the
// verified tombstone tables; see docs/guide/soft-delete-purge.md.
// SoftDeletePurgeConf 配置软删除行的定时硬清理。所有字段可选；零值回退到安全默认值
// （保留 7 天、每小时执行）。缺失该块或 enable 时清理器默认禁用——清理不可逆，必须显式启用
type SoftDeletePurgeConf struct {
	// Enable gates the purger. nil/missing -> DISABLED (default-off): the purge is
	// irreversible, so operators must opt in explicitly (review: an upgrade must
	// not silently hard-delete tombstones that were previously retained forever).
	// Enable 开关清理器。nil/缺失 -> 禁用（默认关闭）：清理不可逆，必须显式开启
	Enable *bool `yaml:"enable"`
	// DryRun selects candidate rows and logs counts but issues no DELETE --
	// use for a safe first rollout against a large existing backlog.
	// DryRun 只选择候选行并记录数量，不执行 DELETE——用于存量积压的安全首次上线
	DryRun bool `yaml:"dry_run"`
	// Retention: rows with deleted_at older than now-Retention are purged.
	// <=0 -> 7-day default; values in (0, 1h) are clamped UP to the 1h minimum
	// (avoids the cutoff>=now foot-gun that would purge seconds-old tombstones).
	// Retention 清理保留期：deleted_at 早于 now-Retention 的行被清理。
	// <=0 用 7 天默认；(0,1h) 之间的值被钳到 1h 下限（避免误删刚软删的墓碑行）
	Retention time.Duration `yaml:"retention"`
	// Interval between purge passes. <=0 -> 1h default; values in (0, 1m) are
	// clamped UP to the 1m minimum.
	// Interval 清理执行间隔。<=0 用 1h 默认；(0,1m) 钳到 1m 下限
	Interval time.Duration `yaml:"interval"`
}

// DefaultCubeEgressCAPath is the canonical install path. Used when
// CubeEgressConf is unset or its CAPath is empty AND Required is true
// (meaning: an operator opted into the strict mode but forgot to
// configure the path; we'd rather try the canonical path than refuse
// to start).
const DefaultCubeEgressCAPath = "/etc/cube/ca/cube-root-ca.crt"

// AppHookConfig 应用钩子配置：按环境变量键组织的钩子列表
type AppHookConfig struct {
	PrestartHookByEnvKeys map[string][]*types.Hook `yaml:"prestart_hook_by_env_keys"` // 按环境变量键的启动前钩子

	VirtiofsCacheHookByEnvKeys map[string]string `yaml:"virtiofs_cache_hook_by_env_keys"` // 按环境变量键的 virtiofs 缓存钩子
}

// HookWhitelist 应用钩子白名单：哪些应用可以挂哪些钩子
type HookWhitelist struct {
	AppsHooks map[string]*AppHookConfig `yaml:"apps_hooks"`
}

// GetDbConfig 返回数据库配置
func GetDbConfig() *DBConfig {
	return cfg.InstanceDBConfig
}

// GetRedisConfig 返回 Redis 配置
func GetRedisConfig() *RedisConf {
	return cfg.RedisConf
}

// GetLogConfig 返回日志配置
func GetLogConfig() *log.Conf {
	return cfg.Log
}

// IsInstanceTypeConfig 判断某实例类型是否在配置中
func IsInstanceTypeConfig(product string) bool {
	if cfg.Scheduler == nil {
		return false
	}
	if cfg.Scheduler.InstanceTypeConf == nil {
		return false
	}
	_, exists := cfg.Scheduler.InstanceTypeConf[product]
	return exists
}

// GetSchedulerInstanceTypeConfs 返回所有配置的实例类型名列表（local.go 的 bufferTaskMap 用它建队列）
func GetSchedulerInstanceTypeConfs() []string {
	if cfg.Scheduler == nil {
		return nil
	}
	if cfg.Scheduler.InstanceTypeConf == nil {
		return nil
	}
	return utils.MapToSlice(cfg.Scheduler.InstanceTypeConf)
}

// GetInstanceTypeOfClusterLabel 根据集群标签反查实例类型（预计算的映射）
//
//go:noinline
func GetInstanceTypeOfClusterLabel(label string) string {
	if cfg.Scheduler == nil {
		return ""
	}
	if cfg.Scheduler.InstanceTypeConf == nil {
		return ""
	}
	if len(cfg.Scheduler.labelRefInstanceTypeMap) == 0 {
		return ""
	}
	return cfg.Scheduler.labelRefInstanceTypeMap[label]
}

// Init 加载并初始化全局配置：
// 从环境变量 CUBE_MASTER_CONFIG_PATH 读配置文件路径，
// 用 hotswap 启动文件监听（支持热更新），经过 preHandle（补默认值）与 validate（校验）后
// 保存到全局 cfg。配置变更时 listener.OnEvent 会重新处理并通知
func Init() (*Config, error) {
	configPath := loadConfigPath()
	if configPath == "" {
		return nil, errors.New("CUBE_MASTER_CONFIG_PATH is empty")
	}
	watcher, err := hotswap.NewWatcher(configPath, 10, &Config{})
	if err != nil {
		return nil, err
	}
	watcher.AppendWatcher(&listener{})
	data, err := watcher.Init()
	if err != nil {
		return nil, err
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

// loadConfigPath 从环境变量读取配置文件路径
func loadConfigPath() string {
	path := os.Getenv("CUBE_MASTER_CONFIG_PATH")
	return path
}

// listener 配置热更新的监听器：文件变化时重新 preHandle + validate 并更新全局 cfg
type listener struct {
}

func (l *listener) OnEvent(data interface{}) {
	conf, err := preHandle(data.(*Config))
	if err != nil {
		CubeLog.Fatalf("preHandle Config:%v fail:%v", data, err)
		return
	}
	err = validate(conf)
	if err != nil {
		CubeLog.Fatalf("validate Config:%v fail:%v", data, err)
		return
	}
	cfg = conf
	notify(conf)
}

// preHandle 配置预处理入口：依次对公共、cubelet、调度器、鉴权配置补默认值
func preHandle(config *Config) (*Config, error) {
	if config == nil {
		return nil, errors.New("config is nil")
	}
	if preComHandleConf(config) != nil {
		return nil, errors.New("preComHandleConf fail")
	}

	if preHandleCubeletConf(config) != nil {
		return nil, errors.New("preHandleCubeletConf fail")
	}

	if preHandleScheduler(config) != nil {
		return nil, errors.New("preHandleScheduler failed")
	}
	if preHandleAuthConf(config) != nil {
		return nil, errors.New("preHandleAuthConf failed")
	}
	return config, nil
}

// preComHandleConf 为通用配置补默认值（HTTP 端口、同步周期、超时等）
func preComHandleConf(config *Config) error {
	if config == nil {
		return errors.New("config is nil")
	}
	if config.Common == nil {
		return errors.New("config.Common is nil")
	}
	if config.Common.HttpPort == 0 {
		config.Common.HttpPort = 8089
	}
	// Default to all interfaces; deployments can override via http_bind.
	// 默认监听所有网卡；部署方可通过 http_bind 覆盖
	if config.Common.HttpBind == "" {
		config.Common.HttpBind = "0.0.0.0"
	}
	if config.Common.ReadTimeout == 0 {
		config.Common.ReadTimeout = 120
	}
	if config.Common.WriteTimeout == 0 {
		config.Common.WriteTimeout = 360
	}
	if config.Common.IdleTimeout == 0 {
		config.Common.IdleTimeout = 360
	}

	if config.Common.CubeOpsBootRetries == 0 {
		// 1s base + 5 retries → ~31s wait window covers a slow CubeOps start.
		config.Common.CubeOpsBootRetries = 5
	}
	if config.Common.CubeOpsBootBackoff == time.Duration(0) {
		config.Common.CubeOpsBootBackoff = 1 * time.Second
	}

	if config.Common.SyncMetaDataInterval == time.Duration(0) {
		// Node changes must converge within ~1s for fast scheduling reaction.
		config.Common.SyncMetaDataInterval = 1 * time.Second
	}

	if config.Common.SyncMetricDataInterval == time.Duration(0) {
		config.Common.SyncMetricDataInterval = 1 * time.Second
	}

	if config.Common.CleanSandboxCacheInterval == time.Duration(0) {
		config.Common.CleanSandboxCacheInterval = 2 * time.Hour
	}

	if config.Common.GraceFullStopTimeoutInSec == 0 {
		config.Common.GraceFullStopTimeoutInSec = 120
	}
	if config.Common.AsyncTaskQueueSize == 0 {
		config.Common.AsyncTaskQueueSize = 10000
	}

	if config.Common.AsyncTaskWorkerNum == 0 {
		config.Common.AsyncTaskWorkerNum = runtime.NumCPU()
	}
	if config.Common.DefaultHeadlessServiceNodesNum == 0 {
		config.Common.DefaultHeadlessServiceNodesNum = 1
	}

	if config.Common.CollectMetricInterval == time.Duration(0) {
		config.Common.CollectMetricInterval = 100 * time.Millisecond
	}

	if config.Common.GwCacheExpiredTime == time.Duration(0) {
		config.Common.GwCacheExpiredTime = 15 * time.Second
	}

	if config.Common.DescribeTaskExpireTime == 0 {
		config.Common.DescribeTaskExpireTime = 86400
	}

	if config.RedisConf != nil {
		if config.RedisConf.NodeMetricTTLSec == 0 {
			// Node metrics are rewritten on every heartbeat, so a short safety
			// TTL only auto-cleans offline nodes and never expires live ones.
			// 节点指标每次心跳都会重写，所以较短的 TTL 只会自动清理离线节点，不会误删存活节点
			config.RedisConf.NodeMetricTTLSec = 600
		}
		// SandboxProxyTTLSec intentionally has no positive default: the proxy
		// route key is written once at sandbox creation with no refresh path, so
		// any TTL shorter than the max sandbox lifetime would expire a live
		// route and break CubeProxy. Lifecycle is managed by explicit DEL.
		// Leave it 0 (disabled) unless a refresh mechanism is added first.
		// SandboxProxyTTLSec 故意不设正数默认值：路由键只在创建时写一次、无刷新路径，
		// 任何短于沙箱最大生命周期的 TTL 都会把存活路由过期掉、弄断 CubeProxy。
		// 生命周期由显式 DEL 管理，默认保持 0（禁用）
	}
	if config.Common.DbMaxRetryCount == 0 {
		config.Common.DbMaxRetryCount = 5
	}
	if config.Common.DbRetryInterval == 0 {
		config.Common.DbRetryInterval = 5 * time.Millisecond
	}

	if config.CubeProxyConf == nil {
		config.CubeProxyConf = &CubeProxyConf{}
	}
	if config.CubeProxyConf.AdminPort == 0 {
		config.CubeProxyConf.AdminPort = 8082
	}
	if config.CubeProxyConf.HeartbeatTTLMs == 0 {
		config.CubeProxyConf.HeartbeatTTLMs = 15000
	}

	if config.Common.MaxNICQueue == 0 {
		config.Common.MaxNICQueue = 4
	}
	return nil
}

// preHandleAuthConf 为鉴权配置补默认值
func preHandleAuthConf(config *Config) error {
	if config.AuthConf == nil {
		config.AuthConf = &AuthConf{}
	}
	if config.AuthConf.SignatureExpireTimeInsec == 0 {
		config.AuthConf.SignatureExpireTimeInsec = 120
	}

	return nil
}

// preHandleCubeletConf 为 cubelet 交互配置补默认值（超时、重试、并发限制、缓冲队列等）
func preHandleCubeletConf(config *Config) error {
	if config.CubeletConf == nil {
		config.CubeletConf = &CubeletConf{}
	}
	if config.CubeletConf.CreateImageTimeoutInSec == 0 {
		config.CubeletConf.CreateImageTimeoutInSec = 300
	}

	if config.CubeletConf.BufferQueueMinJob == 0 {
		config.CubeletConf.BufferQueueMinJob = 10
	}

	if config.CubeletConf.CreateConcurrentLimit == 0 {
		config.CubeletConf.CreateConcurrentLimit = 100
	}

	if config.CubeletConf.DestroyConcurentLimit == 0 {
		config.CubeletConf.DestroyConcurentLimit = 50
	}

	if config.CubeletConf.Grpc == nil {
		config.CubeletConf.Grpc = &GrpcConf{}
	}

	if config.CubeletConf.Grpc.CleanConnTaskIntervalInMin == 0 {
		config.CubeletConf.Grpc.CleanConnTaskIntervalInMin = 60
	}

	if config.CubeletConf.Grpc.CleanConnTaskRoutinePoolSize == 0 {
		config.CubeletConf.Grpc.CleanConnTaskRoutinePoolSize = runtime.NumCPU() * 2
	}

	if config.CubeletConf.Grpc.ConnExpireTimeInSec == 0 {
		config.CubeletConf.Grpc.ConnExpireTimeInSec = 180
	}
	if config.CubeletConf.Grpc.GrpcPort == 0 {
		config.CubeletConf.Grpc.GrpcPort = 9999
	}

	if config.CubeletConf.CommonTimeoutInsec == 0 {
		config.CubeletConf.CommonTimeoutInsec = 30
	}
	// DefaultTimeoutInsec is left untouched — see docs/guide/lifecycle.md.
	// DefaultTimeoutInsec 有意不设默认值——见 docs/guide/lifecycle.md
	if config.CubeletConf.CreateTimeoutInsec <= 0 {
		config.CubeletConf.CreateTimeoutInsec = 600
	}
	if config.CubeletConf.MaxRetries == 0 {
		config.CubeletConf.MaxRetries = 5
	}
	if config.CubeletConf.LoopMaxRetries == 0 {
		config.CubeletConf.LoopMaxRetries = 100
	}

	if config.CubeletConf.MaxDelayInSecond == 0 {
		config.CubeletConf.MaxDelayInSecond = 1
	}

	if config.CubeletConf.BackoffRetryDelay == time.Duration(0) {
		config.CubeletConf.BackoffRetryDelay = 5 * time.Millisecond
	}

	return nil
}

// preHandOverhead 为虚拟化开销配置补默认值
func preHandOverhead(config *Config) error {
	if config.Scheduler.Overhead == nil {
		config.Scheduler.Overhead = &OverheadConf{}
	}
	if config.Scheduler.Overhead.VmMemoryOverheadBase == "" {
		config.Scheduler.Overhead.VmMemoryOverheadBase = "42Mi"
	}
	if config.Scheduler.Overhead.VmMemoryOverheadCoefficient == 0 {
		config.Scheduler.Overhead.VmMemoryOverheadCoefficient = 64
	}
	if config.Scheduler.Overhead.VmCpuOverhead == "" {
		config.Scheduler.Overhead.VmCpuOverhead = "0"
	}
	if config.Scheduler.Overhead.HostCpuOverhead == "" {
		config.Scheduler.Overhead.HostCpuOverhead = "0.3"
	}
	if config.Scheduler.Overhead.HostMemoryOverheadBase == "" {
		config.Scheduler.Overhead.HostMemoryOverheadBase = "24Mi"
	}
	if config.Scheduler.Overhead.CubeMsgMemoryOverhead == "" {
		config.Scheduler.Overhead.CubeMsgMemoryOverhead = "16Mi"
	}
	return nil
}

// preHandleScheduler 为调度器配置补默认值：
// 超卖比例、节点上限、过滤/评分插件参数、选择算法名等
func preHandleScheduler(config *Config) error {
	if config.Scheduler == nil {
		config.Scheduler = &WrapperSchedulerConf{}
	}

	preHandOverhead(config)

	// Account for Redis allocation records during scheduling by default.
	// 默认在调度时计入 Redis 中的已分配记录
	if config.Scheduler.IgnoreRedisAllocation == nil {
		ignore := false
		config.Scheduler.IgnoreRedisAllocation = &ignore
	}
	// Default overcommit ratio: CPU=3, Mem=2. sanitized() guards against
	// non-positive, NaN, or infinite values supplied by operators.
	// 默认超卖比例 CPU=3、Mem=2；sanitized() 防御运维配置的非正数/NaN/无穷值
	if config.Scheduler.OvercommitRatio == nil {
		config.Scheduler.OvercommitRatio = &OvercommitRatioConf{
			CPURatio: defaultCPUOvercommitRatio,
			MemRatio: defaultMemOvercommitRatio,
		}
	} else {
		sanitized := config.Scheduler.OvercommitRatio.sanitized()
		config.Scheduler.OvercommitRatio = &sanitized
	}
	// Sanitize per-instance-type overrides at init time as well so malformed
	// (non-positive/NaN/Inf) ratios are normalized once up front rather than
	// relying solely on the lazy sanitize in GetEffectiveOvercommitRatio.
	// 实例类型级覆盖也在初始化时统一 sanitize，避免每次 GetEffectiveOvercommitRatio 才惰性修正
	for k, v := range config.Scheduler.OvercommitRatioByType {
		config.Scheduler.OvercommitRatioByType[k] = v.sanitized()
	}

	if config.Scheduler.NodeMaxMvmNum == 0 {
		config.Scheduler.NodeMaxMvmNum = 3000
	}
	if config.Scheduler.NodeMaxMvmNumReserveNumPercent == 0.0 {
		config.Scheduler.NodeMaxMvmNumReserveNumPercent = 1.0
	}

	if config.Scheduler.NodeMaxCpuUtil == 0 {
		config.Scheduler.NodeMaxCpuUtil = 80.0
	}
	if config.Scheduler.DiskUsageMaxPercent == 0 {
		config.Scheduler.DiskUsageMaxPercent = 80.0
	}

	if config.Scheduler.NodeMaxMemReservedInMB == 0 {
		config.Scheduler.NodeMaxMemReservedInMB = 10 * 1024
	}
	if config.Scheduler.PreSelectNum == 0 {
		config.Scheduler.PreSelectNum = -1
	}
	if config.Scheduler.PrioritySelectNum == 0 {
		config.Scheduler.PrioritySelectNum = -1
	}

	if config.Scheduler.LeastSelectName == "" {
		config.Scheduler.LeastSelectName = "random"
	}

	if config.Scheduler.MetricUpdateTimeout == time.Duration(0) {
		config.Scheduler.MetricUpdateTimeout = time.Hour
	}

	if config.Scheduler.LocalMetricUpdateTimeout == time.Duration(0) {
		config.Scheduler.LocalMetricUpdateTimeout = time.Hour
	}
	if config.Scheduler.MaxMvmCPU == "" {
		config.Scheduler.maxCpu = resource.MustParse("100")
	} else {
		config.Scheduler.maxCpu = resource.MustParse(config.Scheduler.MaxMvmCPU)
	}

	if config.Scheduler.MaxMvmMemory == "" {
		config.Scheduler.maxMem = resource.MustParse("300Gi")
	} else {
		config.Scheduler.maxMem = resource.MustParse(config.Scheduler.MaxMvmMemory)
	}

	if config.Scheduler.LargeSizeAffinityConf != nil {
		for _, v := range config.Scheduler.LargeSizeAffinityConf {
			if !v.Enable {
				continue
			}
			if !utils.Contains(v.Operator, []string{"Gt", "Lt"}) {
				v.Enable = false
				fmt.Printf("Scheduler.LargeSizeAffinityConf invalid op:%s", v.Operator)
			}
			if v.MemoryLowerWaterMark != "" {
				if _, err := resource.ParseQuantity(v.MemoryLowerWaterMark); err != nil {
					v.Enable = false
					fmt.Printf("Scheduler.LargeSizeAffinityConf invalid MemoryLowerWaterMark:%s", v.MemoryLowerWaterMark)
				}
			}
			if v.CpuLowerWaterMark != "" {
				if _, err := resource.ParseQuantity(v.CpuLowerWaterMark); err != nil {
					v.Enable = false
					fmt.Printf("Scheduler.LargeSizeAffinityConf invalid CpuLowerWaterMark:%s", v.CpuLowerWaterMark)
				}
			}
		}
	}

	preHandSchedulerScore(config)

	if err := checkInstanceTypeLabelValid(config); err != nil {
		return err
	}
	return nil
}

// checkInstanceTypeLabelValid 校验实例类型标签唯一性：
// 构建 集群标签→实例类型 的映射，并检查同一标签没有被多个实例类型引用
func checkInstanceTypeLabelValid(config *Config) error {
	if config.Scheduler == nil {
		return nil
	}

	if config.Scheduler.InstanceTypeConf == nil {
		return nil
	}

	config.Scheduler.labelRefInstanceTypeMap = make(map[string]string)

	labelRefCnt := make(map[string]int)
	for instanceType, v := range config.Scheduler.InstanceTypeConf {
		for k := range v.OssClusterLabels {
			labelRefCnt[k]++
			config.Scheduler.labelRefInstanceTypeMap[k] = instanceType
		}
	}

	for label, cnt := range labelRefCnt {
		if cnt > 1 {
			return fmt.Errorf("label %s is used by multiple product types", label)
		}
	}
	return nil
}

// preHandSchedulerScore 为评分配置补默认值：
// 多因子评分的刷新周期、postscore 的参数因子与白名单 map 转换
func preHandSchedulerScore(config *Config) {
	if config.Scheduler.Score != nil {
		if asynccfg := config.Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage; asynccfg != nil {
			if asynccfg.ScoreInterval == time.Duration(0) {
				asynccfg.ScoreInterval = config.Common.SyncMetricDataInterval
			}
		}
	}

	if config.Scheduler.PostScore != nil {
		if config.Scheduler.PostScore.ParamFactor == 0.0 {
			config.Scheduler.PostScore.ParamFactor = 0.015
		}
		config.Scheduler.PostScore.ActiveWhiteListMap = make(map[string]bool)
		for _, v := range config.Scheduler.PostScore.ActiveWhiteList {
			config.Scheduler.PostScore.ActiveWhiteListMap[v] = true
		}
		config.Scheduler.PostScore.NegativeWhiteListMap = make(map[string]bool)
		for _, v := range config.Scheduler.PostScore.NegativeWhiteList {
			config.Scheduler.PostScore.NegativeWhiteListMap[v] = true
		}
	}
}

// validate 配置校验：检查日志、扩展配置（QoS JSON 合法性）与挂载目录白名单
func validate(cfg *Config) error {
	if cfg.Log == nil {
		return errors.New("log config is nil. ")
	}
	if cfg.ExtraConf == nil {
		cfg.ExtraConf = &ExtraConf{}
	}
	if strings.TrimSpace(cfg.ExtraConf.BlkQos) == "" {
		cfg.ExtraConf.BlkQos = "{}"
	}
	if strings.TrimSpace(cfg.ExtraConf.FsQos) == "" {
		cfg.ExtraConf.FsQos = "{}"
	}
	if strings.TrimSpace(cfg.ExtraConf.NetQosList) == "" {
		cfg.ExtraConf.NetQosList = "[]"
	}
	if !json.Valid([]byte(cfg.ExtraConf.BlkQos)) {
		return errors.New("BlkQos config is not json. ")
	}

	if !json.Valid([]byte(cfg.ExtraConf.FsQos)) {
		return errors.New("FsQos config is not json. ")
	}
	if !json.Valid([]byte(cfg.ExtraConf.NetQosList)) {
		return errors.New("NetQosList config is not json. ")
	}
	for _, v := range cfg.ExtraConf.BlkQosMap {
		if !json.Valid([]byte(v)) {
			return errors.New("BlkQos config is not json. ")
		}
	}
	for _, v := range cfg.ExtraConf.FsQosMap {
		if !json.Valid([]byte(v)) {
			return errors.New("FsQos config is not json. ")
		}
	}
	if cfg.ReqTemplateConf != nil {
		if cfg.ReqTemplateConf.CubeBoxReqTemplate != "" {
			if !json.Valid([]byte(cfg.ReqTemplateConf.CubeBoxReqTemplate)) {
				return errors.New("CubeBoxReqTemplate config is not json. ")
			}
		}
	}
	for _, p := range cfg.ExtraConf.AllowedHostMountPrefixes {
		cleaned := filepath.Clean(p)
		if cleaned == "/" || cleaned == "." || !filepath.IsAbs(p) {
			return fmt.Errorf("allowed_host_mount_prefixes entry %q must be an absolute path and must not be root or empty", p)
		}
	}
	return nil
}

// GetConfig 返回全局配置实例
//
//go:noinline
func GetConfig() *Config {
	return cfg
}

// defaultAllowedHostMountPrefixes 默认允许的宿主机挂载目录前缀
var defaultAllowedHostMountPrefixes = []string{"/data/shared/"}

// GetAllowedHostMountPrefixes returns the configured allowed host-mount
// prefixes, defaulting to ["/data/shared/"] when not configured.
// Trailing "/" is auto-appended if missing. Returns a defensive copy.
// GetAllowedHostMountPrefixes 返回配置的宿主机挂载前缀白名单（未配置时默认 ["/data/shared/"]）；
// 缺尾部 "/" 自动补上；返回防御性拷贝
func GetAllowedHostMountPrefixes() []string {
	c := cfg
	if c == nil || c.ExtraConf == nil || len(c.ExtraConf.AllowedHostMountPrefixes) == 0 {
		return append([]string{}, defaultAllowedHostMountPrefixes...)
	}
	raw := c.ExtraConf.AllowedHostMountPrefixes
	result := make([]string, len(raw))
	for i, p := range raw {
		if !strings.HasSuffix(p, "/") {
			result[i] = p + "/"
		} else {
			result[i] = p
		}
	}
	return result
}

// notify 通知所有已注册的配置监听器配置已更新
func notify(config *Config) {
	for _, l := range listeners {
		l.OnEvent(config)
	}
}

// Watcher 配置变更监听器接口
type Watcher interface {
	OnEvent(data *Config)
}

var listeners []Watcher
var listenerMutex sync.RWMutex

// AppendConfigWatcher 注册配置变更监听器（如调度器等模块需要感知配置变化时调用）
func AppendConfigWatcher(listener Watcher) {
	listenerMutex.Lock()
	defer listenerMutex.Unlock()
	listeners = append(listeners, listener)
}

// IsAppHooks 判断某应用是否配置了钩子
func IsAppHooks(app string) bool {
	if cfg == nil {
		return false
	}
	if cfg.HookWhitelist == nil || cfg.HookWhitelist.AppsHooks == nil {
		return false
	}
	_, ok := cfg.HookWhitelist.AppsHooks[app]
	if !ok {
		return false
	}
	return true
}

// HasEnvPrestartHook 按应用与环境变量键返回启动前钩子列表
func HasEnvPrestartHook(app string, envKey string) []*types.Hook {
	if cfg == nil {
		return nil
	}
	if cfg.HookWhitelist == nil || cfg.HookWhitelist.AppsHooks == nil {
		return nil
	}
	v, ok := cfg.HookWhitelist.AppsHooks[app]
	if !ok || v == nil {
		return nil
	}
	hooks, ok := v.PrestartHookByEnvKeys[envKey]
	if !ok {
		return nil
	}
	return hooks
}

// HasEnvVirtiofsCacheHook 按应用与环境变量键返回 virtiofs 缓存钩子
func HasEnvVirtiofsCacheHook(app string, envKey string) string {
	if cfg == nil {
		return ""
	}
	if cfg.HookWhitelist == nil || cfg.HookWhitelist.AppsHooks == nil {
		return ""
	}
	v, ok := cfg.HookWhitelist.AppsHooks[app]
	if !ok || v == nil {
		return ""
	}
	cache, ok := v.VirtiofsCacheHookByEnvKeys[envKey]
	if !ok {
		return ""
	}

	return cache
}
