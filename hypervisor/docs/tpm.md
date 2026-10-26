# TPM
Tpm in Cloud-Hypervisor is emulated using `swtpm` as the backend. [swtpm](https://github.com/stefanberger/swtpm) is the link to swtpm project.

Current implementation only supports TPM `2.0` version. At the moment only
`CRB Interface` is implemented. This interface is described in
[TCG PC Client Platform TPM Profile Specification for TPM 2.0, Revision 01.05 v4](https://trustedcomputinggroup.org/wp-content/uploads/PC-Client-Specific-Platform-TPM-Profile-for-TPM-2p0-v1p05p_r14_pub.pdf).


## Usage
`--tpm`, an optional argument, can be passed to enable tpm device.
This argument takes an UNIX domain Socket as a `socket` value.

_Example_

An Example invocation with `--tpm` argument:

```
 ./cloud-hypervisor/target/release/cloud-hypervisor \
	--kernel ./hypervisor-fw \
	--disk path=focal-server-cloudimg-amd64.raw \
	--cpus boot=4 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask=" \
	--tpm socket="/var/run/swtpm.socket"
```

## swtpm
Before invoking cloud-hypervisor with `--tpm` argument, a `swtpm`
process should be started to listen at the input socket. Below is an
example invocation of swtpm process.

```
swtpm socket --tmpstate dir=/var/run/swtpm \
	--ctrl type=unixio,path="/var/run/swtpm.socket" \
	--flags startup-clear \
	--tpm2
```

## Guest
After starting a guest with the above commands, ensure below listed modules are
loaded in the guest:

```
# lsmod | grep tpm
tpm_crb                20480  0
tpm                    81920  1 tpm_crb
```

Below is the IO Memory map configured in the guest:

```
# cat /proc/iomem  | grep MSFT
fed40000-fed40fff : MSFT0101:00
  fed40000-fed40fff : MSFT0101:00
```
Below are the devices created in the guest:

```
# ls /dev/tpm*
/dev/tpm0  /dev/tpmrm0
```


## Testing

Inside the guest install `tpm2-tools` package. This package provides some
commands to run against TPM that supports 2.0 version.

_Examples_
```
// Run Self Test
# tpm2_selftest -f
# echo $?
0


# echo "hello" > input.txt
// this command generates hash of the input file using all the algos supported by TPM

# tpm2_pcrevent input.txt
sha1: f572d396fae9206628714fb2ce00f72e94f2258f
sha256: 5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03
sha384: 1d0f284efe3edea4b9ca3bd514fa134b17eae361ccc7a1eefeff801b9bd6604e01f21f6bf249ef030599f0c
218f2ba8c
sha512: e7c22b994c59d9cf2b48e549b1e24666636045930d3da7c1acb299d1c3b7f931f94aae41edda2c2b207a36e
10f8bcb8d45223e54878f5b316e7ce3b6bc019629

// verify one of the hashes
# sha256sum input.txt
5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  input.txt
```

### Bundled Functional Test

Build time dependencies for `tpm2-tss` are captured in [INSTALL](https://github.com/tpm2-software/tpm2-tss/blob/master/INSTALL.md).

```
# git clone https://github.com/tpm2-software/tpm2-tss.git
# cd tpm2-tss
# ./configure --enable-integration --with-devicetests="mandatory,optional" --with-device=/dev/tpm0
# sudo make check-device
.
.
.
.
============================================================================
Testsuite summary for tpm2-tss 3.2.0-74-ge03617d9
============================================================================
# TOTAL: 154
# PASS:  88
# SKIP:  7
# XFAIL: 0
# FAIL:  59
# XPASS: 0
# ERROR: 0
============================================================================
See ./test-suite.log
Please report to https://github.com/tpm2-software/tpm2-tss/issues
============================================================================
```
The same set of failures are noticed while running these tests on `Qemu` with
its TPM implementation.
