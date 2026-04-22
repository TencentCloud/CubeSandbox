# Protocol Documentation
<a name="top"></a>

## Table of Contents

- [api/services/cubebox/v1/cubebox.proto](#api_services_cubebox_v1_cubebox-proto)
    - [AppSnapshotRequest](#cubelet-services-cubebox-v1-AppSnapshotRequest)
    - [AppSnapshotResponse](#cubelet-services-cubebox-v1-AppSnapshotResponse)
    - [CDIDevice](#cubelet-services-cubebox-v1-CDIDevice)
    - [Capability](#cubelet-services-cubebox-v1-Capability)
    - [CleanupTemplateRequest](#cubelet-services-cubebox-v1-CleanupTemplateRequest)
    - [CleanupTemplateResponse](#cubelet-services-cubebox-v1-CleanupTemplateResponse)
    - [CommitSandboxRequest](#cubelet-services-cubebox-v1-CommitSandboxRequest)
    - [CommitSandboxResponse](#cubelet-services-cubebox-v1-CommitSandboxResponse)
    - [Container](#cubelet-services-cubebox-v1-Container)
    - [Container.LabelsEntry](#cubelet-services-cubebox-v1-Container-LabelsEntry)
    - [ContainerConfig](#cubelet-services-cubebox-v1-ContainerConfig)
    - [ContainerConfig.AnnotationsEntry](#cubelet-services-cubebox-v1-ContainerConfig-AnnotationsEntry)
    - [ContainerConfig.SysctlsEntry](#cubelet-services-cubebox-v1-ContainerConfig-SysctlsEntry)
    - [ContainerSecurityContext](#cubelet-services-cubebox-v1-ContainerSecurityContext)
    - [ContainerStateValue](#cubelet-services-cubebox-v1-ContainerStateValue)
    - [CubeSandbox](#cubelet-services-cubebox-v1-CubeSandbox)
    - [CubeSandbox.LabelsEntry](#cubelet-services-cubebox-v1-CubeSandbox-LabelsEntry)
    - [CubeSandboxFilter](#cubelet-services-cubebox-v1-CubeSandboxFilter)
    - [CubeSandboxFilter.LabelSelectorEntry](#cubelet-services-cubebox-v1-CubeSandboxFilter-LabelSelectorEntry)
    - [CubeVSContext](#cubelet-services-cubebox-v1-CubeVSContext)
    - [DNSConfig](#cubelet-services-cubebox-v1-DNSConfig)
    - [DestroyCubeSandboxRequest](#cubelet-services-cubebox-v1-DestroyCubeSandboxRequest)
    - [DestroyCubeSandboxRequest.AnnotationsEntry](#cubelet-services-cubebox-v1-DestroyCubeSandboxRequest-AnnotationsEntry)
    - [DestroyCubeSandboxResponse](#cubelet-services-cubebox-v1-DestroyCubeSandboxResponse)
    - [DestroyCubeSandboxResponse.ExtInfoEntry](#cubelet-services-cubebox-v1-DestroyCubeSandboxResponse-ExtInfoEntry)
    - [Device](#cubelet-services-cubebox-v1-Device)
    - [EmptyDirVolumeSource](#cubelet-services-cubebox-v1-EmptyDirVolumeSource)
    - [ExecCubeSandboxRequest](#cubelet-services-cubebox-v1-ExecCubeSandboxRequest)
    - [ExecCubeSandboxResponse](#cubelet-services-cubebox-v1-ExecCubeSandboxResponse)
    - [HTTPGetAction](#cubelet-services-cubebox-v1-HTTPGetAction)
    - [HTTPHeader](#cubelet-services-cubebox-v1-HTTPHeader)
    - [Hook](#cubelet-services-cubebox-v1-Hook)
    - [Hooks](#cubelet-services-cubebox-v1-Hooks)
    - [HostAlias](#cubelet-services-cubebox-v1-HostAlias)
    - [HostDirSource](#cubelet-services-cubebox-v1-HostDirSource)
    - [HostDirVolumeSources](#cubelet-services-cubebox-v1-HostDirVolumeSources)
    - [IDMapping](#cubelet-services-cubebox-v1-IDMapping)
    - [Int64Value](#cubelet-services-cubebox-v1-Int64Value)
    - [KeyValue](#cubelet-services-cubebox-v1-KeyValue)
    - [LifecycleHandler](#cubelet-services-cubebox-v1-LifecycleHandler)
    - [LinuxSeccompArg](#cubelet-services-cubebox-v1-LinuxSeccompArg)
    - [ListCubeSandboxOption](#cubelet-services-cubebox-v1-ListCubeSandboxOption)
    - [ListCubeSandboxRequest](#cubelet-services-cubebox-v1-ListCubeSandboxRequest)
    - [ListCubeSandboxResponse](#cubelet-services-cubebox-v1-ListCubeSandboxResponse)
    - [OCIConfig](#cubelet-services-cubebox-v1-OCIConfig)
    - [PingAction](#cubelet-services-cubebox-v1-PingAction)
    - [PortMapping](#cubelet-services-cubebox-v1-PortMapping)
    - [PostStop](#cubelet-services-cubebox-v1-PostStop)
    - [PreStop](#cubelet-services-cubebox-v1-PreStop)
    - [Probe](#cubelet-services-cubebox-v1-Probe)
    - [ProbeHandler](#cubelet-services-cubebox-v1-ProbeHandler)
    - [RLimit](#cubelet-services-cubebox-v1-RLimit)
    - [Resource](#cubelet-services-cubebox-v1-Resource)
    - [RunCubeSandboxRequest](#cubelet-services-cubebox-v1-RunCubeSandboxRequest)
    - [RunCubeSandboxRequest.AnnotationsEntry](#cubelet-services-cubebox-v1-RunCubeSandboxRequest-AnnotationsEntry)
    - [RunCubeSandboxRequest.LabelsEntry](#cubelet-services-cubebox-v1-RunCubeSandboxRequest-LabelsEntry)
    - [RunCubeSandboxResponse](#cubelet-services-cubebox-v1-RunCubeSandboxResponse)
    - [RunCubeSandboxResponse.ExtInfoEntry](#cubelet-services-cubebox-v1-RunCubeSandboxResponse-ExtInfoEntry)
    - [SELinuxOption](#cubelet-services-cubebox-v1-SELinuxOption)
    - [SandboxPathVolumeSource](#cubelet-services-cubebox-v1-SandboxPathVolumeSource)
    - [SysCall](#cubelet-services-cubebox-v1-SysCall)
    - [TCPSocketAction](#cubelet-services-cubebox-v1-TCPSocketAction)
    - [UpdateCubeSandboxRequest](#cubelet-services-cubebox-v1-UpdateCubeSandboxRequest)
    - [UpdateCubeSandboxRequest.AnnotationsEntry](#cubelet-services-cubebox-v1-UpdateCubeSandboxRequest-AnnotationsEntry)
    - [UpdateCubeSandboxResponse](#cubelet-services-cubebox-v1-UpdateCubeSandboxResponse)
    - [Volume](#cubelet-services-cubebox-v1-Volume)
    - [VolumeMounts](#cubelet-services-cubebox-v1-VolumeMounts)
    - [VolumeSource](#cubelet-services-cubebox-v1-VolumeSource)
  
    - [ContainerState](#cubelet-services-cubebox-v1-ContainerState)
    - [InstanceType](#cubelet-services-cubebox-v1-InstanceType)
    - [MountPropagation](#cubelet-services-cubebox-v1-MountPropagation)
    - [NetworkType](#cubelet-services-cubebox-v1-NetworkType)
    - [SandboxPathType](#cubelet-services-cubebox-v1-SandboxPathType)
    - [StorageMedium](#cubelet-services-cubebox-v1-StorageMedium)
  
    - [CubeboxMgr](#cubelet-services-cubebox-v1-CubeboxMgr)
  
- [api/services/cubehost/v1/hostimage.proto](#api_services_cubehost_v1_hostimage-proto)
    - [ClientMessage](#cubelet-services-cubebox-hostimage-v1-ClientMessage)
    - [DNSConfig](#cubelet-services-cubebox-hostimage-v1-DNSConfig)
    - [ForwardImageRequest](#cubelet-services-cubebox-hostimage-v1-ForwardImageRequest)
    - [ForwardImageResponse](#cubelet-services-cubebox-hostimage-v1-ForwardImageResponse)
    - [HostImage](#cubelet-services-cubebox-hostimage-v1-HostImage)
    - [ImageDev](#cubelet-services-cubebox-hostimage-v1-ImageDev)
    - [ImageFsStats](#cubelet-services-cubebox-hostimage-v1-ImageFsStats)
    - [InflightRequest](#cubelet-services-cubebox-hostimage-v1-InflightRequest)
    - [LayerMount](#cubelet-services-cubebox-hostimage-v1-LayerMount)
    - [ListImagesRequest](#cubelet-services-cubebox-hostimage-v1-ListImagesRequest)
    - [ListImagesResponse](#cubelet-services-cubebox-hostimage-v1-ListImagesResponse)
    - [ListInflightRequest](#cubelet-services-cubebox-hostimage-v1-ListInflightRequest)
    - [MountImageRequest](#cubelet-services-cubebox-hostimage-v1-MountImageRequest)
    - [PodSandboxConfig](#cubelet-services-cubebox-hostimage-v1-PodSandboxConfig)
    - [PodSandboxConfig.AnnotationsEntry](#cubelet-services-cubebox-hostimage-v1-PodSandboxConfig-AnnotationsEntry)
    - [PodSandboxConfig.LabelsEntry](#cubelet-services-cubebox-hostimage-v1-PodSandboxConfig-LabelsEntry)
    - [PodSandboxMetadata](#cubelet-services-cubebox-hostimage-v1-PodSandboxMetadata)
    - [RemoveSnapshotRequest](#cubelet-services-cubebox-hostimage-v1-RemoveSnapshotRequest)
    - [ResponseStatus](#cubelet-services-cubebox-hostimage-v1-ResponseStatus)
    - [ServerMessage](#cubelet-services-cubebox-hostimage-v1-ServerMessage)
  
    - [LayerType](#cubelet-services-cubebox-hostimage-v1-LayerType)
    - [MessageType](#cubelet-services-cubebox-hostimage-v1-MessageType)
    - [ResponseStatusCode](#cubelet-services-cubebox-hostimage-v1-ResponseStatusCode)
  
    - [CubeHostImageService](#cubelet-services-cubebox-hostimage-v1-CubeHostImageService)
    - [CubeVMImageService](#cubelet-services-cubebox-hostimage-v1-CubeVMImageService)
  
- [api/services/errorcode/v1/errorcode.proto](#api_services_errorcode_v1_errorcode-proto)
    - [Ret](#cubelet-services-errorcode-v1-Ret)
  
    - [ErrorCode](#cubelet-services-errorcode-v1-ErrorCode)
  
- [api/services/images/v1/images.proto](#api_services_images_v1_images-proto)
    - [CreateImageRequest](#cubelet-services-images-v1-CreateImageRequest)
    - [CreateImageRequestResponse](#cubelet-services-images-v1-CreateImageRequestResponse)
    - [CubeListImageRequest](#cubelet-services-images-v1-CubeListImageRequest)
    - [CubeListImageResponse](#cubelet-services-images-v1-CubeListImageResponse)
    - [DestroyImageRequest](#cubelet-services-images-v1-DestroyImageRequest)
    - [DestroyImageResponse](#cubelet-services-images-v1-DestroyImageResponse)
    - [ImageSpec](#cubelet-services-images-v1-ImageSpec)
    - [ImageSpec.AnnotationsEntry](#cubelet-services-images-v1-ImageSpec-AnnotationsEntry)
    - [ImageVolumeSource](#cubelet-services-images-v1-ImageVolumeSource)
    - [VolumUtilsRequest](#cubelet-services-images-v1-VolumUtilsRequest)
    - [VolumUtilsResponse](#cubelet-services-images-v1-VolumUtilsResponse)
  
    - [ImageStorageMediaType](#cubelet-services-images-v1-ImageStorageMediaType)
    - [PullPolicy](#cubelet-services-images-v1-PullPolicy)
  
    - [Images](#cubelet-services-images-v1-Images)
  
- [api/services/multimetadb/v1/multimeta_db.proto](#api_services_multimetadb_v1_multimeta_db-proto)
    - [BucketDefine](#cubelet-services-multimetadb-v1-BucketDefine)
    - [CommonRequestHeader](#cubelet-services-multimetadb-v1-CommonRequestHeader)
    - [CommonResponseHeader](#cubelet-services-multimetadb-v1-CommonResponseHeader)
    - [DbData](#cubelet-services-multimetadb-v1-DbData)
    - [GetBucketDefinesResponse](#cubelet-services-multimetadb-v1-GetBucketDefinesResponse)
    - [GetDataRequest](#cubelet-services-multimetadb-v1-GetDataRequest)
  
    - [MultiMetaDBServer](#cubelet-services-multimetadb-v1-MultiMetaDBServer)
  
- [api/services/nbi/v1/cubelet_api.proto](#api_services_nbi_v1_cubelet_api-proto)
    - [InitRequest](#cubelet-services-cubebox-v1-InitRequest)
    - [InitResponse](#cubelet-services-cubebox-v1-InitResponse)
    - [InitResponse.ExtInfoEntry](#cubelet-services-cubebox-v1-InitResponse-ExtInfoEntry)
  
    - [CubeLet](#cubelet-services-cubebox-v1-CubeLet)
  
- [api/services/version/v1/version.proto](#api_services_version_v1_version-proto)
    - [VersionResponse](#cubelet-services-version-v1-VersionResponse)
  
    - [Version](#cubelet-services-version-v1-Version)
  
- [api/types/v1/descriptor.proto](#api_types_v1_descriptor-proto)
    - [Descriptor](#cubelet-types-Descriptor)
    - [Descriptor.AnnotationsEntry](#cubelet-types-Descriptor-AnnotationsEntry)
  
- [api/types/v1/images.proto](#api_types_v1_images-proto)
    - [AuthConfig](#cubelet-types-AuthConfig)
    - [Image](#cubelet-types-Image)
    - [ImageFilter](#cubelet-types-ImageFilter)
    - [ImageSpec](#cubelet-types-ImageSpec)
    - [ImageSpec.AnnotationsEntry](#cubelet-types-ImageSpec-AnnotationsEntry)
    - [Int64Value](#cubelet-types-Int64Value)
  
- [Scalar Value Types](#scalar-value-types)



<a name="api_services_cubebox_v1_cubebox-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/cubebox/v1/cubebox.proto



<a name="cubelet-services-cubebox-v1-AppSnapshotRequest"></a>

### AppSnapshotRequest
AppSnapshotRequest is the request for creating an app snapshot.
This request creates a cubebox, makes an app snapshot using cube-runtime,
and then destroys the cubebox.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| create_request | [RunCubeSandboxRequest](#cubelet-services-cubebox-v1-RunCubeSandboxRequest) |  | The RunCubeSandboxRequest to create the cubebox. Required annotations: - cube.master.appsnapshot.create: &#34;true&#34; - cube.master.appsnapshot.template.id: &#34;&lt;template_id&gt;&#34; |
| snapshot_dir | [string](#string) |  | Custom snapshot directory path. If empty, uses default path. |






<a name="cubelet-services-cubebox-v1-AppSnapshotResponse"></a>

### AppSnapshotResponse
AppSnapshotResponse is the response for app snapshot creation.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| sandboxID | [string](#string) |  | The sandbox ID that was created and destroyed. |
| templateID | [string](#string) |  | The template ID from the request annotations. |
| snapshotPath | [string](#string) |  | The snapshot path where the snapshot was created. |






<a name="cubelet-services-cubebox-v1-CDIDevice"></a>

### CDIDevice
CDIDevice specifies a CDI device information.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | Fully qualified CDI device name for example: vendor.com/gpu=gpudevice1 see more details in the CDI specification: https://github.com/container-orchestrated-devices/container-device-interface/blob/main/SPEC.md |






<a name="cubelet-services-cubebox-v1-Capability"></a>

### Capability
Capability contains the container capabilities to add or drop


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| add_capabilities | [string](#string) | repeated | List of capabilities to add. |
| drop_capabilities | [string](#string) | repeated | List of capabilities to drop. |
| add_ambient_capabilities | [string](#string) | repeated | List of ambient capabilities to add. |






<a name="cubelet-services-cubebox-v1-CleanupTemplateRequest"></a>

### CleanupTemplateRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| templateID | [string](#string) |  | Logical template ID. |
| snapshotPath | [string](#string) |  | Snapshot path recorded on this node, if any. |






<a name="cubelet-services-cubebox-v1-CleanupTemplateResponse"></a>

### CleanupTemplateResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| templateID | [string](#string) |  | Logical template ID. |






<a name="cubelet-services-cubebox-v1-CommitSandboxRequest"></a>

### CommitSandboxRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| sandboxID | [string](#string) |  | ID of the running sandbox to snapshot. |
| templateID | [string](#string) |  | Logical template ID. |
| snapshot_dir | [string](#string) |  | Custom snapshot directory path. If empty, uses default path. |






<a name="cubelet-services-cubebox-v1-CommitSandboxResponse"></a>

### CommitSandboxResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| sandboxID | [string](#string) |  | ID of the sandbox that was snapshotted. |
| templateID | [string](#string) |  | The logical template ID. |
| snapshotPath | [string](#string) |  | The snapshot path where the snapshot was created. |






<a name="cubelet-services-cubebox-v1-Container"></a>

### Container



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | ID of the container, used by the container runtime to identify a container. |
| image | [string](#string) |  | Spec of the image. |
| type | [string](#string) |  | Type is container type. Sandbox or Container. |
| resources | [Resource](#cubelet-services-cubebox-v1-Resource) |  | Resources specification for the container. |
| state | [ContainerState](#cubelet-services-cubebox-v1-ContainerState) |  | State of the container. |
| created_at | [int64](#int64) |  | Creation time of the container in nanoseconds. |
| finished_at | [int64](#int64) |  | exit time of the container in nanoseconds. |
| labels | [Container.LabelsEntry](#cubelet-services-cubebox-v1-Container-LabelsEntry) | repeated |  |
| paused_at | [int64](#int64) |  |  |






<a name="cubelet-services-cubebox-v1-Container-LabelsEntry"></a>

### Container.LabelsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-ContainerConfig"></a>

### ContainerConfig



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | Name of the container |
| image | [cubelet.services.images.v1.ImageSpec](#cubelet-services-images-v1-ImageSpec) |  | Image to use. |
| command | [string](#string) | repeated | Command to execute (i.e., entrypoint for docker) |
| args | [string](#string) | repeated | Args for the Command (i.e., command for docker) |
| working_dir | [string](#string) |  | Current working directory of the command. |
| envs | [KeyValue](#cubelet-services-cubebox-v1-KeyValue) | repeated | List of environment variable to set in the container. |
| volume_mounts | [VolumeMounts](#cubelet-services-cubebox-v1-VolumeMounts) | repeated | Mounts for the container. |
| r_limit | [RLimit](#cubelet-services-cubebox-v1-RLimit) |  | r_limit for container |
| resources | [Resource](#cubelet-services-cubebox-v1-Resource) |  | Resources specification for the container. |
| security_context | [ContainerSecurityContext](#cubelet-services-cubebox-v1-ContainerSecurityContext) |  | security_context configuration for the container. |
| probe | [Probe](#cubelet-services-cubebox-v1-Probe) |  | probe |
| sysctls | [ContainerConfig.SysctlsEntry](#cubelet-services-cubebox-v1-ContainerConfig-SysctlsEntry) | repeated | sysctls holds linux sysctls config for the container. |
| syscalls | [SysCall](#cubelet-services-cubebox-v1-SysCall) | repeated | syscalls holds linux syscall config for the container. |
| dns_config | [DNSConfig](#cubelet-services-cubebox-v1-DNSConfig) |  | DNS config for the container. |
| annotations | [ContainerConfig.AnnotationsEntry](#cubelet-services-cubebox-v1-ContainerConfig-AnnotationsEntry) | repeated | annotations |
| hostAliases | [HostAlias](#cubelet-services-cubebox-v1-HostAlias) | repeated | HostAliases is an optional list of hosts and IPs that will be injected into the sandbox&#39;s hosts file if specified. |
| prestop | [PreStop](#cubelet-services-cubebox-v1-PreStop) |  | PreStop |
| poststop | [PostStop](#cubelet-services-cubebox-v1-PostStop) |  | PostStop |
| id | [string](#string) |  | container name use by cubebox |
| hooks | [Hooks](#cubelet-services-cubebox-v1-Hooks) |  | Hooks for the container. |
| oci_config | [OCIConfig](#cubelet-services-cubebox-v1-OCIConfig) |  | CDI |






<a name="cubelet-services-cubebox-v1-ContainerConfig-AnnotationsEntry"></a>

### ContainerConfig.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-ContainerConfig-SysctlsEntry"></a>

### ContainerConfig.SysctlsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-ContainerSecurityContext"></a>

### ContainerSecurityContext
ContainerSecurityContext holds linux security configuration that will be applied to a container.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| capabilities | [Capability](#cubelet-services-cubebox-v1-Capability) |  | Capabilities to add or drop. |
| privileged | [bool](#bool) |  | If set, run container in privileged mode. Privileged mode is incompatible with the following options. If privileged is set, the following features MAY have no effect: 1. capabilities 2. selinux_options 4. seccomp 5. apparmor

Privileged mode implies the following specific options are applied: 1. All capabilities are added. 2. Sensitive paths, such as kernel module paths within sysfs, are not masked. 3. Any sysfs and procfs mounts are mounted RW. 4. AppArmor confinement is not applied. 5. Seccomp restrictions are not applied. 6. The device cgroup does not restrict access to any devices. 7. All devices from the host&#39;s /dev are available within the container. 8. SELinux restrictions are not applied (e.g. label=disabled). |
| run_as_user | [Int64Value](#cubelet-services-cubebox-v1-Int64Value) |  | UID to run the container process as. Only one of run_as_user and run_as_username can be specified at a time. |
| no_new_privs | [bool](#bool) |  | no_new_privs defines if the flag for no_new_privs should be set on the container. |
| run_as_group | [Int64Value](#cubelet-services-cubebox-v1-Int64Value) |  | GID to run the container process as. run_as_group should only be specified when run_as_user or run_as_username is specified; otherwise, the runtime MUST error. |
| run_as_username | [string](#string) |  | User name to run the container process as. If specified, the user MUST exist in the container image (i.e. in the /etc/passwd inside the image), and be resolved there by the runtime; otherwise, the runtime MUST error. |
| readonly_rootfs | [bool](#bool) |  | If set, the root filesystem of the container is read-only. when not set,RunCubeSandboxRequest.volumes should provide one empty_dir source for the rootfs rw and Container.VolumeMounts provide container_path=&#34;/&#34; format |






<a name="cubelet-services-cubebox-v1-ContainerStateValue"></a>

### ContainerStateValue
ContainerStateValue is the wrapper of ContainerState.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| state | [ContainerState](#cubelet-services-cubebox-v1-ContainerState) |  | State of the container. |






<a name="cubelet-services-cubebox-v1-CubeSandbox"></a>

### CubeSandbox



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | ID of the CubeSandbox. |
| namespace | [string](#string) |  | containerd namespace. |
| created_at | [int64](#int64) |  | Creation time of the sandbox. |
| containers | [Container](#cubelet-services-cubebox-v1-Container) | repeated | containers Sandbox.Spec.Containers |
| port_mappings | [PortMapping](#cubelet-services-cubebox-v1-PortMapping) | repeated | Local cubevs portmapping |
| labels | [CubeSandbox.LabelsEntry](#cubelet-services-cubebox-v1-CubeSandbox-LabelsEntry) | repeated |  |
| numa_node | [int32](#int32) |  |  |
| paused_at | [int64](#int64) |  |  |
| private_cubebox_storage_data | [bytes](#bytes) |  | cubebox_storage_data is the private data of the cubebox store. It is used to store the data of the cubebox store. this field only used with with_cubebox_store option for cubecli command. |






<a name="cubelet-services-cubebox-v1-CubeSandbox-LabelsEntry"></a>

### CubeSandbox.LabelsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-CubeSandboxFilter"></a>

### CubeSandboxFilter
PodSandboxFilter is used to filter a list of PodSandboxes.
All those fields are combined with &#39;AND&#39;


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| state | [ContainerStateValue](#cubelet-services-cubebox-v1-ContainerStateValue) |  | State of the sandbox. |
| label_selector | [CubeSandboxFilter.LabelSelectorEntry](#cubelet-services-cubebox-v1-CubeSandboxFilter-LabelSelectorEntry) | repeated | LabelSelector to select matches. Only api.MatchLabels is supported for now and the requirements are ANDed. MatchExpressions is not supported yet. |
| instance_type | [string](#string) |  | There are some specific instance types for different scenarios. Each type has its own special logic during its lifecycle. |






<a name="cubelet-services-cubebox-v1-CubeSandboxFilter-LabelSelectorEntry"></a>

### CubeSandboxFilter.LabelSelectorEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-CubeVSContext"></a>

### CubeVSContext



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| allow_internet_access | [bool](#bool) | optional |  |
| allow_out | [string](#string) | repeated |  |
| deny_out | [string](#string) | repeated |  |






<a name="cubelet-services-cubebox-v1-DNSConfig"></a>

### DNSConfig
DNSConfig specifies the DNS servers and search domains of a sandbox.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| servers | [string](#string) | repeated | List of DNS servers of the cluster. |
| searches | [string](#string) | repeated | List of DNS search domains of the cluster. |
| options | [string](#string) | repeated | List of DNS options. See https://linux.die.net/man/5/resolv.conf for all available options. |






<a name="cubelet-services-cubebox-v1-DestroyCubeSandboxRequest"></a>

### DestroyCubeSandboxRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| sandboxID | [string](#string) |  | ID of the Sandbox. |
| annotations | [DestroyCubeSandboxRequest.AnnotationsEntry](#cubelet-services-cubebox-v1-DestroyCubeSandboxRequest-AnnotationsEntry) | repeated | Annotations can also be useful for runtime authors to experiment with new features that are opaque to the Kubernetes APIs (both user-facing and the CRI). Whenever possible, however, runtime authors SHOULD consider proposing new typed fields for any new features instead. |
| filter | [CubeSandboxFilter](#cubelet-services-cubebox-v1-CubeSandboxFilter) |  | filter provide a strong assumption for destroy if not match, do nothing |






<a name="cubelet-services-cubebox-v1-DestroyCubeSandboxRequest-AnnotationsEntry"></a>

### DestroyCubeSandboxRequest.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-DestroyCubeSandboxResponse"></a>

### DestroyCubeSandboxResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| sandboxID | [string](#string) |  | ID of the Sandbox. |
| ext_info | [DestroyCubeSandboxResponse.ExtInfoEntry](#cubelet-services-cubebox-v1-DestroyCubeSandboxResponse-ExtInfoEntry) | repeated |  |






<a name="cubelet-services-cubebox-v1-DestroyCubeSandboxResponse-ExtInfoEntry"></a>

### DestroyCubeSandboxResponse.ExtInfoEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [bytes](#bytes) |  |  |






<a name="cubelet-services-cubebox-v1-Device"></a>

### Device
Device specifies a host device to mount into a container.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| container_path | [string](#string) |  | Path of the device within the container. |
| host_path | [string](#string) |  | Path of the device on the host. |
| permissions | [string](#string) |  | Cgroups permissions of the device, candidates are one or more of * r - allows container to read from the specified device. * w - allows container to write to the specified device. * m - allows container to create device files that do not yet exist. |






<a name="cubelet-services-cubebox-v1-EmptyDirVolumeSource"></a>

### EmptyDirVolumeSource
EmptyDirVolumeSource represents an empty directory for a sandbox.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| Medium | [StorageMedium](#cubelet-services-cubebox-v1-StorageMedium) |  | media more like a scheduling problem - user says what traits they need, we give them a backing store that satisfies that. For now this will cover the most common needs. Optional: what type of storage medium should back this directory. The default is &#34;&#34; which means to use the node&#39;s default medium. &#43;optional |
| SizeLimit | [string](#string) |  | Total amount of local storage required for this EmptyDir volume. The size limit is also applicable for memory medium. The maximum usage on memory medium EmptyDir would be the minimum value between the SizeLimit specified here and the sum of memory limits of all containers in a Sandbox. The default is nil which means that the limit is undefined. More info: http://kubernetes.io/docs/user-guide/volumes#emptydir default 512Mi |






<a name="cubelet-services-cubebox-v1-ExecCubeSandboxRequest"></a>

### ExecCubeSandboxRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| sandbox_id | [string](#string) |  | 容器所在的sandbox的id |
| container_id | [string](#string) |  | 要执行的容器ID。 |
| terminal | [bool](#bool) |  | 是否为交互式终端模式。 |
| args | [string](#string) | repeated | Args specifies the binary and arguments for the application to execute. |
| env | [string](#string) | repeated | Env populates the process environment for the process. |
| cwd | [string](#string) |  | Cwd is the current working directory for the process and must be relative to the container&#39;s root. |






<a name="cubelet-services-cubebox-v1-ExecCubeSandboxResponse"></a>

### ExecCubeSandboxResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |






<a name="cubelet-services-cubebox-v1-HTTPGetAction"></a>

### HTTPGetAction
HTTPGetAction describes an action based on HTTP Get requests.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) | optional | Path to access on the HTTP server. |
| port | [int32](#int32) |  | The port to access on the container. Number must be in the range 1 to 65535. |
| host | [string](#string) | optional | Host name to connect to, defaults to the sandbox IP. You probably want to set &#34;Host&#34; in httpHeaders instead. |
| http_headers | [HTTPHeader](#cubelet-services-cubebox-v1-HTTPHeader) | repeated | Custom headers to set in the request. HTTP allows repeated headers. &#43;optional |






<a name="cubelet-services-cubebox-v1-HTTPHeader"></a>

### HTTPHeader
HTTPHeader describes a custom header to be used in HTTP probes


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) | optional | The header field name. This will be canonicalized upon output, so case-variant names will be understood as the same header. |
| value | [string](#string) | optional | The header field value |






<a name="cubelet-services-cubebox-v1-Hook"></a>

### Hook



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  |  |
| args | [string](#string) | repeated |  |
| env | [string](#string) | repeated |  |
| timeout | [int32](#int32) | optional |  |






<a name="cubelet-services-cubebox-v1-Hooks"></a>

### Hooks



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| Prestart | [Hook](#cubelet-services-cubebox-v1-Hook) | repeated |  |






<a name="cubelet-services-cubebox-v1-HostAlias"></a>

### HostAlias
HostAlias holds the mapping between IP and hostnames that will be injected as an entry in the
sandbox&#39;s hosts file.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| hostnames | [string](#string) | repeated | Hostnames for the above IP address. |
| ip | [string](#string) |  | IP address of the host file entry. |






<a name="cubelet-services-cubebox-v1-HostDirSource"></a>

### HostDirSource
HostDirSource defines a single host directory to be shared into the VM/container
via a virtiofs propagation channel.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | Unique name for this source, referenced by VolumeMount.name. |
| host_path | [string](#string) |  | Absolute path of the directory on the host to share. |






<a name="cubelet-services-cubebox-v1-HostDirVolumeSources"></a>

### HostDirVolumeSources
HostDirVolumeSources contains a list of host directory volumes to share into
containers via virtiofs propagation channels (virtio_ro / virtio_rw).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| volume_sources | [HostDirSource](#cubelet-services-cubebox-v1-HostDirSource) | repeated | List of host directory sources. |






<a name="cubelet-services-cubebox-v1-IDMapping"></a>

### IDMapping
IDMapping describes host to container ID mappings for a pod sandbox.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| host_id | [uint32](#uint32) |  | HostId is the id on the host. |
| container_id | [uint32](#uint32) |  | ContainerId is the id in the container. |
| length | [uint32](#uint32) |  | Length is the size of the range to map. |






<a name="cubelet-services-cubebox-v1-Int64Value"></a>

### Int64Value
Int64Value is the wrapper of int64.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| value | [int64](#int64) |  | The value. |






<a name="cubelet-services-cubebox-v1-KeyValue"></a>

### KeyValue



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-LifecycleHandler"></a>

### LifecycleHandler
LifecycleHandler defines a specific action that should be taken in a lifecycle hook. One and only one of the fields, except TCPSocket must be specified


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| http_get | [HTTPGetAction](#cubelet-services-cubebox-v1-HTTPGetAction) |  |  |






<a name="cubelet-services-cubebox-v1-LinuxSeccompArg"></a>

### LinuxSeccompArg



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| index | [uint32](#uint32) |  |  |
| value | [uint64](#uint64) |  |  |
| valueTwo | [uint64](#uint64) |  |  |
| op | [string](#string) |  | op ref:LinuxSeccompOperator |






<a name="cubelet-services-cubebox-v1-ListCubeSandboxOption"></a>

### ListCubeSandboxOption



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| private_with_cubebox_store | [bool](#bool) |  | use cubebox store data warning: this private field only used by cubecli |






<a name="cubelet-services-cubebox-v1-ListCubeSandboxRequest"></a>

### ListCubeSandboxRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) | optional | ID of the sandbox. |
| filter | [CubeSandboxFilter](#cubelet-services-cubebox-v1-CubeSandboxFilter) | optional | CubeSandboxFilter to filter a list of CubeSandboxes. |
| option | [ListCubeSandboxOption](#cubelet-services-cubebox-v1-ListCubeSandboxOption) | optional |  |






<a name="cubelet-services-cubebox-v1-ListCubeSandboxResponse"></a>

### ListCubeSandboxResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| items | [CubeSandbox](#cubelet-services-cubebox-v1-CubeSandbox) | repeated | List of CubeSandboxes. |






<a name="cubelet-services-cubebox-v1-OCIConfig"></a>

### OCIConfig



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| devices | [Device](#cubelet-services-cubebox-v1-Device) | repeated | Devices for the container. |
| cdi_devices | [CDIDevice](#cubelet-services-cubebox-v1-CDIDevice) | repeated | CDI devices for the container. |






<a name="cubelet-services-cubebox-v1-PingAction"></a>

### PingAction
TCPSocketAction describes an action based on ping.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| udp | [bool](#bool) |  |  |






<a name="cubelet-services-cubebox-v1-PortMapping"></a>

### PortMapping



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| container_port | [int32](#int32) |  | Port exposed in the container. |
| host_port | [int32](#int32) |  | Port exposed on the host. |






<a name="cubelet-services-cubebox-v1-PostStop"></a>

### PostStop



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| timeout_ms | [int32](#int32) |  | Length of request timeout. In Millisecond. |
| lifecyle_handler | [LifecycleHandler](#cubelet-services-cubebox-v1-LifecycleHandler) |  | How often (in Millisecond) to perform the probe. The action taken to execute the afterstop hook The action taken to determine the health of a container |






<a name="cubelet-services-cubebox-v1-PreStop"></a>

### PreStop
PreStop This hook is called immediately before a container is terminated due to an API request or management event such as a liveness/startup probe failure, preemption,
resource contention and others.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| termination_grace_period_ms | [int32](#int32) |  | Length of time before container is killed. In Millisecond. |
| lifecyle_handler | [LifecycleHandler](#cubelet-services-cubebox-v1-LifecycleHandler) |  | The action taken to execute the prestop hook The action taken to determine the health of a container |






<a name="cubelet-services-cubebox-v1-Probe"></a>

### Probe
Probe describes a health check to be performed against a container to determine whether it is
alive or ready to receive traffic.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| probe_handler | [ProbeHandler](#cubelet-services-cubebox-v1-ProbeHandler) |  | The action taken to determine the health of a container |
| initial_delay_ms | [int32](#int32) |  | Length of time before health checking is activated. In Millisecond. &#43;optional |
| timeout_ms | [int32](#int32) |  | Length of time before health checking times out. In Millisecond. &#43;optional |
| period_ms | [int32](#int32) |  | How often (in Millisecond) to perform the probe. &#43;optional |
| success_threshold | [int32](#int32) |  | Minimum consecutive successes for the probe to be considered successful after having failed. Must be 1 for liveness and startup. |
| failure_threshold | [int32](#int32) |  | Minimum consecutive failures for the probe to be considered failed after having succeeded. &#43;optional |
| probe_timeout_ms | [int32](#int32) |  | 单次探测超时时间(毫秒），默认100 &#43;optional |






<a name="cubelet-services-cubebox-v1-ProbeHandler"></a>

### ProbeHandler
ProbeHandler defines a specific action that should be taken in a probe.
One and only one of the fields must be specified.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tcp_socket | [TCPSocketAction](#cubelet-services-cubebox-v1-TCPSocketAction) |  | TCPSocket specifies an action involving a TCP port. &#43;optional |
| ping | [PingAction](#cubelet-services-cubebox-v1-PingAction) |  | Ping specifies the ping request to perform. &#43;optional |
| http_get | [HTTPGetAction](#cubelet-services-cubebox-v1-HTTPGetAction) |  | HTTPGet specifies the http request to perform. &#43;optional |






<a name="cubelet-services-cubebox-v1-RLimit"></a>

### RLimit
Rlimits specifies rlimit options to apply to the process.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| no_file | [uint64](#uint64) |  | the RLIMIT_NOFILE value to set |






<a name="cubelet-services-cubebox-v1-Resource"></a>

### Resource
Resource
ref by https://kubernetes.io/zh-cn/docs/reference/kubernetes-api/common-definitions/quantity/#Quantity


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| cpu | [string](#string) |  | Cpu的限制，单位为core数 |
| mem | [string](#string) |  | 内存限制，单位可以为字节 |
| cpu_limit | [string](#string) |  | mvm内对容器的cpu限制，单位为core数 |
| mem_limit | [string](#string) |  | mvm内对容器的内存限制，单位可以为字节 |






<a name="cubelet-services-cubebox-v1-RunCubeSandboxRequest"></a>

### RunCubeSandboxRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | request_ID reqID |
| volumes | [Volume](#cubelet-services-cubebox-v1-Volume) | repeated | volumes |
| containers | [ContainerConfig](#cubelet-services-cubebox-v1-ContainerConfig) | repeated | containers Sandbox.Spec.Containers |
| annotations | [RunCubeSandboxRequest.AnnotationsEntry](#cubelet-services-cubebox-v1-RunCubeSandboxRequest-AnnotationsEntry) | repeated | Annotations can also be useful for runtime authors to experiment with new features that are opaque to the Kubernetes APIs (both user-facing and the CRI). Whenever possible, however, runtime authors SHOULD consider proposing new typed fields for any new features instead. |
| labels | [RunCubeSandboxRequest.LabelsEntry](#cubelet-services-cubebox-v1-RunCubeSandboxRequest-LabelsEntry) | repeated | Key-value pairs that may be used to scope and select individual resources. |
| exposed_ports | [int64](#int64) | repeated | expose ports for external access |
| runtime_handler | [string](#string) |  | Named runtime configuration to use for this PodSandbox. If the runtime handler is unknown, this request should be rejected. An empty string should select the default handler, equivalent to the behavior before this feature was added. See https://git.k8s.io/enhancements/keps/sig-node/585-runtime-class |
| instance_type | [string](#string) |  | There are some specific instance types for different scenarios. Each type has its own special logic during its lifecycle. |
| network_type | [string](#string) |  | NetworkType default to tap |
| namespace | [string](#string) |  | user define namespace |
| cubevs_context | [CubeVSContext](#cubelet-services-cubebox-v1-CubeVSContext) | optional | CubeVS egress policy for the sandbox template/runtime. |






<a name="cubelet-services-cubebox-v1-RunCubeSandboxRequest-AnnotationsEntry"></a>

### RunCubeSandboxRequest.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-RunCubeSandboxRequest-LabelsEntry"></a>

### RunCubeSandboxRequest.LabelsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-RunCubeSandboxResponse"></a>

### RunCubeSandboxResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| sandboxID | [string](#string) |  | ID of the Sandbox. |
| sandboxUUID | [string](#string) |  | UUID of the Sandbox(mvm uuid). |
| sandboxIP | [string](#string) |  | ip of the Sandbox(mvm ip). |
| ext_info | [RunCubeSandboxResponse.ExtInfoEntry](#cubelet-services-cubebox-v1-RunCubeSandboxResponse-ExtInfoEntry) | repeated |  |
| port_mappings | [PortMapping](#cubelet-services-cubebox-v1-PortMapping) | repeated |  |






<a name="cubelet-services-cubebox-v1-RunCubeSandboxResponse-ExtInfoEntry"></a>

### RunCubeSandboxResponse.ExtInfoEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [bytes](#bytes) |  |  |






<a name="cubelet-services-cubebox-v1-SELinuxOption"></a>

### SELinuxOption
SELinuxOption are the labels to be applied to the container.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| user | [string](#string) |  |  |
| role | [string](#string) |  |  |
| type | [string](#string) |  |  |
| level | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-SandboxPathVolumeSource"></a>

### SandboxPathVolumeSource
Represents a sandbox path mapped into a container.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | path of the directory on the sandbox vm. |
| type | [string](#string) |  | type for SandboxPath Volume |






<a name="cubelet-services-cubebox-v1-SysCall"></a>

### SysCall



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| names | [string](#string) | repeated |  |
| action | [string](#string) |  | action ref:LinuxSeccompAction |
| errno | [uint32](#uint32) |  |  |
| args | [LinuxSeccompArg](#cubelet-services-cubebox-v1-LinuxSeccompArg) | repeated | args ref:LinuxSeccompArg |






<a name="cubelet-services-cubebox-v1-TCPSocketAction"></a>

### TCPSocketAction
TCPSocketAction describes an action based on opening a socket.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| port | [int32](#int32) |  |  |
| host | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-UpdateCubeSandboxRequest"></a>

### UpdateCubeSandboxRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| sandboxID | [string](#string) |  | ID of the Sandbox. |
| annotations | [UpdateCubeSandboxRequest.AnnotationsEntry](#cubelet-services-cubebox-v1-UpdateCubeSandboxRequest-AnnotationsEntry) | repeated | Annotations can also be useful for runtime authors to experiment with new features that are opaque to the Kubernetes APIs (both user-facing and the CRI). Whenever possible, however, runtime authors SHOULD consider proposing new typed fields for any new features instead. |






<a name="cubelet-services-cubebox-v1-UpdateCubeSandboxRequest-AnnotationsEntry"></a>

### UpdateCubeSandboxRequest.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-UpdateCubeSandboxResponse"></a>

### UpdateCubeSandboxResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |






<a name="cubelet-services-cubebox-v1-Volume"></a>

### Volume
Volume represents a named volume in a pod that may be accessed by any container in the pod.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | Required: This must be a DNS_LABEL. Each volume in a Sandbox must have a unique name. 这里必须包含用户资源的唯一标识,比如：tenant&#43;funcid |
| volume_source | [VolumeSource](#cubelet-services-cubebox-v1-VolumeSource) |  | The VolumeSource represents the location and type of a volume to mount. |






<a name="cubelet-services-cubebox-v1-VolumeMounts"></a>

### VolumeMounts
VolumeMount describes a mounting of a Volume within a container.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | Required: This must be a DNS_LABEL. Each volume in a Sandbox must have a unique name. |
| container_path | [string](#string) |  | Path of the mount within the container. |
| readonly | [bool](#bool) |  | If set, the mount is read-only. |
| propagation | [MountPropagation](#cubelet-services-cubebox-v1-MountPropagation) |  | Requested propagation mode. |
| uid | [string](#string) |  | UID for tmpfs mount (only applies to tmpfs volumes) |
| gid | [string](#string) |  | GID for tmpfs mount (only applies to tmpfs volumes) |
| exec | [bool](#bool) |  | If set to true, allow execution on tmpfs mount (only applies to tmpfs volumes) |
| host_path | [string](#string) |  | Path of the mount on the host. If the hostPath doesn&#39;t exist, then runtimes should report error. If the hostpath is a symbolic link, runtimes should follow the symlink and mount the real destination to container. |
| uidMappings | [IDMapping](#cubelet-services-cubebox-v1-IDMapping) | repeated | Requested propagation mode. UidMappings specifies the runtime UID mappings for the mount. |
| gidMappings | [IDMapping](#cubelet-services-cubebox-v1-IDMapping) | repeated | GidMappings specifies the runtime GID mappings for the mount. |
| recursive_read_only | [bool](#bool) |  | If set to true, the mount is made recursive read-only. In this CRI API, recursive_read_only is a plain true/false boolean, although its equivalent in the Kubernetes core API is a quaternary that can be nil, &#34;Enabled&#34;, &#34;IfPossible&#34;, or &#34;Disabled&#34;. kubelet translates that quaternary value in the core API into a boolean in this CRI API. Remarks: - nil is just treated as false - when set to true, readonly must be explicitly set to true, and propagation must be PRIVATE (0). - (readonly == false &amp;&amp; recursive_read_only == false) does not make the mount read-only. |
| selinux_relabel | [bool](#bool) |  | If set, the mount is read-only. |
| sub_path | [string](#string) |  | Specific image sub path to be used from inside the image instead of its root, only necessary if the above image field is set. If the sub path is not empty and does not exist in the image, then runtimes should fail and return an error. Introduced in the Image Volume Source KEP beta graduation: https://kep.k8s.io/4639 |






<a name="cubelet-services-cubebox-v1-VolumeSource"></a>

### VolumeSource
VolumeSource represents the source location of a volume to mount.
Only one of its members may be specified.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| empty_dir | [EmptyDirVolumeSource](#cubelet-services-cubebox-v1-EmptyDirVolumeSource) |  |  |
| sandbox_path | [SandboxPathVolumeSource](#cubelet-services-cubebox-v1-SandboxPathVolumeSource) |  |  |
| host_dir_volumes | [HostDirVolumeSources](#cubelet-services-cubebox-v1-HostDirVolumeSources) |  | host_dir_volumes shares host directories into the container via virtiofs. |
| image | [cubelet.services.images.v1.ImageVolumeSource](#cubelet-services-images-v1-ImageVolumeSource) |  | image volume source for image volume mount |





 


<a name="cubelet-services-cubebox-v1-ContainerState"></a>

### ContainerState
ContainerState represents the lifecycle state of a container.

| Name | Number | Description |
| ---- | ------ | ----------- |
| CONTAINER_CREATED | 0 |  |
| CONTAINER_RUNNING | 1 |  |
| CONTAINER_EXITED | 2 |  |
| CONTAINER_UNKNOWN | 3 |  |
| CONTAINER_PAUSING | 4 |  |
| CONTAINER_PAUSED | 5 |  |



<a name="cubelet-services-cubebox-v1-InstanceType"></a>

### InstanceType


| Name | Number | Description |
| ---- | ------ | ----------- |
| cubebox | 0 |  |



<a name="cubelet-services-cubebox-v1-MountPropagation"></a>

### MountPropagation
MountPropagation determines how mounts are propagated from the host
to container and the other way around.
When not set, MountPropagationNone is used.

| Name | Number | Description |
| ---- | ------ | ----------- |
| PROPAGATION_PRIVATE | 0 | No mount propagation (&#34;private&#34; in Linux terminology). |
| PROPAGATION_HOST_TO_CONTAINER | 1 | Mounts get propagated from the host to the container (&#34;rslave&#34; in Linux). |
| PROPAGATION_BIDIRECTIONAL | 2 | Mounts get propagated from the host to the container and from the container to the host (&#34;rshared&#34; in Linux). |



<a name="cubelet-services-cubebox-v1-NetworkType"></a>

### NetworkType


| Name | Number | Description |
| ---- | ------ | ----------- |
| tap | 0 |  |



<a name="cubelet-services-cubebox-v1-SandboxPathType"></a>

### SandboxPathType


| Name | Number | Description |
| ---- | ------ | ----------- |
| Directory | 0 | A directory must exist at the given path |
| Cgroup | 1 | A cgroup mount must exist at the given path |
| SharedBindMount | 2 |  |



<a name="cubelet-services-cubebox-v1-StorageMedium"></a>

### StorageMedium


| Name | Number | Description |
| ---- | ------ | ----------- |
| StorageMediumDefault | 0 | use blk device the default is for the node |
| StorageMediumMemory | 1 | use memory (e.g. tmpfs on linux) |
| StorageMediumCubeMsg | 2 | use cubemsg |


 

 


<a name="cubelet-services-cubebox-v1-CubeboxMgr"></a>

### CubeboxMgr
Service for handling cubesandbox

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| Create | [RunCubeSandboxRequest](#cubelet-services-cubebox-v1-RunCubeSandboxRequest) | [RunCubeSandboxResponse](#cubelet-services-cubebox-v1-RunCubeSandboxResponse) |  |
| Destroy | [DestroyCubeSandboxRequest](#cubelet-services-cubebox-v1-DestroyCubeSandboxRequest) | [DestroyCubeSandboxResponse](#cubelet-services-cubebox-v1-DestroyCubeSandboxResponse) |  |
| List | [ListCubeSandboxRequest](#cubelet-services-cubebox-v1-ListCubeSandboxRequest) | [ListCubeSandboxResponse](#cubelet-services-cubebox-v1-ListCubeSandboxResponse) |  |
| Update | [UpdateCubeSandboxRequest](#cubelet-services-cubebox-v1-UpdateCubeSandboxRequest) | [UpdateCubeSandboxResponse](#cubelet-services-cubebox-v1-UpdateCubeSandboxResponse) |  |
| Exec | [ExecCubeSandboxRequest](#cubelet-services-cubebox-v1-ExecCubeSandboxRequest) | [ExecCubeSandboxResponse](#cubelet-services-cubebox-v1-ExecCubeSandboxResponse) |  |
| AppSnapshot | [AppSnapshotRequest](#cubelet-services-cubebox-v1-AppSnapshotRequest) | [AppSnapshotResponse](#cubelet-services-cubebox-v1-AppSnapshotResponse) | AppSnapshot creates a cubebox, makes an app snapshot, and destroys the cubebox. Required annotations: - cube.master.appsnapshot.create: &#34;true&#34; - cube.master.appsnapshot.template.id: &#34;&lt;template_id&gt;&#34; |
| CommitSandbox | [CommitSandboxRequest](#cubelet-services-cubebox-v1-CommitSandboxRequest) | [CommitSandboxResponse](#cubelet-services-cubebox-v1-CommitSandboxResponse) | CommitSandbox snapshots an existing running sandbox into a template snapshot. |
| CleanupTemplate | [CleanupTemplateRequest](#cubelet-services-cubebox-v1-CleanupTemplateRequest) | [CleanupTemplateResponse](#cubelet-services-cubebox-v1-CleanupTemplateResponse) | CleanupTemplate removes local template snapshot/base data from this node. |

 



<a name="api_services_cubehost_v1_hostimage-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/cubehost/v1/hostimage.proto



<a name="cubelet-services-cubebox-hostimage-v1-ClientMessage"></a>

### ClientMessage



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| status | [ResponseStatus](#cubelet-services-cubebox-hostimage-v1-ResponseStatus) |  |  |
| type | [MessageType](#cubelet-services-cubebox-hostimage-v1-MessageType) |  |  |
| hello | [string](#string) |  | 客户端连接通知 |
| forwardImageResponse | [ForwardImageResponse](#cubelet-services-cubebox-hostimage-v1-ForwardImageResponse) |  | 下载操作响应 |
| common | [string](#string) |  | 通用消息 |
| listInflightRequest | [ListInflightRequest](#cubelet-services-cubebox-hostimage-v1-ListInflightRequest) |  | 列出正在进行的请求 |
| imageFsStats | [ImageFsStats](#cubelet-services-cubebox-hostimage-v1-ImageFsStats) |  | 获取镜像文件系统信息 |






<a name="cubelet-services-cubebox-hostimage-v1-DNSConfig"></a>

### DNSConfig
DNSConfig specifies the DNS servers and search domains of a sandbox.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| servers | [string](#string) | repeated | List of DNS servers of the cluster. |
| searches | [string](#string) | repeated | List of DNS search domains of the cluster. |
| options | [string](#string) | repeated | List of DNS options. See https://linux.die.net/man/5/resolv.conf for all available options. |






<a name="cubelet-services-cubebox-hostimage-v1-ForwardImageRequest"></a>

### ForwardImageRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| image | [cubelet.types.ImageSpec](#cubelet-types-ImageSpec) |  | Spec of the image. |
| auth | [cubelet.types.AuthConfig](#cubelet-types-AuthConfig) |  | Authentication configuration for pulling the image. |
| sandbox_config | [PodSandboxConfig](#cubelet-services-cubebox-hostimage-v1-PodSandboxConfig) |  | Config of the PodSandbox, which is used to pull image in PodSandbox context. |






<a name="cubelet-services-cubebox-hostimage-v1-ForwardImageResponse"></a>

### ForwardImageResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| image | [HostImage](#cubelet-services-cubebox-hostimage-v1-HostImage) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-HostImage"></a>

### HostImage
Basic information about a container image.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | ID of the image. |
| repo_tags | [string](#string) | repeated | Other names by which this image is known. |
| repo_digests | [string](#string) | repeated | Digests by which this image is known. |
| size | [uint64](#uint64) |  | Size of the image in bytes. Must be &gt; 0. |
| spec | [cubelet.types.ImageSpec](#cubelet-types-ImageSpec) |  | ImageSpec for image which includes annotations |
| descriptors | [google.protobuf.Any](#google-protobuf-Any) | repeated | all descriptors for this image |
| LayerMounts | [LayerMount](#cubelet-services-cubebox-hostimage-v1-LayerMount) | repeated | LayerMounts is a list of mounts for the image layers. |
| image_devs | [ImageDev](#cubelet-services-cubebox-hostimage-v1-ImageDev) |  | image_devs is list of snapshots belong to erofs image device. A image can consist of the following components: 1. Composed solely of erofs layers 2. The erofs layer serves as the bottom layer, and the regular image layer serves as the upper layer 3. Ordinary image layer That is to say, the image cannot be composed of a regular image layer as the bottom layer and an erofs image layer as the upper layer. And a maximum of one erofs device can be used for one image. |






<a name="cubelet-services-cubebox-hostimage-v1-ImageDev"></a>

### ImageDev



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| serial_id | [string](#string) |  |  |
| host_device_path | [string](#string) |  |  |
| vm_device_path | [string](#string) |  | vm_device_path is the path of the device in the VM. this fields is only used in the VM. |
| vm_root_path | [string](#string) |  | this fields is only used in the VM. |
| fs_type | [string](#string) |  | fs_type is the filesystem type of the volume. |
| mount_options | [string](#string) | repeated |  |
| snapshots | [string](#string) | repeated | snapshots is a list of snapshots of the image. |
| image_id | [string](#string) |  | image id of all snapshot |
| image_references | [string](#string) | repeated |  |
| bdf | [string](#string) |  | pci bdf of virtio-blk |






<a name="cubelet-services-cubebox-hostimage-v1-ImageFsStats"></a>

### ImageFsStats



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| AvailableBytes | [uint64](#uint64) |  |  |
| CapacityBytes | [uint64](#uint64) |  |  |
| UsedBytes | [uint64](#uint64) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-InflightRequest"></a>

### InflightRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-LayerMount"></a>

### LayerMount
Mount specifies a host volume to mount into a container.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  |  |
| host_path | [string](#string) |  |  |
| vm_path | [string](#string) |  |  |
| digest | [string](#string) |  |  |
| parent | [string](#string) |  |  |
| usage | [string](#string) |  |  |
| dev_serial_id | [string](#string) |  | dev_serial_id is the serial id of the device. this fields is only used when the mount is a device.like erofs. When a layer belongs to a certain device, it is not allowed to use the `host_path` field again |
| layer_type | [LayerType](#cubelet-services-cubebox-hostimage-v1-LayerType) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-ListImagesRequest"></a>

### ListImagesRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| filter | [cubelet.types.ImageFilter](#cubelet-types-ImageFilter) |  | Filter to list images. |






<a name="cubelet-services-cubebox-hostimage-v1-ListImagesResponse"></a>

### ListImagesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| images | [cubelet.types.Image](#cubelet-types-Image) | repeated | List of images. |






<a name="cubelet-services-cubebox-hostimage-v1-ListInflightRequest"></a>

### ListInflightRequest
ListInflightRequest is the request message for listing inflight requests.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requests | [InflightRequest](#cubelet-services-cubebox-hostimage-v1-InflightRequest) | repeated |  |






<a name="cubelet-services-cubebox-hostimage-v1-MountImageRequest"></a>

### MountImageRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ref | [string](#string) |  |  |
| target_path | [string](#string) |  |  |
| error_msg | [string](#string) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-PodSandboxConfig"></a>

### PodSandboxConfig
PodSandboxConfig holds all the required and optional fields for creating a
sandbox.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| metadata | [PodSandboxMetadata](#cubelet-services-cubebox-hostimage-v1-PodSandboxMetadata) |  | Metadata of the sandbox. This information will uniquely identify the sandbox, and the runtime should leverage this to ensure correct operation. The runtime may also use this information to improve UX, such as by constructing a readable name. |
| hostname | [string](#string) |  | Hostname of the sandbox. Hostname could only be empty when the pod network namespace is NODE. |
| log_directory | [string](#string) |  | Path to the directory on the host in which container log files are stored. By default the log of a container going into the LogDirectory will be hooked up to STDOUT and STDERR. However, the LogDirectory may contain binary log files with structured logging data from the individual containers. For example, the files might be newline separated JSON structured logs, systemd-journald journal files, gRPC trace files, etc. E.g., PodSandboxConfig.LogDirectory = `/var/log/pods/&lt;NAMESPACE&gt;_&lt;NAME&gt;_&lt;UID&gt;/` ContainerConfig.LogPath = `containerName/Instance#.log` |
| dns_config | [DNSConfig](#cubelet-services-cubebox-hostimage-v1-DNSConfig) |  | DNS config for the sandbox. |
| labels | [PodSandboxConfig.LabelsEntry](#cubelet-services-cubebox-hostimage-v1-PodSandboxConfig-LabelsEntry) | repeated | Port mappings for the sandbox. repeated PortMapping port_mappings = 5; Key-value pairs that may be used to scope and select individual resources. |
| annotations | [PodSandboxConfig.AnnotationsEntry](#cubelet-services-cubebox-hostimage-v1-PodSandboxConfig-AnnotationsEntry) | repeated | Unstructured key-value map that may be set by the kubelet to store and retrieve arbitrary metadata. This will include any annotations set on a pod through the Kubernetes API.

Annotations MUST NOT be altered by the runtime; the annotations stored here MUST be returned in the PodSandboxStatus associated with the pod this PodSandboxConfig creates.

In general, in order to preserve a well-defined interface between the kubelet and the container runtime, annotations SHOULD NOT influence runtime behaviour.

Annotations can also be useful for runtime authors to experiment with new features that are opaque to the Kubernetes APIs (both user-facing and the CRI). Whenever possible, however, runtime authors SHOULD consider proposing new typed fields for any new features instead.

Optional configurations specific to Linux hosts. LinuxPodSandboxConfig linux = 8; Optional configurations specific to Windows hosts. WindowsPodSandboxConfig windows = 9; |






<a name="cubelet-services-cubebox-hostimage-v1-PodSandboxConfig-AnnotationsEntry"></a>

### PodSandboxConfig.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-PodSandboxConfig-LabelsEntry"></a>

### PodSandboxConfig.LabelsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-PodSandboxMetadata"></a>

### PodSandboxMetadata
PodSandboxMetadata holds all necessary information for building the sandbox name.
The container runtime is encouraged to expose the metadata associated with the
PodSandbox in its user interface for better user experience. For example,
the runtime can construct a unique PodSandboxName based on the metadata.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | Pod name of the sandbox. Same as the pod name in the Pod ObjectMeta. |
| uid | [string](#string) |  | Pod UID of the sandbox. Same as the pod UID in the Pod ObjectMeta. |
| namespace | [string](#string) |  | Pod namespace of the sandbox. Same as the pod namespace in the Pod ObjectMeta. |
| attempt | [uint32](#uint32) |  | Attempt number of creating the sandbox. Default: 0. |






<a name="cubelet-services-cubebox-hostimage-v1-RemoveSnapshotRequest"></a>

### RemoveSnapshotRequest
RemoveSnapshotRequest is the request message for removing a snapshot.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| LayerMounts | [LayerMount](#cubelet-services-cubebox-hostimage-v1-LayerMount) | repeated | LayerMounts is a list of mounts for the image layers. |






<a name="cubelet-services-cubebox-hostimage-v1-ResponseStatus"></a>

### ResponseStatus



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| code | [ResponseStatusCode](#cubelet-services-cubebox-hostimage-v1-ResponseStatusCode) |  |  |
| message | [string](#string) |  |  |






<a name="cubelet-services-cubebox-hostimage-v1-ServerMessage"></a>

### ServerMessage



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| status | [ResponseStatus](#cubelet-services-cubebox-hostimage-v1-ResponseStatus) |  |  |
| type | [MessageType](#cubelet-services-cubebox-hostimage-v1-MessageType) |  |  |
| hello | [string](#string) |  | 客户端连接通知 |
| forwardImageRequest | [ForwardImageRequest](#cubelet-services-cubebox-hostimage-v1-ForwardImageRequest) |  | 下载操作响应 |
| removeSnapshotRequest | [RemoveSnapshotRequest](#cubelet-services-cubebox-hostimage-v1-RemoveSnapshotRequest) |  | 删除快照请求 |
| listInflightRequest | [ListInflightRequest](#cubelet-services-cubebox-hostimage-v1-ListInflightRequest) |  | 列出正在进行的请求 |





 


<a name="cubelet-services-cubebox-hostimage-v1-LayerType"></a>

### LayerType


| Name | Number | Description |
| ---- | ------ | ----------- |
| LAYER_TYPE_FS | 0 |  |
| LAYER_TYPE_DEVICE | 1 |  |



<a name="cubelet-services-cubebox-hostimage-v1-MessageType"></a>

### MessageType


| Name | Number | Description |
| ---- | ------ | ----------- |
| CLIENT_HELLO | 0 |  |
| IMAGE_FORWARD | 1 |  |
| REMOVE_SNAPSHOT | 2 |  |
| LIST_INFLIGHT_REQUEST | 3 |  |
| IMAGE_FS_STATS | 4 |  |



<a name="cubelet-services-cubebox-hostimage-v1-ResponseStatusCode"></a>

### ResponseStatusCode


| Name | Number | Description |
| ---- | ------ | ----------- |
| OK | 0 |  |
| ERROR | 1 |  |


 

 


<a name="cubelet-services-cubebox-hostimage-v1-CubeHostImageService"></a>

### CubeHostImageService


| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ReverseStreamForwardImage | [ClientMessage](#cubelet-services-cubebox-hostimage-v1-ClientMessage) stream | [ServerMessage](#cubelet-services-cubebox-hostimage-v1-ServerMessage) stream | ReverseStreamForwardImage Reverse forwarding image download interface: 1. After launching the sandbox, the cubelet on the host first attempts to send a hello message indicating successful connection. The server inside the VM should establish a context representing this connection is ready. 2. When the server inside the VM receives a CRI image download request, it should send a reverse forwardImageRequest to the host to request image download. 3. After completing the image download on the host, it should proactively notify the VM with the image information based on the request ID.

Note: When the host has not actively connected to the server inside the VM, the server is considered invalid and image forwarding is prohibited. |
| ListImages | [ListImagesRequest](#cubelet-services-cubebox-hostimage-v1-ListImagesRequest) | [ListImagesResponse](#cubelet-services-cubebox-hostimage-v1-ListImagesResponse) | ListImages lists existing images. |


<a name="cubelet-services-cubebox-hostimage-v1-CubeVMImageService"></a>

### CubeVMImageService
CubeVMImageService is used by cube image converter service in vm.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListImages | [ListImagesRequest](#cubelet-services-cubebox-hostimage-v1-ListImagesRequest) | [ListImagesResponse](#cubelet-services-cubebox-hostimage-v1-ListImagesResponse) | ListImages lists existing images. |
| PullImage | [ForwardImageRequest](#cubelet-services-cubebox-hostimage-v1-ForwardImageRequest) | [ForwardImageResponse](#cubelet-services-cubebox-hostimage-v1-ForwardImageResponse) | PullImage pulls an image from the host. |
| Mount | [MountImageRequest](#cubelet-services-cubebox-hostimage-v1-MountImageRequest) | [MountImageRequest](#cubelet-services-cubebox-hostimage-v1-MountImageRequest) | mount image to target path |

 



<a name="api_services_errorcode_v1_errorcode-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/errorcode/v1/errorcode.proto



<a name="cubelet-services-errorcode-v1-Ret"></a>

### Ret



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ret_code | [ErrorCode](#cubelet-services-errorcode-v1-ErrorCode) |  |  |
| ret_msg | [string](#string) |  |  |





 


<a name="cubelet-services-errorcode-v1-ErrorCode"></a>

### ErrorCode


| Name | Number | Description |
| ---- | ------ | ----------- |
| OK | 0 |  |
| Unknown | -1 |  |
| Success | 200 |  |
| GrpcError | 130500 |  |
| ListContainerFailed | 130504 |  |
| ContainerNotFound | 130505 |  |
| RemoveContainerFailed | 130506 |  |
| CreateContainerFailed | 130507 |  |
| StartContainerFailed | 130508 |  |
| BindContainerFailed | 130511 |  |
| ConcurrentFailed | 130513 |  |
| DownloadCodeFailed | 130514 |  |
| DownloadLayerFailed | 130516 |  |
| DiskNotEnough | 130519 |  |
| DeployCodeTimeout | 130524 |  |
| DeployLayerTimeout | 130525 |  |
| DeployCodeLayerTimeout | 130526 |  |
| NoSpaceLeftOnDevice | 130527 |  |
| RunContainerTimeout | 130539 |  |
| CreateStorageFailed | 130545 |  |
| CreateImageFailed | 130546 |  |
| CreateNetworkFailed | 130547 |  |
| InitHostFailed | 130548 |  |
| SetCubeboxCgroupLimitFailed | 130549 |  |
| LoadContainerFromMetaDataFailed | 130550 |  |
| DeleteTaskFailed | 130551 |  |
| DeleteContainerFromMetaDataFailed | 130552 |  |
| UpdateLocalMetaDataFailed | 130553 |  |
| ResolveLocalSpecFailed | 130554 |  |
| NewContainerMetaDataFailed | 130555 |  |
| NewTaskFailed | 130556 |  |
| WaitTaskFailed | 130557 |  |
| StartTaskFailed | 130558 |  |
| DestroyStorageFailed | 130559 |  |
| CreateShimlogFailed | 130560 |  |
| DestroyShimlogFailed | 130561 |  |
| CreateCgroupFailed | 130562 |  |
| DestroyCgroupFailed | 130563 |  |
| DestroyNetworkFailed | 130564 |  |
| UpdateLocalImageSpecFailed | 130565 |  |
| CreateVolumeFailed | 130566 |  |
| HostDiskNotEnough | 130567 |  |
| PreConditionFailed | 130568 |  |
| DestroyImageFailed | 130569 |  |
| TaskPauseFailed | 130588 |  |
| TaskResumeFailed | 130589 |  |
| InitCommandPathError | 130445 |  |
| ContainerStateExitedByUser | 130451 |  |
| PullImageFailed | 130456 |  |
| PullImageTimeOut | 130457 |  |
| PortBindingFailed | 130459 |  |
| PullImageTokenAuthFailed | 130460 |  |
| SquashfsMountFailed | 130461 |  |
| LoadUserProcTimeout | 130462 |  |
| ConcurrentLimited | 130480 |  |
| PreStopTimeOut | 130481 |  |
| PreStopFailed | 130482 |  |
| InvalidParamFormat | 130483 |  |
| TaskStateInvalid | 130490 |  |
| ExecCommandInSandboxFailed | 130497 | 尝试在沙箱中执行命令失败 |
| AppSnapshotNotExist | 130498 | 应用快照不存在 |


 

 

 



<a name="api_services_images_v1_images-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/images/v1/images.proto



<a name="cubelet-services-images-v1-CreateImageRequest"></a>

### CreateImageRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| spec | [ImageSpec](#cubelet-services-images-v1-ImageSpec) |  |  |






<a name="cubelet-services-images-v1-CreateImageRequestResponse"></a>

### CreateImageRequestResponse
CreateImageRequestResponse contains the response for CreateImageRequest


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | Unique identifier matching the request |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Result of the operation |






<a name="cubelet-services-images-v1-CubeListImageRequest"></a>

### CubeListImageRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| filter | [cubelet.types.ImageFilter](#cubelet-types-ImageFilter) |  | Filter to list images. |






<a name="cubelet-services-images-v1-CubeListImageResponse"></a>

### CubeListImageResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| images | [cubelet.types.Image](#cubelet-types-Image) | repeated | List of images. |






<a name="cubelet-services-images-v1-DestroyImageRequest"></a>

### DestroyImageRequest
DestroyImageRequest destroy image


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| spec | [ImageSpec](#cubelet-services-images-v1-ImageSpec) |  |  |






<a name="cubelet-services-images-v1-DestroyImageResponse"></a>

### DestroyImageResponse
DestroyImageResponse destroy image


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |






<a name="cubelet-services-images-v1-ImageSpec"></a>

### ImageSpec
ImageSpec is an internal representation of an image.
ImageSpec defines the specification of an image


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| image | [string](#string) |  | Container&#39;s Image field (e.g. imageID or imageDigest). |
| annotations | [ImageSpec.AnnotationsEntry](#cubelet-services-images-v1-ImageSpec-AnnotationsEntry) | repeated | annotations 传递username和token信息 ie. annotations[&#34;cube.image.username&#34;] = xxx ie. annotations[&#34;cube.image.token&#34;] = xxx |
| storage_media | [string](#string) |  | Type of storage media for the image (docker/ext4) |
| namespace | [string](#string) |  |  |
| runtime_handler | [string](#string) |  |  |






<a name="cubelet-services-images-v1-ImageSpec-AnnotationsEntry"></a>

### ImageSpec.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-services-images-v1-ImageVolumeSource"></a>

### ImageVolumeSource
ImageVolumeSource represents a image volume resource.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| reference | [ImageSpec](#cubelet-services-images-v1-ImageSpec) |  | Required: Image or artifact reference to be used. |
| pullPolicy | [PullPolicy](#cubelet-services-images-v1-PullPolicy) |  |  |






<a name="cubelet-services-images-v1-VolumUtilsRequest"></a>

### VolumUtilsRequest
VolumUtilsRequest volum utils


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| cmd | [string](#string) |  | cmd |
| input | [string](#string) |  | input |






<a name="cubelet-services-images-v1-VolumUtilsResponse"></a>

### VolumUtilsResponse
VolumUtilsResponse volum utils


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  | requestID reqID |
| ret | [cubelet.services.errorcode.v1.Ret](#cubelet-services-errorcode-v1-Ret) |  | Ret. |
| output | [string](#string) |  | output |





 


<a name="cubelet-services-images-v1-ImageStorageMediaType"></a>

### ImageStorageMediaType


| Name | Number | Description |
| ---- | ------ | ----------- |
| docker | 0 | Docker means normal oci container images.should be pulled from dockerhub |
| ext4 | 1 | image from local disk in ext4 format rootfs. |



<a name="cubelet-services-images-v1-PullPolicy"></a>

### PullPolicy
Policy for pulling OCI objects. Possible values are:
Always: the kubelet always attempts to pull the reference. Container creation will fail If the pull fails.
Never: the kubelet never pulls the reference and only uses a local image or artifact. Container creation will fail if the reference isn&#39;t present.
IfNotPresent: the kubelet pulls if the reference isn&#39;t already present on disk. Container creation will fail if the reference isn&#39;t present and the pull fails.
Defaults to Always if :latest tag is specified, or IfNotPresent otherwise.
&#43;optional

| Name | Number | Description |
| ---- | ------ | ----------- |
| Always | 0 | PullAlways means that kubelet always attempts to pull the latest image. Container will fail If the pull fails. |
| Never | 1 | PullNever means that kubelet never pulls an image, but only uses a local image. Container will fail if the image isn&#39;t present |
| IfNotPresent | 2 | PullIfNotPresent means that kubelet pulls if the image isn&#39;t present on disk. Container will fail if the image isn&#39;t present and the pull fails. |


 

 


<a name="cubelet-services-images-v1-Images"></a>

### Images
Service for handling image operations.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| CreateImage | [CreateImageRequest](#cubelet-services-images-v1-CreateImageRequest) | [CreateImageRequestResponse](#cubelet-services-images-v1-CreateImageRequestResponse) |  |
| ListImages | [CubeListImageRequest](#cubelet-services-images-v1-CubeListImageRequest) | [CubeListImageResponse](#cubelet-services-images-v1-CubeListImageResponse) | cubelet v1 not support list and destroy image |
| DestroyImage | [DestroyImageRequest](#cubelet-services-images-v1-DestroyImageRequest) | [DestroyImageResponse](#cubelet-services-images-v1-DestroyImageResponse) |  |
| VolumeUtils | [VolumUtilsRequest](#cubelet-services-images-v1-VolumUtilsRequest) | [VolumUtilsResponse](#cubelet-services-images-v1-VolumUtilsResponse) |  |

 



<a name="api_services_multimetadb_v1_multimeta_db-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/multimetadb/v1/multimeta_db.proto



<a name="cubelet-services-multimetadb-v1-BucketDefine"></a>

### BucketDefine
bucket define for boltdb


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | bucket 的名字 |
| is_namespace | [bool](#bool) |  |  |
| db_name | [string](#string) |  | 当使用独立db时，需要指定db的名字。例如cgroup插件使用了独立db |
| describe | [string](#string) |  | 对该bucket的描述 |






<a name="cubelet-services-multimetadb-v1-CommonRequestHeader"></a>

### CommonRequestHeader



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  |  |






<a name="cubelet-services-multimetadb-v1-CommonResponseHeader"></a>

### CommonResponseHeader



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  |  |
| code | [cubelet.services.errorcode.v1.ErrorCode](#cubelet-services-errorcode-v1-ErrorCode) |  |  |
| Message | [string](#string) |  |  |






<a name="cubelet-services-multimetadb-v1-DbData"></a>

### DbData



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  | key of this data |
| buckets | [string](#string) | repeated | bucket 的定义，可能如 erofs {namespace} 这种二级定义 |
| value | [bytes](#bytes) |  | 数据的value |






<a name="cubelet-services-multimetadb-v1-GetBucketDefinesResponse"></a>

### GetBucketDefinesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| header | [CommonResponseHeader](#cubelet-services-multimetadb-v1-CommonResponseHeader) |  |  |
| bucket_defines | [BucketDefine](#cubelet-services-multimetadb-v1-BucketDefine) | repeated |  |






<a name="cubelet-services-multimetadb-v1-GetDataRequest"></a>

### GetDataRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| header | [CommonRequestHeader](#cubelet-services-multimetadb-v1-CommonRequestHeader) |  |  |
| buckets | [string](#string) | repeated | bucket 的定义，可能如 erofs {namespace} 这种二级定义 |
| key | [string](#string) |  | 数据的key |
| db_name | [string](#string) |  | 当使用独立db时，需要指定db的名字。例如cgroup插件使用了独立db |





 

 

 


<a name="cubelet-services-multimetadb-v1-MultiMetaDBServer"></a>

### MultiMetaDBServer
Service for machine level operations.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| GetBucketDefines | [CommonRequestHeader](#cubelet-services-multimetadb-v1-CommonRequestHeader) | [GetBucketDefinesResponse](#cubelet-services-multimetadb-v1-GetBucketDefinesResponse) |  |
| GetStreamData | [GetDataRequest](#cubelet-services-multimetadb-v1-GetDataRequest) | [DbData](#cubelet-services-multimetadb-v1-DbData) stream | 使用流式接口，批量查询db中的数据 |

 



<a name="api_services_nbi_v1_cubelet_api-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/nbi/v1/cubelet_api.proto



<a name="cubelet-services-cubebox-v1-InitRequest"></a>

### InitRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  |  |






<a name="cubelet-services-cubebox-v1-InitResponse"></a>

### InitResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requestID | [string](#string) |  |  |
| code | [cubelet.services.errorcode.v1.ErrorCode](#cubelet-services-errorcode-v1-ErrorCode) |  |  |
| Message | [string](#string) |  |  |
| ext_info | [InitResponse.ExtInfoEntry](#cubelet-services-cubebox-v1-InitResponse-ExtInfoEntry) | repeated |  |






<a name="cubelet-services-cubebox-v1-InitResponse-ExtInfoEntry"></a>

### InitResponse.ExtInfoEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [bytes](#bytes) |  |  |





 

 

 


<a name="cubelet-services-cubebox-v1-CubeLet"></a>

### CubeLet
Service for machine level operations.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| InitHost | [InitRequest](#cubelet-services-cubebox-v1-InitRequest) | [InitResponse](#cubelet-services-cubebox-v1-InitResponse) | Initialize the host, destroy all of the container and initialize metadata. This is a dangerous operation. |

 



<a name="api_services_version_v1_version-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/services/version/v1/version.proto



<a name="cubelet-services-version-v1-VersionResponse"></a>

### VersionResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| version | [string](#string) |  |  |
| revision | [string](#string) |  |  |





 

 

 


<a name="cubelet-services-version-v1-Version"></a>

### Version


| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| Version | [.google.protobuf.Empty](#google-protobuf-Empty) | [VersionResponse](#cubelet-services-version-v1-VersionResponse) |  |

 



<a name="api_types_v1_descriptor-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/types/v1/descriptor.proto



<a name="cubelet-types-Descriptor"></a>

### Descriptor
Descriptor describes a blob in a content store.

This descriptor can be used to reference content from an
oci descriptor found in a manifest.
See https://godoc.org/github.com/opencontainers/image-spec/specs-go/v1#Descriptor


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| media_type | [string](#string) |  |  |
| digest | [string](#string) |  |  |
| size | [int64](#int64) |  |  |
| annotations | [Descriptor.AnnotationsEntry](#cubelet-types-Descriptor-AnnotationsEntry) | repeated |  |
| data | [google.protobuf.Any](#google-protobuf-Any) |  | Any is a message type wrapper allowing you to attach arbitrary metadata to a blob. |






<a name="cubelet-types-Descriptor-AnnotationsEntry"></a>

### Descriptor.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |





 

 

 

 



<a name="api_types_v1_images-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## api/types/v1/images.proto



<a name="cubelet-types-AuthConfig"></a>

### AuthConfig
AuthConfig contains authorization information for connecting to a registry.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| username | [string](#string) |  |  |
| password | [string](#string) |  |  |
| auth | [string](#string) |  |  |
| serverAddress | [string](#string) |  |  |
| identityToken | [string](#string) |  | IdentityToken is used to authenticate the user and get an access token for the registry. |
| registryToken | [string](#string) |  | RegistryToken is a bearer token to be sent to a registry |






<a name="cubelet-types-Image"></a>

### Image
Basic information about a container image.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | ID of the image. |
| repoTags | [string](#string) | repeated | Other names by which this image is known. |
| repoDigests | [string](#string) | repeated | Digests by which this image is known. |
| size | [uint64](#uint64) |  | Size of the image in bytes. Must be &gt; 0. |
| uid | [Int64Value](#cubelet-types-Int64Value) |  | UID that will run the command(s). This is used as a default if no user is specified when creating the container. UID and the following user name are mutually exclusive. |
| username | [string](#string) |  | User name that will run the command(s). This is used if UID is not set and no user is specified when creating container. |
| spec | [ImageSpec](#cubelet-types-ImageSpec) |  | ImageSpec for image which includes annotations |
| pinned | [bool](#bool) |  | Recommendation on whether this image should be exempt from garbage collection. It must only be treated as a recommendation -- the client can still request that the image be deleted, and the runtime must oblige. |
| namespace | [string](#string) |  | namespace of image, this field is used for cube image management. |






<a name="cubelet-types-ImageFilter"></a>

### ImageFilter



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| image | [ImageSpec](#cubelet-types-ImageSpec) |  | Spec of the image. |






<a name="cubelet-types-ImageSpec"></a>

### ImageSpec
ImageSpec is an internal representation of an image.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| image | [string](#string) |  | Container&#39;s Image field (e.g. imageID or imageDigest). |
| annotations | [ImageSpec.AnnotationsEntry](#cubelet-types-ImageSpec-AnnotationsEntry) | repeated | Unstructured key-value map holding arbitrary metadata. ImageSpec Annotations can be used to help the runtime target specific images in multi-arch images. |






<a name="cubelet-types-ImageSpec-AnnotationsEntry"></a>

### ImageSpec.AnnotationsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="cubelet-types-Int64Value"></a>

### Int64Value
Int64Value is the wrapper of int64.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| value | [int64](#int64) |  | The value. |





 

 

 

 



## Scalar Value Types

| .proto Type | Notes | C++ | Java | Python | Go | C# | PHP | Ruby |
| ----------- | ----- | --- | ---- | ------ | -- | -- | --- | ---- |
| <a name="double" /> double |  | double | double | float | float64 | double | float | Float |
| <a name="float" /> float |  | float | float | float | float32 | float | float | Float |
| <a name="int32" /> int32 | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint32 instead. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="int64" /> int64 | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint64 instead. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="uint32" /> uint32 | Uses variable-length encoding. | uint32 | int | int/long | uint32 | uint | integer | Bignum or Fixnum (as required) |
| <a name="uint64" /> uint64 | Uses variable-length encoding. | uint64 | long | int/long | uint64 | ulong | integer/string | Bignum or Fixnum (as required) |
| <a name="sint32" /> sint32 | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int32s. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="sint64" /> sint64 | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int64s. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="fixed32" /> fixed32 | Always four bytes. More efficient than uint32 if values are often greater than 2^28. | uint32 | int | int | uint32 | uint | integer | Bignum or Fixnum (as required) |
| <a name="fixed64" /> fixed64 | Always eight bytes. More efficient than uint64 if values are often greater than 2^56. | uint64 | long | int/long | uint64 | ulong | integer/string | Bignum |
| <a name="sfixed32" /> sfixed32 | Always four bytes. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="sfixed64" /> sfixed64 | Always eight bytes. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="bool" /> bool |  | bool | boolean | boolean | bool | bool | boolean | TrueClass/FalseClass |
| <a name="string" /> string | A string must always contain UTF-8 encoded or 7-bit ASCII text. | string | String | str/unicode | string | string | string | String (UTF-8) |
| <a name="bytes" /> bytes | May contain any arbitrary sequence of bytes. | string | ByteString | str | []byte | ByteString | string | String (ASCII-8BIT) |
