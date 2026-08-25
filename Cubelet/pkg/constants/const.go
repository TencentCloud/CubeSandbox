// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import (
	"os"
	"strings"
	"unicode"

	"github.com/containerd/plugin"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

const (
	InternalPlugin plugin.Type = "io.cubelet.internal.v1"

	CubeboxServicePlugin plugin.Type = "io.cubelet.cubebox-service.v1"

	ImagesServicePlugin plugin.Type = "io.cubelet.images-service.v1"

	WorkflowPlugin plugin.Type = "io.cubelet.workflow.v1"

	CubeServicePlugin plugin.Type = "io.cubelet.cube.v1"

	PluginCfsImage plugin.Type = "io.cubelet.cfs.v1"

	PluginChi plugin.Type = "io.cubelet.chi.v1"

	PluginCBRIManager   plugin.Type = "io.cubelet.cbrim.v1"
	PluginCBRI          plugin.Type = "io.cubelet.cbri.v1"
	CubeTransferManager plugin.Type = "io.cubelet.transfer.v1"
	CubeMetric          plugin.Type = "io.cubelet.metric.v1"

	CubeStorePlugin     plugin.Type = "io.cubelet.cubestore.v1"
	CubeMetaStorePlugin plugin.Type = "io.cubelet.cubemetastore.v1"

	CubeMountManagerPlugin plugin.Type = "io.cubelet.mount.v1"
)

type PluginId string

const (
	NetworkID PluginId = "network"

	StorageID PluginId = "storage"

	MetricID PluginId = "metric"

	CgroupID PluginId = "cgroup"

	ImagesID PluginId = "images"

	CubeboxID PluginId = "cubebox"

	CreateID PluginId = "createid"

	APPSnapshotID PluginId = "appsnapshot"

	ShimLogID      PluginId = "shimlog"
	GCID           PluginId = "cleanup"
	VolumeSourceID PluginId = "volume"
	BackupID       PluginId = "backup"

	WorkflowID PluginId = "workflow"

	ImagesServiceID PluginId = "images-service"

	CubeboxServiceID PluginId = "cubebox-service"

	GCServiceID PluginId = "gc-service"

	PluginIDMount PluginId = "mount"

	PluginSandboxControllerRuncPod           = "runc"
	PluginSandboxControllerCubeShim          = "cube-shim"
	PluginVSocketManger                      = "vsocket-manager"
	PluginManager                            = "manager"
	SNHostID                        PluginId = "snhost"
	PluginImage                     PluginId = "image"

	NumaID PluginId = "numa"

	NetFile     PluginId = "netfile"
	MultiMetaID PluginId = "mutilmeta"
)

const (
	ControllerConfigPlugin  plugin.Type = "io.cubelet.controller.config.v1"
	ControllerCubeletPlugin plugin.Type = "io.cubelet.controller.cubelet.v1"
	ControllerPlugin        plugin.Type = "io.cubelet.controller.v1"

	PluginCubelet                       string   = "cubelet"
	PluginRunTemplateManager            PluginId = "run-template-manager"
	PluginCubeRuntimeTemplateController PluginId = "cube-runtime-template-controller"
)

const (
	ExtensionsKey = "extensions"

	CubeInnerId = "cubebox-inner"

	CubeRunCgroupId = "cube-inner-cgroup"

	CubeProbeId = "probe"

	CubeNewContainerId = "new-metadata"

	CubeDelContainerId = "del-metadata"

	CubeShimBinaryStartId = "binarystart"

	CubeShimCreatetId = "create"

	CubeImageEnsureId = "image-ensure"

	CubeImageResolveId = "image-resolve"

	CubeImageMountCfsId = "image-cfs"

	CubeImageMountSqfsId = "image-sqfs"

	CubeImageUpdateSnapId = "image-updatesnap"

	CubeExecProcessId = "cube-exec-process"

	CubeFilesPrepareId = "files-prepare"

	CubeContainerSpecId = "gen-spec"

	CubeShimWaitId = "wait"

	CubeShimStartId = "start"

	CubeDeleteTaskId = "del-task"

	CubeShimUpdateId = "update"

	CubePrestopId = "prestop"

	CubePoststopId = "poststop"

	LimiterId = "workflow-limiter"

	DelSandbox    = "del-sandbox"
	DelContainer  = "del-container"
	ProcessExists = "process-exists-check"

	CubeExtNumaKey = "cube-ext-numa"

	CubeExtQueueKey = "cube-ext-queue"

	// CubeExtVolumeRefEvents carries a JSON array of plugin_volume node-level
	// reference-state changes ([{"volume_id","referenced"}]) reported to
	// CubeMaster on create/destroy responses. referenced is 1 when this node
	// started referencing the volume (0→1) and 0 when it stopped (1→0); repeat
	// references on the same node emit no entry.
	CubeExtVolumeRefEvents = "cube-volume-refcount-events"

	CubeShimPid = "shim-pid"
	CubeVmPid   = "vm-pid"
)

func (s PluginId) ID() string {
	return string(s)
}

const (
	MasterAnnotationsNetWork       = "cube.master.net"
	MasterAnnotationsBlkQos        = "cube.master.blk.qos"
	MasterAnnotationsFSQos         = "cube.master.fs.qos"
	MasterAnnotationsImageUserName = "cube.master.image.username"
	MasterAnnotationsImagetoken    = "cube.master.image.token"
	MasterAnnotationsNetCubeVips   = "cube.master.vips"
	MasterAnnotationsPICMode       = "cube.master.instance.pic_mode"
	MasterAnnotationsNumaNode      = "cube.master.instance.numa_node"

	MasterAnnotationsUserData                 = "cube.master.instance.user_data"
	MasterAnnotationsDataDisk                 = "cube.master.instance.data_disk"
	MasterAnnotationsVPC                      = "cube.master.instance.virtual_private_cloud"
	MasterAnnotationsSGIDS                    = "cube.master.instance.sg_ids"
	MasterAnnotationsRetainIP                 = "cube.master.instance.retain_ip"
	MasterAnnotationsInsRegion                = "cube.master.instance.region"
	MasterAnnotationsInsHostIP                = "cube.master.instance.host_ip"
	MasterAnnotationsInsHostUUID              = "cube.master.instance.host_uuid"
	MasterAnnotationsInsHostCpuTotal          = "cube.master.instance.host_cpu_total"
	MasterAnnotationsInsHostVirtualQuotaArray = "cube.master.instance.host_virtual_quota_array"
	MasterAnnotationsDisableVmCgroup          = "cube.master.disable_vm_cgroup"
	MasterAnnotationsDisableHostCgroup        = "cube.master.disable_host_cgroup"
	MasterAnnotationsUpdateAction             = "cube.master.update_action"
	MasterAnnotationsFallbackToSlowPath       = "cube.master.fallback_to_slow_path"
	MasterAnnotationsAppSnapshotCreate        = "cube.master.appsnapshot.create"
	MasterAnnotationAppSnapshotTemplateID     = "cube.master.appsnapshot.template.id"
	MasterAnnotationRuntimeSnapshotID         = "cube.master.runtime.snapshot.id"
	MasterAnnotationRuntimeSnapshotAttachedAt = "cube.master.runtime.snapshot.attached_at"
	// MasterAnnotationRuntimeRestoreSnapshotID tracks the snapshot id whose
	// memory image the running VM was last *restored* from (set by Create
	// for restore-from-snapshot path, and by Rollback). Unlike
	// MasterAnnotationRuntimeSnapshotID, Commit does NOT bump this — Commit
	// does not restart the VM, so the in-process pagemap_anon bitmap is
	// still tracking "anon pages dirtied since the last restore", which
	// matches the memory file recorded here.
	MasterAnnotationRuntimeRestoreSnapshotID         = "cube.master.runtime.restore.snapshot.id"
	MasterAnnotationRuntimeRestoreSnapshotAttachedAt = "cube.master.runtime.restore.snapshot.attached_at"
	// MasterAnnotationPauseSnapshotID is the Master-allocated pause snapshot
	// id (same snap-* format as normal Commit snapshots). Cubelet only stores
	// the local catalog under this id; Kind=pause_snapshot.
	MasterAnnotationPauseSnapshotID = "cube.master.pause.snapshot.id"
	// MasterAnnotationStorageBackend is the CoW backend Master passes on
	// Pause / Commit (xfs｜s3). Empty means xfs.
	MasterAnnotationStorageBackend = "cube.master.storage.backend"
	// MasterAnnotationSnapshotRemoteUUIDs is the JSON blob of remote
	// volume uuids (rootfs/memory/metadata) for cubecow_import_lvol.
	MasterAnnotationSnapshotRemoteUUIDs = "cube.master.snapshot.remote_uuids"
	// MasterAnnotationSnapshotCrossNode says placement landed this restore on
	// a node that holds no replica of the package, so it has to be imported
	// from S3 first. Value is the literal "true". Master is the only one that
	// can know this, so Cubelet takes it as fact and refuses a restore it
	// cannot satisfy instead of failing later on an unexplained catalog miss.
	MasterAnnotationSnapshotCrossNode = "cube.master.snapshot.cross_node"
	// MasterAnnotationDesiredSandboxID asks createid to use this sandbox ID
	// instead of generating a new one (Resume-from-pause / same-ID recreate).
	MasterAnnotationDesiredSandboxID = "cube.master.desired.sandbox.id"
	// AnnotationPauseKeepTombstone is set on in-process Destroy after
	// PauseToSnapshot (Cubelet Pause owns this; Master no longer issues a
	// separate Destroy RPC): Detach volumes / wipe leftover live runtime, but
	// keep the PAUSED CubeBox row for List/Info. PauseToSnapshot leaves the
	// shim alive; Destroy's paused path uses task Delete to reap it.
	AnnotationPauseKeepTombstone = "cube.pause.keep_tombstone"
	// AnnotationPauseDeleteTombstone is set when deleting a paused sandbox for
	// good: remove the PAUSED CubeBox store row (after pause-snap CleanupTemplate
	// on Master).
	AnnotationPauseDeleteTombstone = "cube.pause.delete_tombstone"

	MasterAnnotationAppSnapshotVersion               = "cube.master.appsnapshot.version"
	MasterAnnotationRootfsArtifactID                 = "cube.master.rootfs.artifact.id"
	MasterAnnotationRootfsArtifactJobID              = "cube.master.rootfs.artifact.job_id"
	MasterAnnotationRootfsArtifactURL                = "cube.master.rootfs.artifact.url"
	MasterAnnotationRootfsArtifactToken              = "cube.master.rootfs.artifact.token"
	MasterAnnotationRootfsArtifactSHA256             = "cube.master.rootfs.artifact.sha256"
	MasterAnnotationRootfsArtifactSizeBytes          = "cube.master.rootfs.artifact.size_bytes"
	MasterAnnotationWritableLayerSize                = "cube.master.rootfs.writable_layer_size"
	MasterAnnotationTemplateSpecFingerprint          = "cube.master.template.spec_fingerprint"
	MasterAnnotationComponentEnvdVersion             = "cube.master.components.envd.version"
	MasterAnnotationCreateTimeEnvVars                = "cube.master.internal.create_time_env_vars"
	MasterAnnotationInstanceType                     = "cube.master.instance.type"
	MasterAnnotationNetworkPolicyBlockAll            = "cube.master.network.policy.block_all"
	MasterAnnotationNetworkPolicyAllowPublicServices = "cube.master.network.policy.allow_public_services"
	MasterAnnotationNetworkPolicyDefault             = "cube.master.network.policy.default"
)

// Inventory version annotations used by Ensure
const (
	MasterAnnotationComponentCubeShimVersion   = "cube.master.components.cube-shim.version"
	MasterAnnotationComponentCubeKernelVersion = "cube.master.components.cube-kernel-scf.version"
	MasterAnnotationComponentCubeImageVersion  = "cube.master.components.cube-image.version"
	MasterAnnotationComponentCubeAgentVersion  = "cube.master.components.cube-agent.version"
)

const (
	AnnotationPmem          = "cube.pmem"
	AnnotationsVFIONet      = "cube.vfio.net"
	AnnotationsVFIODisk     = "cube.vfio.disk"
	AnnotationsVFIODiskRM   = "cube.vfio.disk.rm"
	AnnotationsNetWork      = "cube.net"
	AnnotationsSandboxDNS   = "cube.sandbox.dns"
	AnnotationsMountListKey = "cube.disk"
	AnnotationsFSKey        = "cube.fs"
	AnnotationsVMSpecKey    = "cube.vmmres"
	AnnotationsRootfsKey    = "cube.rootfs.info"

	AnnotationsNetCubeVips        = "cube.net.vips"
	AnnotationsCgroupPath         = "cube.sandbox_cgroup_path"
	AnnotationsRuntimeCfgPath     = "cube.runtime.config.path"
	AnnotationsVMOSImagePath      = "cube.vm.os-image.path"
	AnnotationsVMKernelPath       = "cube.vm.kernel.path"
	AnnotationsVMAgentPath        = "cube.vm.agent.path"
	AnnotationsRootfsWritableKey  = "cube.rootfs.wlayer.path"
	AnnotationsRootfsWlayerSubdir = "cube.rootfs.wlayer.subdir"
	AnnotationsCubeMsgKey         = "cube.msg.dev.path"
	AnnotationsProduct            = "cube.product"

	AnnotationVMSnapshotPath           = "cube.vm.snapshot.base.path"
	AnnotationVMSnapshotMemoryVolURL   = "cube.vm.snapshot.memory_vol_url"
	AnnotationPmemContainerPrefix      = "pmem-cntr"
	AnnotationPmemLangPrefix           = "pmem-lang"
	AnnotationPmemCubeBoxImageIDPrefix = "pmem-cubebox-image"
	AnnotationAppSnapshotCreate        = "cube.appsnapshot.create"
	AnnotationAppSnapshotRestore       = "cube.appsnapshot.restore"
	AnnotationAppSnapshotContainerID   = "cube.appsnapshot.container.id"
	AnnotationSnapshotDisable          = "cube.snapshot.disable"
	AnnotationAppSnapshotFinished      = "cube.appsnapshot.finished"

	DefaultSnapshotDir = "/usr/local/services/cubetoolbox/cube-snapshot"

	AnnotationContaineRootfsPropagation = "cube.container.rootfs.propagation"

	AnnotationVMKernelCmdlineAppend = "cube.vm.kernel.cmdline.append"
	AnnotationVirtiofs              = "cube.virtiofs"

	AnnotationPropagationMounts          = "cube.propagation.mounts"
	AnnotationPropagationContainerMounts = "cube.propagation.container.mounts"
	AnnotationPropagationExecMounts      = "cube.propagation.exec.mounts"

	AnnotationPropagationContainerUmounts = "cube.propagation.container.umounts"
)

const (
	LabelPinnedImageKey        = "cube.image.pinned"
	LabelPinnedImageValue      = "pinned"
	AnnotationCollectMemOnExit = "cube.instance.collect_memory"
	AnnotationUseNetfileCache  = "cube.instance.use_netfile_cache"
	AnnotationsDebugStdout     = "com.debug.stdout"

	AnnotationVideoEnable        = "cloud.tencent.com/video/enable"
	AnnotationVideoResolution    = "cloud.tencent.com/video/resolution"
	AnnotationVideoMaxResolution = "cloud.tencent.com/video/max-resolution"
	AnnotationVideoFPS           = "cloud.tencent.com/video/fps"

	LabelContainerImagePem     = "cube.image.pem"
	LabelContainerImageCosType = "cube.image.cos"
	LabelNumaNode              = "cube.numa_node"
	LabelHealthCheckPod        = "X-Origin-Caller"
)

const (
	MountTypeBind       = "bind"
	MountTypeTmpfs      = "tmpfs"
	MountTypeCgroup     = "cgroup"
	MountTypeBindShared = "bind-share"
	MountTypeLocal      = "local"

	MountPropagationRprivate = "rprivate"
	MountPropagationRSlave   = "rslave"
	MountPropagationRShared  = "rshared"
	MountPropagationSlave    = "slave"
	MountOptBindRO           = "rbind"
	MountOptBind             = "bind"
	MountPropagationShared   = "shared"
	MountOptReadOnly         = "ro"
	MountOptReadWrite        = "rw"
	MountOptNoSuid           = "nosuid"
	MountOptNoExec           = "noexec"
	MountOptNoDev            = "nodev"
)

const (
	UpdateActionAddDevice    = "addDevice"
	UpdateActionRemoveDevice = "removeDevice"
	UpdateActionPause        = "pause"
	UpdateActionResume       = "resume"
	UpdateActionNetwork      = "network"
	PreStopTypePause         = "pause"
	PreStopTypeDestroy       = "destroy"
)

type contextKey string

const (
	KCubeIndexContext contextKey = "kCubeIndex"
)
const (
	BoltOpenTimeout = "io.containerd.timeout.bolt.open"
)

func MakeContainerIDEnvKey(name string) string {
	var sb strings.Builder
	sb.WriteString("CUBE_CONTAINER_ID_")
	for _, c := range name {
		if c == '-' {
			sb.WriteByte('_')
		} else {
			sb.WriteRune(unicode.ToUpper(c))
		}
	}
	return sb.String()
}

func GetInstanceTypeWithDefault(s string) string {
	if s == "" {
		return cubebox.InstanceType_cubebox.String()
	}
	return s
}

const (
	PCIModePF = "PF"
	PCIModeVF = "VF"
)

const (
	CubeVersion          = "v1"
	CubeDefaultNamespace = "default"
)

var (
	DeviceDefaultFileMode        = os.FileMode(0o666)
	ALLUid                uint32 = 0xffffffff
)

const (
	DeviceTypeChar  = "c"
	DeviceTypeBlock = "b"

	DeviceNamePrefix = "/dev"
	DeviceNameVsock  = DeviceNamePrefix + "/vsock"
)

const (
	PrefixSha256 = "sha256:"
)

const (
	UserAgentKey          = "user-agent"
	UserAgentDefaultValue = ""
	UserAgentCubecli      = "cubecli"
)

const (
	StringTrueValue  = "true"
	StringFalseValue = "false"
)

const (
	PropagationVirtioRo       = "virtio_ro"
	PropagationVirtioRw       = "virtio_rw"
	PropagationContainerDirRo = "/.container_ro"
	PropagationContainerDirRw = "/.container_rw"
)

const (
	VirtiofsCacheAuto   = 0
	VirtiofsCacheAlways = 1
	VirtiofsCacheNever  = 2
	VirtiofsCacheNone   = 3
)
