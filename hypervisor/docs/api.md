- [Cloud Hypervisor API](#cloud-hypervisor-api)
  - [External API](#external-api)
    - [REST API](#rest-api)
    - [Location and availability](#location-and-availability)
    - [Endpoints](#endpoints)
      - [Virtual Machine Manager (VMM) Actions](#virtual-machine-manager-vmm-actions)
      - [Virtual Machine (VM) Actions](#virtual-machine-vm-actions)
    - [REST API Examples](#rest-api-examples)
      - [Create a Virtual Machine](#create-a-virtual-machine)
      - [Boot a Virtual Machine](#boot-a-virtual-machine)
      - [Dump a Virtual Machine Information](#dump-a-virtual-machine-information)
      - [Reboot a Virtual Machine](#reboot-a-virtual-machine)
      - [Shut a Virtual Machine Down](#shut-a-virtual-machine-down)
    - [Command Line Interface](#command-line-interface)
    - [REST API and CLI Architectural Relationship](#rest-api-and-cli-architectural-relationship)
  - [Internal API](#internal-api)
    - [Goals and Design](#goals-and-design)
  - [End to End Example](#end-to-end-example)

# Cloud Hypervisor API

The Cloud Hypervisor API is made of 2 distinct interfaces:

1. **The external API**. This is the user facing API. Users and operators can
   control and manage Cloud Hypervisor through either a REST API or a Command
   Line Interface (CLI).
1. **The internal API**, based on [rust's Multi-Producer, Single-Consumer (MPSC)](https://doc.rust-lang.org/std/sync/mpsc/)
   module. This API is used internally by the Cloud Hypervisor threads to
   communicate between each others.

The goal of this document is to describe the Cloud Hypervisor API as a whole,
and to outline how the internal and external APIs are architecturally related.

## External API

### REST API

The Cloud Hypervisor [REST](https://en.wikipedia.org/wiki/Representational_state_transfer)
API triggers VM and VMM specific actions, and as such it is designed as a
collection of RPC-style, static methods.

The API is [OpenAPI 3.0](https://github.com/OAI/OpenAPI-Specification/blob/master/versions/3.0.0.md)
compliant. Please consult the [Cloud Hypervisor API](https://raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/master/vmm/src/api/openapi/cloud-hypervisor.yaml)
document for more details about the API payloads and responses.

### Location and availability

The REST API is available as soon as the Cloud Hypervisor binary is started,
through a local UNIX socket.
By default, it is located at `/run/user/{user ID}/cloud-hypervisor.{Cloud Hypervisor PID}`.
For example, if you launched Cloud Hypervisor as user ID 1000 and its PID is
123456, the Cloud Hypervisor REST API will be available at `/run/user/1000/cloud-hypervisor.123456`.

The REST API default URL can be overridden through the Cloud Hypervisor
option `--api-socket`:

```
$ ./target/debug/cloud-hypervisor --api-socket /tmp/cloud-hypervisor.sock
Cloud Hypervisor Guest
    API server: /tmp/cloud-hypervisor.sock
    vCPUs: 1
    Memory: 512 MB
    Kernel: None
    Kernel cmdline:
    Disk(s): None
```

### Endpoints

The Cloud Hypervisor API exposes the following actions through its endpoints:

#### Virtual Machine Manager (VMM) Actions

| Action                              | Endpoint        | Request Body | Response Body              | Prerequisites      |
| ----------------------------------- | --------------- | ------------ | -------------------------- | ------------------ |
| Check for the REST API availability | `/vmm.ping`     | N/A          | `/schemas/VmmPingResponse` | N/A                |
| Shut the VMM down                   | `/vmm.shutdown` | N/A          | N/A                        | The VMM is running |

#### Virtual Machine (VM) Actions

| Action                             | Endpoint              | Request Body                | Response Body            | Prerequisites                    |
| ---------------------------------- | --------------------- | --------------------------- | ------------------------ | -------------------------------- |
| Create the VM                      | `/vm.create`          | `/schemas/VmConfig`         | N/A                      | The VM is not created yet        |
| Delete the VM                      | `/vm.delete`          | N/A                         | N/A                      | N/A                              |
| Boot the VM                        | `/vm.boot`            | N/A                         | N/A                      | The VM is created but not booted |
| Shut the VM down                   | `/vm.shutdown`        | N/A                         | N/A                      | The VM is booted                 |
| Reboot the VM                      | `/vm.reboot`          | N/A                         | N/A                      | The VM is booted                 |
| Trigger power button of the VM     | `/vm.power-button`    | N/A                         | N/A                      | The VM is booted                 |
| Pause the VM                       | `/vm.pause`           | N/A                         | N/A                      | The VM is booted                 |
| Resume the VM                      | `/vm.resume`          | N/A                         | N/A                      | The VM is paused                 |
| Task a snapshot of the VM          | `/vm.snapshot`        | `/schemas/VmSnapshotConfig` | N/A                      | The VM is paused                 |
| Perform a coredump of the VM       | `/vm.coredump`        | `/schemas/VmCoredumpData`   | N/A                      | The VM is paused                 |
| Restore the VM from a snapshot     | `/vm.restore`         | `/schemas/RestoreConfig`    | N/A                      | The VM is created but not booted |
| Add/remove CPUs to/from the VM     | `/vm.resize`          | `/schemas/VmResize`         | N/A                      | The VM is booted                 |
| Add/remove memory from the VM      | `/vm.resize`          | `/schemas/VmResize`         | N/A                      | The VM is booted                 |
| Add/remove memory from a zone      | `/vm.resize-zone`     | `/schemas/VmResizeZone`     | N/A                      | The VM is booted                 |
| Dump the VM information            | `/vm.info`            | N/A                         | `/schemas/VmInfo`        | The VM is created                |
| Add VFIO PCI device to the VM      | `/vm.add-device`      | `/schemas/VmAddDevice`      | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add disk device to the VM          | `/vm.add-disk`        | `/schemas/DiskConfig`       | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add fs device to the VM            | `/vm.add-fs`          | `/schemas/FsConfig`         | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add pmem device to the VM          | `/vm.add-pmem`        | `/schemas/PmemConfig`       | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add network device to the VM       | `/vm.add-net`         | `/schemas/NetConfig`        | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add userspace PCI device to the VM | `/vm.add-user-device` | `/schemas/VmAddUserDevice`  | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add vdpa device to the VM          | `/vm.add-vdpa`        | `/schemas/VdpaConfig`       | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Add vsock device to the VM         | `/vm.add-vsock`       | `/schemas/VsockConfig`      | `/schemas/PciDeviceInfo` | The VM is booted                 |
| Remove device from the VM          | `/vm.remove-device`   | `/schemas/VmRemoveDevice`   | N/A                      | The VM is booted                 |
| Dump the VM counters               | `/vm.counters`        | N/A                         | `/schemas/VmCounters`    | The VM is booted                 |

### REST API Examples

For the following set of examples, we assume Cloud Hypervisor is started with
the REST API available at `/tmp/cloud-hypervisor.sock`:

```
$ ./target/debug/cloud-hypervisor --api-socket /tmp/cloud-hypervisor.sock
Cloud Hypervisor Guest
    API server: /tmp/cloud-hypervisor.sock
    vCPUs: 1
    Memory: 512 MB
    Kernel: None
    Kernel cmdline:
    Disk(s): None
```

#### Create a Virtual Machine

We want to create a virtual machine with the following characteristics:

* 4 vCPUs
* 1 GB of RAM
* 1 virtio based networking interface
* Direct kernel boot from a custom 5.6.0-rc4 Linux kernel located at
  `/opt/clh/kernel/vmlinux-virtio-fs-virtio-iommu`
* Using a Ubuntu image as its root filesystem, located at
  `/opt/clh/images/focal-server-cloudimg-amd64.raw`

```shell
#!/bin/bash

curl --unix-socket /tmp/cloud-hypervisor.sock -i \
     -X PUT 'http://localhost/api/v1/vm.create'  \
     -H 'Accept: application/json'               \
     -H 'Content-Type: application/json'         \
     -d '{
         "cpus":{"boot_vcpus": 4, "max_vcpus": 4},
         "payload":{"kernel":"/opt/clh/kernel/vmlinux-virtio-fs-virtio-iommu", "cmdline":"console=ttyS0 console=hvc0 root=/dev/vda1 rw"},
         "disks":[{"path":"/opt/clh/images/focal-server-cloudimg-amd64.raw"}],
         "rng":{"src":"/dev/urandom"},
         "net":[{"ip":"192.168.10.10", "mask":"255.255.255.0", "mac":"12:34:56:78:90:01"}]
         }'
```

#### Boot a Virtual Machine

Once the VM is created, we can boot it:

```shell
#!/bin/bash

curl --unix-socket /tmp/cloud-hypervisor.sock -i -X PUT 'http://localhost/api/v1/vm.boot'
```

#### Dump a Virtual Machine Information

We can fetch information about any VM, as soon as it's created:

```shell
#!/bin/bash

curl --unix-socket /tmp/cloud-hypervisor.sock -i \
     -X GET 'http://localhost/api/v1/vm.info' \
     -H 'Accept: application/json'
```

#### Reboot a Virtual Machine

We can reboot a VM that's already booted:

```shell
#!/bin/bash

curl --unix-socket /tmp/cloud-hypervisor.sock -i -X PUT 'http://localhost/api/v1/vm.reboot'
```

#### Shut a Virtual Machine Down

Once booted, we can shut a VM down from the REST API:

```shell
#!/bin/bash

curl --unix-socket /tmp/cloud-hypervisor.sock -i -X PUT 'http://localhost/api/v1/vm.shutdown'
```

### Command Line Interface

The Cloud Hypervisor Command Line Interface (CLI) can only be used for launching
the Cloud Hypervisor binary, i.e. it can not be used for controlling the VMM or
the launched VM once they're up and running.

If you want to inspect the VMM, or control the VM after launching Cloud
Hypervisor from the CLI, you must use the [REST API](#rest-api).

From the CLI, one can either:

1. Create and boot a complete virtual machine by using the CLI options to build
   the VM config. Run `cloud-hypervisor --help` for a complete list of CLI
   options. As soon as the `cloud-hypervisor` binary is launched, the
   [REST API](#rest-api) is available for controlling and managing the VM.
1. Start the [REST API](#rest-api) server only, by not passing any VM
   configuration options. The VM can then be asynchronously created and booted
   by sending HTTP commands to the [REST API](#rest-api). Check the
   [REST API examples](#rest-api-examples) section for more details.

### REST API and CLI Architectural Relationship

The REST API and the CLI both rely on a common, [internal API](#internal-api).

The CLI options are parsed by the
[clap crate](https://docs.rs/clap/2.33.0/clap/) and then translated into
[internal API](#internal-api) commands.

The REST API is processed by an HTTP thread using the
[Firecracker's `micro_http`](https://github.com/firecracker-microvm/firecracker/tree/master/src/micro_http)
crate. As with the CLI, the HTTP requests eventually get translated into
[internal API](#internal-api) commands.

As a summary, the REST API and the CLI are essentially frontends for the
[internal API](#internal-api):

```
                                  +------------------+
                        REST API  |                  |
                       +--------->+    micro_http    +--------+
                       |          |                  |        |
                       |          +------------------+        |
                       |                                      |      +------------------------+
                       |                                      |      |                        |
+------------+         |                                      |      |                        |
|            |         |                                      |      | +--------------+       |
|    User    +---------+                                      +------> | Internal API |       |
|            |         |                                      |      | +--------------+       |
+------------+         |                                      |      |                        |
                       |                                      |      |                        |
                       |                                      |      +------------------------+
                       |            +----------+              |                 VMM
                       |     CLI    |          |              |
                       +----------->+   clap   +--------------+
                                    |          |
                                    +----------+


```

## Internal API

The Cloud Hypervisor internal API, as its name suggests, is used internally
by the different Cloud Hypervisor threads (VMM, HTTP, control loop, etc) to
send commands and responses to each others.

It is based on [rust's Multi-Producer, Single-Consumer (MPSC)](https://doc.rust-lang.org/std/sync/mpsc/),
and the single consumer (a.k.a. the API receiver) is the Cloud Hypervisor
control loop.

API producers are the HTTP thread handling the [REST API](#rest-api) and the
main thread that initially parses the [CLI](#command-line-interface).

### Goals and Design

The internal API is designed for controlling, managing and inspecting a Cloud
Hypervisor VMM and its guest. It is a backend for handling external, user
visible requests through either the [REST API](#rest-api) or the
[CLI](#command-line-interface) interfaces.

The API follows a command-response scheme that closely maps the [REST API](#rest-api).
Any command must be replied to with a response.

Commands are [MPSC](https://doc.rust-lang.org/std/sync/mpsc/) based messages and
are received and processed by the VMM control loop.

In order for the VMM control loop to respond to any internal API command, it
must be able to send a response back to the MPSC sender. For that purpose, all
internal API command payload carry the [Sender](https://doc.rust-lang.org/std/sync/mpsc/struct.Sender.html)
end of an [MPSC](https://doc.rust-lang.org/std/sync/mpsc/) channel.

The sender of any internal API command is therefore responsible for:

1. Creating an [MPSC](https://doc.rust-lang.org/std/sync/mpsc/) response
   channel.
1. Passing the [Sender](https://doc.rust-lang.org/std/sync/mpsc/struct.Sender.html)
   end of the response channel as part of the internal API command payload.
1. Waiting for the internal API command's response on the [Receiver](https://doc.rust-lang.org/std/sync/mpsc/struct.Receiver.html)
   end of the response channel.

## End to End Example

In order to further understand how the external and internal Cloud Hypervisor
APIs work together, let's look at a complete VM creation flow, from the
[REST API](#rest-api) call, to the reply the external user will receive:

1. A user or operator sends an HTTP request to the Cloud Hypervisor
   [REST API](#rest-api) in order to creates a virtual machine:
   ```
   shell
   #!/bin/bash

	curl --unix-socket /tmp/cloud-hypervisor.sock -i \
		-X PUT 'http://localhost/api/v1/vm.create'  \
		-H 'Accept: application/json'               \
		-H 'Content-Type: application/json'         \
		-d '{
			"cpus":{"boot_vcpus": 4, "max_vcpus": 4},
			"payload":{"kernel":"/opt/clh/kernel/vmlinux-virtio-fs-virtio-iommu", "cmdline":"console=ttyS0 console=hvc0 root=/dev/vda1 rw"},
			"disks":[{"path":"/opt/clh/images/focal-server-cloudimg-amd64.raw"}],
			"rng":{"src":"/dev/urandom"},
			"net":[{"ip":"192.168.10.10", "mask":"255.255.255.0", "mac":"12:34:56:78:90:01"}]
			}'
   ```
1. The Cloud Hypervisor HTTP thread processes the request and de-serializes the
   HTTP request JSON body into an internal `VmConfig` structure.
1. The Cloud Hypervisor HTTP thread creates an
   [MPSC](https://doc.rust-lang.org/std/sync/mpsc/) channel for the internal API
   server to send its response back.
1. The Cloud Hypervisor HTTP thread prepares an internal API command for creating a
   virtual machine. The command's payload is made of the de-serialized
   `VmConfig` structure and the response channel:
   ```Rust
   VmCreate(Arc<Mutex<VmConfig>>, Sender<ApiResponse>)
   ```
1. The Cloud Hypervisor HTTP thread sends the internal API command, and waits
   for the response:
   ```Rust
   // Send the VM creation request.
    api_sender
        .send(ApiRequest::VmCreate(config, response_sender))
        .map_err(ApiError::RequestSend)?;
    api_evt.write(1).map_err(ApiError::EventFdWrite)?;

    response_receiver.recv().map_err(ApiError::ResponseRecv)??;
   ```
1. The Cloud Hypervisor control loop receives the command, as it listens on the
   internal API [MPSC](https://doc.rust-lang.org/std/sync/mpsc/) channel:
   ```Rust
   // Read from the API receiver channel
   let api_request = api_receiver.recv().map_err(Error::ApiRequestRecv)?;
   ```
1. The Cloud Hypervisor control loop matches the received internal API against
   the `VmCreate` payload, and extracts both the `VmConfig` structure and the
   [Sender](https://doc.rust-lang.org/std/sync/mpsc/struct.Sender.html) from the
   command payload. It stores the `VmConfig` structure and replies back to the
   sender ((The HTTP thread):
   ```Rust
   match api_request {
	   ApiRequest::VmCreate(config, sender) => {
		   // We only store the passed VM config.
		   // The VM will be created when being asked to boot it.
		   let response = if self.vm_config.is_none() {
			   self.vm_config = Some(config);
			   Ok(ApiResponsePayload::Empty)
		   } else {
			   Err(ApiError::VmAlreadyCreated)
		   };

	       sender.send(response).map_err(Error::ApiResponseSend)?;
	   }
   ```
1. The Cloud Hypervisor HTTP thread receives the internal API command response
   as the return value from its `VmCreate` HTTP handler. Depending on the
   control loop internal API response, it generates the appropriate HTTP
   response:
   ```Rust
   // Call vm_create()
   match vm_create(api_notifier, api_sender, Arc::new(Mutex::new(vm_config)))
	   .map_err(HttpError::VmCreate)
   {
	   Ok(_) => Response::new(Version::Http11, StatusCode::NoContent),
	   Err(e) => error_response(e, StatusCode::InternalServerError),
   }
   ```
1. The Cloud Hypervisor HTTP thread sends the formed HTTP response back to the
   user. This is abstracted by the
   [micro_http](https://github.com/firecracker-microvm/firecracker/tree/master/src/micro_http)
   crate.
