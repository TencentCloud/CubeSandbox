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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/hotswap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/api/resource"
)

var cfg *Config

type Config struct {
	Common            *CommonConf           `yaml:"common"`
	AuthConf          *AuthConf             `yaml:"auth"`
	Log               *log.Conf             `yaml:"log"`
	CubeletConf       *CubeletConf          `yaml:"cubelet_conf"`
	OssDBConfig       *DBConfig             `yaml:"ossdb_config"`
	InstanceDBConfig  *DBConfig             `yaml:"instance_db_config"`
	RedisConf         *RedisConf            `yaml:"redis"`
	RedisReadConf     *RedisConf            `yaml:"redis_read"`
	RedisWriteConf    *RedisConf            `yaml:"redis_write"`
	RedisMetadataConf *RedisConf            `yaml:"redis_metadata"`
	ExtraConf         *ExtraConf            `yaml:"extra_conf"`
	Scheduler         *WrapperSchedulerConf `yaml:"scheduler"`
	ReqTemplateConf   *ReqTemplateConf      `yaml:"req_template_conf"`
	HookWhitelist     *HookWhitelist        `yaml:"hook_whitelist"`
}

type CommonConf struct {
	MockUpdateAction                bool              `yaml:"mock_update_action"`
	DebugDumpHttpBody               bool              `yaml:"debug_dump_http_body"`
	MockDebug                       bool              `yaml:"mock_debug"`
	MockNodeNum                     int               `yaml:"mock_node_num"`
	MockCreateDirect                bool              `yaml:"mock_create_direct"`
	MockCreateDirectHandle          bool              `yaml:"mock_create_direct_handle"`
	MockHttpDirect                  bool              `yaml:"mock_http_direct"`
	MockCreateSleep                 time.Duration     `yaml:"mock_create_sleep"`
	MockPercents                    []float64         `yaml:"mock_percents"`
	CubeDestroyCheckFilter          bool              `yaml:"cube_destroy_check_filter"`
	Debug                           Debug             `toml:"debug"`
	HttpPort                        int               `yaml:"http_port"`
	WriteTimeout                    int               `yaml:"http_writetimeout"`
	ReadTimeout                     int               `yaml:"http_readtimeout"`
	IdleTimeout                     int               `yaml:"http_idletimeout"`
	GraceFullStopTimeoutInSec       int               `yaml:"gracefull_stop_timeout_insec"`
	SyncMetaDataInterval            time.Duration     `yaml:"sync_meta_data_interval"`
	SyncMetricDataInterval          time.Duration     `yaml:"sync_metric_data_interval"`
	CleanSandboxCacheInterval       time.Duration     `yaml:"clean_sandbox_cache_interval"`
	EnabledListRunningSandboxCache  bool              `yaml:"enabled_list_running_sandbox_cache"`
	AsyncTaskQueueSize              int               `yaml:"async_task_queue_size"`
	AsyncTaskWorkerNum              int               `yaml:"async_task_worker_num"`
	HeadlessServiceName             string            `yaml:"headless_service_name"`
	DefaultHeadlessServiceNodesNum  int64             `yaml:"default_headless_service_nodes_num"`
	ListFilterOutLabels             map[string]string `yaml:"list_filter_out_lables"`
	CollectMetricInterval           time.Duration     `yaml:"collect_metric_interval"`
	ReportLocalCreateNum            bool              `yaml:"report_local_create_num"`
	ReportStdevMetric               bool              `yaml:"report_stdev_metric"`
	GwCacheExpiredTime              time.Duration     `yaml:"gw_cache_expired_time"`
	GwCacheEnable                   bool              `yaml:"gw_cache_enable"`
	ReportGWRedisGetMetric          bool              `yaml:"report_gw_redis_get_metric"`
	EnableGetStatusFromCubelet      bool              `yaml:"enable_get_status_from_cubelet"`
	DisableHardDelete               bool              `yaml:"disable_hard_delete"`
	CollectSandboxMemoryWhitelist   []string          `yaml:"collect_sandbox_memory_whitelist"`
	EnableAllCollectSandboxMemory   bool              `yaml:"enable_all_collect_sandbox_memory"`
	FilterErrMsgErrorCode           map[int]bool      `yaml:"filter_err_msg_error_code"`
	DescribeInstancesWhiteList      map[string]bool   `yaml:"describe_instances_white_list"`
	DescribeTaskExpireTime          int               `yaml:"describe_task_expire_time"`
	EnablePrivateIpQuery            bool              `yaml:"enable_private_ip_query"`
	DbMaxRetryCount                 int               `yaml:"db_max_retry_count"`
	DbRetryInterval                 time.Duration     `yaml:"db_retry_interval"`
	EnableCheckComNetIDParam        bool              `yaml:"enable_check_com_net_id_param"`
	EnableDescribeInstanceFromRedis bool              `yaml:"enable_describe_instance_from_redis"`
	MaxNICQueue                     int               `yaml:"max_nic_queue"`
	DisableCreateImageCluster       map[string]bool   `yaml:"disable_create_image_cluster"`
	EnableAGSColdStartSwitch        bool              `yaml:"enable_ags_cold_start_switch"`
}

type AuthConf struct {
	Enable                   bool                         `yaml:"enable"`
	SignatureExpireTimeInsec int64                        `yaml:"signature_expire_time_insec"`
	SecretKeyMap             map[string]map[string]string `yaml:"secret_key_map"`
}

type Debug struct {
	Address string `toml:"address"`
}

type DBConfig struct {
	Addr                   string `yaml:"addr"`
	User                   string `yaml:"user"`
	Pwd                    string `yaml:"pwd"`
	DBName                 string `yaml:"db_name"`
	ConnTimeout            int    `yaml:"conn_timeout"`
	ReadTimeout            int    `yaml:"read_timeout"`
	WriteTimeout           int    `yaml:"write_timeout"`
	MaxIdleConns           int    `yaml:"max_idle_conns"`
	MaxOpenConns           int    `yaml:"max_open_conns"`
	MaxConnLifeTimeSeconds int    `yaml:"max_conn_life_time_seconds"`
}

type ExtraConf struct {
	BlkQos     string            `yaml:"blk_qos"`
	BlkQosMap  map[string]string `yaml:"blk_qos_map"`
	FsQos      string            `yaml:"fs_qos"`
	FsQosMap   map[string]string `yaml:"fs_qos_map"`
	NetQosList string            `yaml:"net_qos_list"`
}

type RedisConf struct {
	Password    string `yaml:"password"`
	MaxActive   int    `yaml:"max_active"`
	MaxIdle     int    `yaml:"max_idle"`
	IdleTimeout int    `yaml:"idle_timeout"`
	DbNo        int    `yaml:"db_no"`

	Nodes    string `yaml:"nodes"`
	MaxRetry int    `yaml:"max_retry"`
}

type SchedulerConf struct {
	Overhead                         *OverheadConf                `yaml:"overhead"`
	NodeMaxMvmNum                    int64                        `yaml:"node_max_mvm_num"`
	NodeMaxMvmNumReserveNumPercent   float64                      `yaml:"node_max_mvm_num_reserve_num_percent"`
	NodeMaxMemReservedInMB           int64                        `yaml:"node_max_mem_reserved_in_mb"`
	NodeMaxCpuUtil                   float64                      `yaml:"node_max_cpu_util"`
	PreSelectNum                     int                          `yaml:"pre_select_num"`
	PrioritySelectNum                int                          `yaml:"priority_select_num"`
	LeastSelectName                  string                       `yaml:"least_select_name"`
	MetricUpdateTimeout              time.Duration                `yaml:"metric_update_timeout"`
	LocalMetricUpdateTimeout         time.Duration                `yaml:"local_metric_update_timeout"`
	Filter                           *SchedulerFilterConf         `yaml:"filter"`
	Score                            *SchedulerScoreConf          `yaml:"score"`
	PostScore                        *PostScoreConf               `yaml:"postscore"`
	DisableCircuitFilter             bool                         `yaml:"disable_circuit_filter"`
	InBackoffMode                    bool                         `yaml:"in_backoff_mode"`
	AffinityConf                     map[string]AffinityConf      `yaml:"affinityconf"`
	NodeMaxMvmNumConf                map[string]NodeMaxMvmNumConf `yaml:"node_max_mvm_num_conf"`
	EnableRunInstanceHostIps         bool                         `yaml:"enable_run_instance_host_ips"`
	MaxMvmCPU                        string                       `yaml:"max_mvm_cpu"`
	maxCpu                           resource.Quantity
	MaxMvmMemory                     string `yaml:"max_mvm_memory"`
	maxMem                           resource.Quantity
	DiskUsageMaxPercent              float64                           `yaml:"disk_usage_max_percent"`
	LargeSizeAffinityConf            map[string]LargeSizeAffinityConf  `yaml:"large_size_affinity_conf"`
	NodeMaxMemReservedConf           map[string]NodeMaxMemReservedConf `yaml:"node_max_mem_reserved_conf"`
	DisableBackoffFilterInstanceType map[string]bool                   `yaml:"disable_backoff_filter_instance_type"`
	ThirtpartyFilterInstanceType     map[string]bool                   `yaml:"thirtparty_filter_instance_type"`
	InstanceTypeConf                 map[string]InstanceTypeConf       `yaml:"instance_type_conf"`
}

type WrapperSchedulerConf struct {
	SchedulerConf           `yaml:",inline"`
	labelRefInstanceTypeMap map[string]string
}

type InstanceTypeConf struct {
	OssClusterLabels map[string]any `yaml:"oss_cluster_labels"`
}

type LargeSizeAffinityConf struct {
	Enable               bool           `yaml:"enable"`
	MemoryLowerWaterMark string         `yaml:"memory_lower_water_mark"`
	CpuLowerWaterMark    string         `yaml:"cpu_lower_water_mark"`
	Operator             string         `yaml:"operator"`
	ClusterLabels        map[string]any `yaml:"cluster_labels"`
}

type NodeMaxMvmNumConf struct {
	MvmNum                  int64   `yaml:"mvm_num"`
	MvmNumReserveNumPercent float64 `yaml:"mvm_num_reserve_num_percent"`
}

type NodeMaxMemReservedConf struct {
	MaxMemReservedInMB        int64   `yaml:"max_mem_reserved_in_mb"`
	MaxMemReservedInMBPercent float64 `yaml:"max_mem_reserved_in_mb_percent"`
}

type AffinityConf struct {
	Enable            bool           `yaml:"enable"`
	DisableVmCgroup   bool           `yaml:"disable_vm_cgroup"`
	DisableHostCgroup bool           `yaml:"disable_host_cgroup"`
	ClusterLabels     map[string]any `yaml:"cluster_labels"`
}

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

func (s *SchedulerConf) GetLargeSizeAffinityConf(serviceName string) LargeSizeAffinityConf {
	if s.LargeSizeAffinityConf == nil {
		return LargeSizeAffinityConf{
			Enable: false,
		}
	}
	return s.LargeSizeAffinityConf[serviceName]
}

func (s *SchedulerConf) MaxMvmCPURes() resource.Quantity {
	if s.maxCpu.IsZero() {
		return resource.MustParse(s.MaxMvmCPU)
	}
	return s.maxCpu
}

func (s *SchedulerConf) MaxMvmMemoryRes() resource.Quantity {
	if s.maxMem.IsZero() {
		return resource.MustParse(s.MaxMvmMemory)
	}
	return s.maxMem
}

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

type SchedulerFilterConf struct {
	EnableFilters []string `yaml:"enable_filters"`
}

type PostScoreConf struct {
	Disable              bool               `yaml:"disable"`
	ParamFactor          float64            `yaml:"param_factor"`
	ResourceWeights      map[string]float64 `yaml:"resource_weights"`
	ActiveWhiteList      []string           `yaml:"active_white_list"`
	ActiveWhiteListMap   map[string]bool    `yaml:"-"`
	NegativeWhiteList    []string           `yaml:"negative_white_list"`
	NegativeWhiteListMap map[string]bool    `yaml:"-"`
}

type SchedulerScoreConf struct {
	EnableScorers   []string           `yaml:"enable_scorers"`
	ResourceWeights map[string]float64 `yaml:"resource_weights"`
	ScorePluginConf ScorePluginConf    `yaml:"plugin_conf"`
}

type ScorePluginConf struct {
	MultiFactorWeightedAverage *MultiFactorWeightedAverage `yaml:"multi_factor_weighted_average"`
	RealTimeWeightedAverage    *RealTimeWeightedAverage    `yaml:"real_time_weighted_average"`
	AffinityScore              *AffinityScore              `yaml:"affinity_score"`
	ImageScore                 *ImageScore                 `yaml:"image_score"`
	TemplateScore              *TemplateScore              `yaml:"template_score"`
}

type MultiFactorWeightedAverage struct {
	ScoreInterval       time.Duration `yaml:"score_interval"`
	Weight              float64       `yaml:"weight"`
	EnableWeightFactors []string      `yaml:"enable_weight_factors"`
	Disable             bool          `yaml:"disable"`
}

type RealTimeWeightedAverage struct {
	Weight              float64  `yaml:"weight"`
	EnableWeightFactors []string `yaml:"enable_weight_factors"`
	Disable             bool     `yaml:"disable"`
}

type AffinityScore struct {
	Weight              float64  `yaml:"weight"`
	EnableWeightFactors []string `yaml:"enable_weight_factors"`
	Disable             bool     `yaml:"disable"`
}

type ImageScore struct {
	Weight              float64  `yaml:"weight"`
	EnableWeightFactors []string `yaml:"enable_weight_factors"`
	Disable             bool     `yaml:"disable"`
}

type TemplateScore struct {
	Weight              float64  `yaml:"weight"`
	EnableWeightFactors []string `yaml:"enable_weight_factors"`
	Disable             bool     `yaml:"disable"`
}

type CubeletConf struct {
	Grpc                    *GrpcConf            `yaml:"grpc"`
	CommonTimeoutInsec      int                  `yaml:"common_timeout_insec"`
	CreateImageTimeoutInSec int                  `yaml:"create_image_timeout_insec"`
	AsyncFlows              map[string]asyncFlow `yaml:"async_flows"`
	RetryCode               []string             `yaml:"retry_code"`
	LoopRetryCode           []string             `yaml:"loop_retry_code"`
	ReuseRetryCode          []string             `yaml:"reuse_retry_code"`
	CircuitBreakCode        []string             `yaml:"circuit_break_code"`
	ExcludeLoopRetryCode    []string             `yaml:"exclude_loop_retry_code"`
	BackoffRetryCode        []string             `yaml:"backoff_retry_code"`
	MaxRetries              int64                `yaml:"max_retries"`
	LoopMaxRetries          int64                `yaml:"loop_max_retries"`
	BufferQueueMinJob       int64                `yaml:"buffer_queue_min_job"`
	CreateConcurrentLimit   int64                `yaml:"create_concurrent_limit"`
	DestroyConcurrentLimit   int64                `yaml:"destroy_concurent_limit"`
	ExposedPortList         []string             `yaml:"exposed_port_list"`
	EnableExposedPort       bool                 `yaml:"enable_exposed_port"`
	DisableRedisProxyPort   bool                 `yaml:"disable_redis_proxy_port"`
	MaxDelayInSecond        int64                `yaml:"max_delay_in_second"`
	BackoffRetryDelay       time.Duration        `yaml:"backoff_retry_delay"`
}

type GrpcConf struct {
	GrpcPort                     int `yaml:"grpc_port"`
	CleanConnTaskIntervalInMin   int `yaml:"clean_conn_task_interval_in_min"`
	CleanConnTaskRoutinePoolSize int `yaml:"clean_conn_task_routine_pool_size"`
	ConnExpireTimeInSec          int `yaml:"conn_expire_time_insec"`
}

type asyncFlow struct {
	MaxConcurrent  int64 `yaml:"concurrent"`
	MaxRetries     int64 `yaml:"max_retries"`
	LoopMaxRetries int64 `yaml:"loop_max_retries"`
}

type OverheadConf struct {
	VmMemoryOverheadBase        string `yaml:"vm_memory_overhead_base"`
	VmMemoryOverheadCoefficient int64  `yaml:"vm_memory_overhead_coefficient"`
	HostMemoryOverheadBase      string `yaml:"host_memory_overhead_base"`
	CubeMsgMemoryOverhead       string `yaml:"cube_msg_memory_overhead"`
	VmCpuOverhead               string `yaml:"vm_cpu_overhead"`
	HostCpuOverhead             string `yaml:"host_cpu_overhead"`
}

type ReqTemplateConf struct {
	CubeBoxReqTemplate string         `yaml:"cube_box_req_template"`
	WhitelistReqTag    map[string]any `yaml:"whitelist_req_tag"`
}

type AppHookConfig struct {
	PrestartHookByEnvKeys map[string][]*types.Hook `yaml:"prestart_hook_by_env_keys"`

	VirtiofsCacheHookByEnvKeys map[string]string `yaml:"virtiofs_cache_hook_by_env_keys"`
}

type HookWhitelist struct {
	AppsHooks map[string]*AppHookConfig `yaml:"apps_hooks"`
}

func GetDbConfig() *DBConfig {
	return cfg.OssDBConfig
}

func GetInstanceConfig() *DBConfig {
	return cfg.InstanceDBConfig
}

func GetRedisConfig() *RedisConf {
	return cfg.RedisConf
}

func GetLogConfig() *log.Conf {
	return cfg.Log
}

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

func GetSchedulerInstanceTypeConfs() []string {
	if cfg.Scheduler == nil {
		return nil
	}
	if cfg.Scheduler.InstanceTypeConf == nil {
		return nil
	}
	return utils.MapToSlice(cfg.Scheduler.InstanceTypeConf)
}

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

func loadConfigPath() string {
	path := os.Getenv("CUBE_MASTER_CONFIG_PATH")
	return path
}

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
	if config.Common.ReadTimeout == 0 {
		config.Common.ReadTimeout = 120
	}
	if config.Common.WriteTimeout == 0 {
		config.Common.WriteTimeout = 360
	}
	if config.Common.IdleTimeout == 0 {
		config.Common.IdleTimeout = 360
	}

	if config.Common.SyncMetaDataInterval == time.Duration(0) {
		config.Common.SyncMetaDataInterval = 30 * time.Second
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
	if config.Common.DbMaxRetryCount == 0 {
		config.Common.DbMaxRetryCount = 5
	}
	if config.Common.DbRetryInterval == 0 {
		config.Common.DbRetryInterval = 5 * time.Millisecond
	}

	if config.Common.MaxNICQueue == 0 {
		config.Common.MaxNICQueue = 4
	}
	return nil
}
func preHandleAuthConf(config *Config) error {
	if config.AuthConf == nil {
		config.AuthConf = &AuthConf{}
	}
	if config.AuthConf.SignatureExpireTimeInsec == 0 {
		config.AuthConf.SignatureExpireTimeInsec = 120
	}

	return nil
}
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

	if config.CubeletConf.DestroyConcurrentLimit == 0 {
		config.CubeletConf.DestroyConcurrentLimit = 50
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
func preHandleScheduler(config *Config) error {
	if config.Scheduler == nil {
		config.Scheduler = &WrapperSchedulerConf{}
	}

	preHandOverhead(config)

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
	return nil
}

//go:noinline
func GetConfig() *Config {
	return cfg
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
