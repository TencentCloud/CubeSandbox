# VSOCK support

VSOCK provides a way for guest and host to communicate through a socket. VSOCK sockets support both stream and datagram types.

The `virtio-vsock` is based on the [Firecracker](https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md) implementation, where additional details can be found.

## What is a CID?

CID is a 32-bit context identifier describing the source or destination. In combination with the port, the complete addressing can be achieved to describe multiple listeners running on the same machine.

The table below depicts the well known CID values:

| CID | Description |
|-----|-------------|
| -1  | Random CID | 
|  0  | Hypervisor | 
|  1  | Loopback | 
|  2  | Host | 

## Prerequisites

### Kernel Requirements

Host kernel: CONFIG_VHOST_VSOCK

Guest kernel: CONFIG_VIRTIO_VSOCKETS

### Nested VM support

Linux __v5.5__ or newer is required for the L1 VM.

### Loopback support

Linux __v5.6__ or newer is required.

## Establishing VSOCK Connection

VSOCK device becomes available with `--vsock` option passed by the VM start. Cloud Hypervisor can be invoked for instance as below:

```bash
cloud-hypervisor \
	--cpus boot=1 \
	--memory size=4G \
	--firmware CLOUDHV.fd \
	--disk path=jammy-server-cloudimg.raw \
	--vsock cid=3,socket=/tmp/ch.vsock
```

The examples use __socat__ `>=1.7.4` to illustrate the VSOCK functionality. However, there are other tools supporting VSOCK, like [ncat](https://stefano-garzarella.github.io/posts/2019-11-08-kvmforum-2019-vsock/).

### Connecting from Host to Guest

The host starts to listen on the defined port:

`$ socat - VSOCK-LISTEN:1234`

Once the host is listening, the guest can send data:

`echo -e "CONNECT 1234\\nHello from host!" | socat - UNIX-CONNECT:/tmp/ch.vsock
 
Note the string `CONNECT <port>` prepended to the actual data. It is possible for the guest to start listening on different ports, thus the specific command is needed to instruct VSOCK to which listener the host wants to connect. It needs to be sent once per connection. Once the connection established, data transfers can take place directly.

### Connecting from Guest to Host

This first requires a listening UNIX socket on the host side. The UNIX socket path has to be constructed by using the socket path used at the VM launch time with appended `_` and the port number to be used on the guest side. As in the example above, if we'd intended to connect from the guest to the port `1234`, the Unix socket path on the host side would be `/tmp/ch.vsock_1234`.

Also note that the CID used on the guest side is the well known CID value `2`.

Listening on the host side:

`$ socat - UNIX-LISTEN:/tmp/ch.vsock_1234`

From the guest:

`$ echo -e "Hello from guest!" | socat - VSOCK-CONNECT:2:1234`

## Links

- [virtio-vsock in QEMU, Firecracker and Linux: Status, Performance and Challenges](https://kvmforum2019.sched.com/event/TmwK)
- [Leveraging virtio-vsock in the cloud and containers](https://archive.fosdem.org/2021/schedule/event/vai_virtio_vsock/)
- [VSOCK man page](https://manpages.ubuntu.com/manpages/focal/man7/vsock.7.html)
- [https://stefano-garzarella.github.io/posts/2020-02-20-vsock-nested-vms-loopback/](https://stefano-garzarella.github.io/posts/2020-02-20-vsock-nested-vms-loopback/)
- [https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md)

