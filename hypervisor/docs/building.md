- [Building Cloud Hypervisor](#building-cloud-hypervisor)
  - [Preparation](#preparation)
  - [Install prerequisites](#install-prerequisites)
  - [Clone and build](#clone-and-build)
    - [Containerized builds and tests](#containerized-builds-and-tests)

# Building Cloud Hypervisor

We recommend users use the pre-built binaries that are mentioned in the README.md file in the root of the repository. Building from source is only necessary if you wish to make modifications.

## Preparation

We create a folder to build and run `cloud-hypervisor` at `$HOME/cloud-hypervisor`

```shell
$ export CLOUDH=$HOME/cloud-hypervisor
$ mkdir $CLOUDH
```

## Install prerequisites

You need to install some prerequisite packages in order to build and test Cloud
Hypervisor. Here, all the steps are based on Ubuntu, for other Linux
distributions please replace the package manager and package name.

```shell
# Install build-essential, git, and qemu-utils
$ sudo apt install git build-essential qemu-utils
# Install rust tool chain
$ curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
# If you want to build statically linked binary please add musl target
$ rustup target add x86_64-unknown-linux-musl
```

## Clone and build

First you need to clone and build the Cloud Hypervisor repository:

```shell
$ pushd $CLOUDH
$ git clone https://github.com/cloud-hypervisor/cloud-hypervisor.git
$ cd cloud-hypervisor
$ cargo build --release

# We need to give the cloud-hypervisor binary the NET_ADMIN capabilities for it to set TAP interfaces up on the host.
$ sudo setcap cap_net_admin+ep ./target/release/cloud-hypervisor

# If you want to build statically linked binary
$ cargo build --release --target=x86_64-unknown-linux-musl --all
$ popd
```

This will build a `cloud-hypervisor` binary under
`$CLOUDH/cloud-hypervisor/target/release/cloud-hypervisor`.

### Containerized builds and tests

If you want to build and test Cloud Hypervisor without having to install all the
required dependencies (The rust toolchain, cargo tools, etc), you can also use
Cloud Hypervisor's development script: `dev_cli.sh`. Please note that upon its
first invocation, this script will pull a fairly large container image.

For example, to build the Cloud Hypervisor release binary:

```shell
$ pushd $CLOUDH
$ cd cloud-hypervisor
$ ./scripts/dev_cli.sh build --release
```

With `dev_cli.sh`, one can also run the Cloud Hypervisor CI locally. This can be
very convenient for debugging CI errors without having to fully rely on the
Cloud Hypervisor CI infrastructure.

For example, to run the Cloud Hypervisor unit tests:

```shell
$ ./scripts/dev_cli.sh tests --unit
```

Run the `./scripts/dev_cli.sh --help` command to view all the supported
development script commands and their related options.
