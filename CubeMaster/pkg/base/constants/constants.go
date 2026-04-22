// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package constants contains all of the application wide constant values used throughout Cube Master.
package constants

const (
	SelectorPreFilterID     = "PreFilter"
	SelectorBackoffFilterID = "Backoff"
	SelectorFilterID        = "Filter"
	SelectorScoreID         = "Score"
	SelectorPostScoreID     = "PostScore"
)

const (
	SelectCtxReqKey    = "create_req"
	SelectCtxConfKey   = "select_conf"
	SelectCtxResCpuKey = "create_res_cpu"
	SelectCtxResMemKey = "create_res_mem"
)

const (
	AnnotationsNetID            = "com.netid"
	AnnotationsInvokePort       = "com.invoke_port"
	AnnotationsExposedPort      = "com.exposed_ports"
	AnnotationsVIPs             = "com.vips"
	AnnotationsDebug            = "com.cube.debug"
	AnnotationsCollectMemOnExit = "com.cube.collect_memory"

	AnnotationsNodeAffinityClusterLabel = "com.nodeaffinity.cluster.label"

	AnnotationsNodeAffinityInstanceType = "com.nodeaffinity.instancetype"
)

const (
	CubeAnnotationsPrefix                   = "cube.master"
	CubeAnnotationsVIPs                     = "cube.master.vips"
	CubeAnnotationsBlkQos                   = "cube.master.blk.qos"
	CubeAnnotationsFSQos                    = "cube.master.fs.qos"
	CubeAnnotationsNetWork                  = "cube.master.net"
	CubeAnnotationsImageName                = "cube.master.image.username"
	CubeAnnotationsImageToken               = "cube.master.image.token"
	CubeAnnotationsInsVirtualPrivateCloud   = "cube.master.instance.virtual_private_cloud"
	CubeAnnotationsInsDataDisk              = "cube.master.instance.data_disk"
	CubeAnnotationsInsUserData              = "cube.master.instance.user_data"
	CubeAnnotationsInsType                  = "cube.master.instance.type"
	CubeAnnotationsInsSecurityGroupIDS      = "cube.master.instance.sg_ids"
	CubeAnnotationsInsRetainIP              = "cube.master.instance.retain_ip"
	CubeAnnotationsInsRegion                = "cube.master.instance.region"
	CubeAnnotationsInsHostIP                = "cube.master.instance.host_ip"
	CubeAnnotationsInsHostUUID              = "cube.master.instance.host_uuid"
	CubeAnnotationsInsHostCpuTotal          = "cube.master.instance.host_cpu_total"
	CubeAnnotationsInsHostVirtualQuotaArray = "cube.master.instance.host_virtual_quota_array"
	CubeAnnotationsPICMode                  = "cube.master.instance.pic_mode"
	CubeAnnotationsDisableVmCgroup          = "cube.master.disable_vm_cgroup"
	CubeAnnotationsDisableHostCgroup        = "cube.master.disable_host_cgroup"
	CubeAnnotationsUpdateAction             = "cube.master.update_action"
	CubeAnnotationsTerminateFatalEvent      = "cube.master.terminate_fatal_event"
	CubeAnnotationsCloadPrefix              = "cloud.tencent.com"
	CubeAnnotationsNumaNode                 = "cube.master.instance.numa_node"
	CubeAnnotationsFallbackToSlowPath       = "cube.master.fallback_to_slow_path"
	CubeAnnotationsSystemDiskSize           = "cube.master.system_disk_size"

	CubeAnnotationsAppSnapshotCreate         = "cube.master.appsnapshot.create"
	CubeAnnotationAppSnapshotTemplateID      = "cube.master.appsnapshot.template.id"
	CubeAnnotationAppSnapshotVersion         = "cube.master.appsnapshot.version"
	CubeAnnotationAppSnapshotTemplateVersion = "cube.master.appsnapshot.template.version"
	CubeAnnotationRootfsArtifactID           = "cube.master.rootfs.artifact.id"
	CubeAnnotationRootfsArtifactJobID        = "cube.master.rootfs.artifact.job_id"
	CubeAnnotationRootfsArtifactURL          = "cube.master.rootfs.artifact.url"
	CubeAnnotationRootfsArtifactToken        = "cube.master.rootfs.artifact.token"
	CubeAnnotationRootfsArtifactSHA256       = "cube.master.rootfs.artifact.sha256"
	CubeAnnotationRootfsArtifactSizeBytes    = "cube.master.rootfs.artifact.size_bytes"
	CubeAnnotationWritableLayerSize          = "cube.master.rootfs.writable_layer_size"
	CubeAnnotationTemplateSpecFingerprint    = "cube.master.template.spec_fingerprint"

	CubeAnnotationsVirtiofsCache = "cube.master.virtiofs.cache"
)
const (
	CubeAnnotationsUseNetFileCache = "cube.instance.use_netfile_cache"
	CubeAnnotationCollectMemOnExit = "cube.instance.collect_memory"

	UpdateActionAddDevice    = "addDevice"
	UpdateActionRemoveDevice = "removeDevice"
)

const (
	CubeExtNumaKey = "cube-ext-numa"

	CubeExtQueueKey = "cube-ext-queue"
)

const (
	CubeMasterServiceID     string = "cubemaster-service"
	CubeMasterTemplateID    string = "cubemaster-template"
	CubeMasterInnerId       string = "cubemaster-inner"
	CubeMasterNetworkGet    string = "cubemaster-network-get"
	CubeMasterScheduleId    string = "cubemaster-schedule"
	CubeboxServiceID        string = "cubebox-service"
	ExtInfoCubeE2E          string = "cube-e2e"
	CubeMasterInnerRetryID  string = "cubemaster-inner-retry"
	CubeMasterInnerHandleID string = "cubemaster-inner-handle"
)

const (
	CubeMasterServiceIDLocalCache = "cubemaster-nodeinfo"
	Caller                        = "X-Caller"
	CallerHostIP                  = "X-Cube-Caller-Host-IP"
	RequestID                     = "X-RequestID"
	RetCode                       = "X-RetCode"
	MySQL                         = "MySQL"
	Redis                         = "Redis"
	CubeLet                       = "CubeLet"
	ActionLoadDBAll               = "loadall"
	ActionLoadDBByIDs             = "loadbyids"
	ActionLoadDBByIndex           = "loadbyindex"
	ActionBufferHandle            = "bufferhandle"
	ActionDBCreate                = "dbcreate"
	ActionDBUpdate                = "dbupdate"
	ActionDBGetById               = "dbgetbyid"
	ActionDBDelete                = "dbdelete"
	ActionDBGetByIndex            = "dbgetbyindex"
	ActionTemplateResolve         = "templateresolve"
	ActionTemplateGetDefinition   = "templategetdefinition"
	ActionTemplateLocality        = "templatelocality"
	ActionTemplateCacheHit        = "templatecachehit"
	ActionTemplateCacheMiss       = "templatecachemiss"
	ActionTemplateLocalityHit     = "templatelocalityhit"
	ActionTemplateLocalityMiss    = "templatelocalitymiss"
	ActionTemplateReplicaFallback = "templatereplicadbfallback"
)

const (
	HeartbeatHealth             = "LIVE"
	HostStatusRunning           = "RUNNING"
	MetadataTableName           = "t_cube_host_info"
	HostTypeTableName           = "t_cube_host_type"
	HostSubInfoTableName        = "t_cube_sub_host_info"
	InstanceInfoTableName       = "t_cube_instance_info"
	InstanceUserDataTableName   = "t_cube_instance_userdata"
	NodeMetaRegistrationTable   = "t_cube_node_registration"
	NodeMetaStatusTable         = "t_cube_node_status"
	TemplateDefinitionTableName = "t_cube_template_definition"
	TemplateReplicaTableName    = "t_cube_template_replica"
	RootfsArtifactTableName     = "t_cube_rootfs_artifact"
	TemplateImageJobTableName   = "t_cube_template_image_job"
)

const (
	WeightFactorReqCpu                = "req_cpu"
	WeightFactorReqMem                = "req_mem"
	WeightFactorMvmNum                = "mvm_num"
	WeightFactorQuotaCpu              = "quota_cpu_usage"
	WeightFactorQuotaMem              = "quota_mem_usage"
	WeightFactorCpuUtil               = "cpu_util"
	WeightFactorMemUsage              = "mem_usage"
	WeightFactorCpuLoadUsage          = "cpu_load"
	WeightFactorMetricUpdate          = "metric_update"
	WeightFactorLocalMetricUpdate     = "metric_local_update_at"
	WeightFactorCreateConcurrentLimit = "create_concurrent_limit"
	WeightFactorRealTimeCreateNum     = "realtime_create_num"
	WeightFactorLocalCreateNum        = "local_create_num"
	WeightFactorActiveWhiteList       = "active_whitelist"
	WeightFactorNegativeWhiteList     = "negative_whitelist"
	WeightFactorDataDiskUsage         = "data_disk_usage"
	WeightFactorStorageDiskUsage      = "storage_disk_usage"
	WeightFactorSysDiskUsage          = "sys_disk_usage"
	WeightFactorImageID               = "image_id"
	WeightFactorTemplateID            = "template_id"
)

const (
	AuthCubeVersion     = "cube_version"
	AuthUserID          = "cube_user_id"
	AuthTimestamp       = "cube_timestamp"
	AuthNonce           = "cube_nonce"
	AuthSignatureMethod = "cube_sgn_method"
	AuthSignature       = "cube_signature"
)

const (
	InstanceStatePending     = "PENDING"
	InstanceStateRunning     = "RUNNING"
	InstanceStateFailed      = "LAUNCH_FAILED"
	InstanceStateStopped     = "STOPPED"
	InstanceStateShutdown    = "SHUTDOWN"
	InstanceStateTerminating = "TERMINATING"
	InstanceStateTerminated  = "TERMINATED"
)

const (
	TaskStatusRunning = "RUNNING"
	TaskStatusSuccess = "SUCCESS"
	TaskStatusFailed  = "FAILED"
)

const (
	AffinityKeyCPUType             = "kubernetes.io/cpu-type"
	AffinityKeyZone                = "topology.kubernetes.io/zone"
	AffinityKeyClusterID           = "topology.kubernetes.io/cluster-id"
	AffinityKeyDisasterRecoverGroup = "topology.kubernetes.io/disaster-recover-group-id"
	AffinityKeyMemorySize          = "kubernetes.io/memory-size"
	AffinityKeyCPUCores            = "kubernetes.io/cpu-cores"
	AffinityKeyInstanceType        = "kubernetes.io/instance-type"
)

const (
	FilterVpcID = "vpc-id"

	FilterInstanceID = "instance-id"

	FilterPrivateIPAddress = "private-ip-address"

	FilterInstanceState = "instance-state"

	FilterZone = "zone"

	FilterCPUType = "cpu-type"
)

const (
	DefaultInstanceTypeName = "default"
)

const (
	RuntimeHandlerCube = "cube"
	RuntimeHandlerRunc = "runc"
)
