# Protocol Documentation
<a name="top"></a>

## Table of Contents

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
  
- [Scalar Value Types](#scalar-value-types)



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
| vm_device_path | [string](#string) |  | vm_device_path is the path of the device in the VM. this fileds is only used in the VM. |
| vm_root_path | [string](#string) |  | this fileds is only used in the VM. |
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
| dev_serial_id | [string](#string) |  | dev_serial_id is the serial id of the device. this fileds is only used when the mount is a device.like erofs. When a layer belongs to a certain device, it is not allowed to use the `host_path` field again |
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

