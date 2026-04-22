# Windows Support

Starting with the release version [0.10.0](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v0.10.0), Cloud Hypervisor supports Windows guests.

__Requirements__

- Host with KVM enabled 
- [UEFI](uefi.md) capable Windows guest image with Virtio drivers integrated

Any modern Windows Server version is compatible. Cloud Hypervisor has been successfully tested with Windows Server 2019 and Windows Server Core 2004.

At the current stage, only UEFI capable Windows images are supported. This implies the presence of the OVMF firmware during the Windows installation and in any subsequent usage. BIOS boot is not supported.

The subsequent sections will tell, in detail, how to prepare an appropriate Windows image.

## Image Preparation

### Installation using the stock Windows ISO

__Prerequisites__

- QEMU, version >=5.0.0 is recommended.
- Windows installation ISO. Obtained through MSDN, Visual Studio subscription, evaluation center, etc. 
- [VirtIO driver ISO](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/)
- Suitable firmware for Cloud Hypervisor (`CLOUDHV.fd`) and for QEMU (`OVMF.fd`)
- With the suggested image size of 30G, there should be enough free disk space to hold the installation ISO and any other necessary files

This step currently requires QEMU to install Windows onto the guest. QEMU is only used at the preparation stage, the resulting image is then fully functional with Cloud Hypervisor.

Preparing several command parts as these will be used in the follow up sections as well.

```shell
IMG_FILE=windows-disk.raw
WIN_ISO_FILE=en_windows_server_version_2004_updated_may_2020_x64_dvd_1e7f1cfa.iso
VIRTIO_ISO_FILE=virtio-win-0.1.185.iso
OVMF_DIR=./FV
```

Create an empty image file, `raw` is supported.
```shell
qemu-img create -f raw $IMG_FILE 30G
```

Begin the Windows installation process under QEMU
```shell
qemu-system-x86_64 \
	-machine q35,accel=kvm \
	-cpu host \
	-m 4G \
	-bios ./$OVMF_DIR/OVMF_CODE.fd \
	-cdrom ./$WIN_ISO_FILE \
	-drive file=./$VIRTIO_ISO_FILE,index=0,media=cdrom
	-drive if=none,id=root,file=./$IMG_FILE \
	-device virtio-blk-pci,drive=root,disable-legacy=on \
	-device virtio-net-pci,netdev=mynet0,disable-legacy=on \
	-netdev user,id=mynet0 \
	-vga std
```

Before the installation can proceed, point the Windows installation program to the VirtIO disk and install the necessary storage controller drivers. After that, the attached hard drive will become visible and the actual installation can commence.

After the installation has completed, proceed further to the configuration section. QEMU will be needed at least once more to enable the Windows Special Administration Console (SAC) and to possibly install extra device drivers.

## Image Usage

The basic command to boot a Windows image. The configuration section should be checked before executing it for the first time.

```shell
cloud-hypervisor \
	--kernel ./$OVMF_DIR/CLOUDHV.fd \
	--disk path=./$IMG_FILE \
	--cpus boot=1,kvm_hyperv=on \
	--memory size=4G \
	--serial tty \
	--console off \
	--net tap=
```

It is necessary to always:

- Carry the OVMF firmware in the `--kernel` option
- Add `kvm_hyperv=on` to the `--cpus` option

In cases where the host processor supports address space > 39 bits, it might be necessary to limit the address space. It can be done by appending the option `max_phys_bits=X` to the `--cpus` parameter, where `X` is the number of bits to be supported. Windows was tested to support at least 39-bit address space.

To daemonize the Cloud Hypervisor process, `nohup` can be used. Some STDIO redirections might need to be done. In a simple case it is sufficient to just redirect all the output to `/dev/null`.

## Image Configuration

### Device Drivers

After the Windows installation has finished under QEMU, there might be still devices with no drivers installed. This might happen for example, when a device was not used during the installation. In particular it is important to ensure that the VirtIO network device is setup correctly because further steps for the configuration and the usage require network in most case.

Boot once more under QEMU and use the [Device Manager](https://support.microsoft.com/en-in/help/4028443/windows-10-update-drivers), to ensure all the device drivers, and especially the network card, are installed correctly. Also, as Cloud Hypervisor can introduce new devices, it is advisable to repeat the procedure while booted under Cloud Hypervisor, when the RDP access to the image is functional.

### Windows Special Administration Console (SAC) enablement

SAC provides a text based console access to the Windows guest. As Cloud Hypervisor doesn't implement a VGA adaptor, SAC is an important instrument for the Windows guest management.

Boot the Windows image under QEMU and execute the below commands to permanently enable SAC

```cmd
bcdedit /emssettings emsport:1 emsbaudrate:115200
bcdedit /ems on
bcdedit /bootems on
```

Once SAC is enabled, the image can be booted under Cloud Hypervisor. The SAC prompt will show up 

<pre>
Computer is booting, SAC started and initialized.                               
                                                                                
Use the "ch -?" command for information about using channels.                   
Use the "?" command for general help.                                           
                                                                                
                                                                                
SAC>
</pre>

To open a console on the guest, the command sequence below can be used
<pre>
SAC>cmd
The Command Prompt session was successfully launched.
SAC>
EVENT:   A new channel has been created.  Use "ch -?" for channel help.
Channel: Cmd0001
SAC>ch -si 1
</pre>

See also the [links](#Links) section for a more extended SAC documentation.

## Network

This section illustrates the Windows specific aspects of the VM network configuration.

### Basic Networking

As the simplest option, using `--net tap=` in the Cloud Hypervisor command line will create a `vmtapX` device on the host with the default IPv4 address `192.168.249.1`. After SAC becomes available, the guest configuration can be set with

<pre>
SAC>i 10 192.168.249.2 255.255.255.0 192.168.249.1
</pre> 

Where `10` is the device index as shown by the `i` command.

### Guest Internet Connectivity

Additional steps are necessary to provide the guest with internet access.

- On the guest, add the DNS server either by using `netsh` or by opening `Network and Connectivity Center` and editing the adapter properties.
- On the host, configure the traffic forwarding. Replace the `NET_DEV` with the name of your network device.
```shell
NET_DEV=wlp3s0
sysctl -w net.ipv4.ip_forward=1
iptables -t nat -A POSTROUTING -o $NET_DEV -j MASQUERADE
```

### Remote Desktop Protocol (RDP) enablement

#### Using QEMU
  - Execute `SystemPropertiesRemote`
  - In the properties window, choose "Allow remote connections to this computer"
  - Click "Select Users" and add some user to the allow list
#### Using powershell
```powershell
Set-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Terminal Server\" -Name "fDenyTSConnections" -Value 0
Enable-NetFirewallRule -DisplayGroup "Remote Desktop"
Add-LocalGroupMember -Group "Remote Desktop Users" -Member someuser
```
 
Administrators can always RDP, non administrator users have to be explicitly enabled. 

Once the configuration is set, RDP clients can connect to `192.168.249.2`.

### SSH

#### Enable using powershell

```powershell
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Start-Service sshd
Set-Service -Name sshd -StartupType ‘Automatic’
```

This allows for SSH login from a remote machine, for example through the `administrator` user: `ssh administrator@192.168.249.2`. For a more detailed OpenSSH guide, please follow the MSDN article from the [links](#links) section.

## Hotplug capability

CPU hotplug is supported. The VM operating system needs to support hotplug and be appropriately licensed. SKU limitations like constraints on the number of cores are to be taken into consideration. Note, that Windows doesn't support CPU hot-remove. When `ch-remote` is invoked to reduce the number of CPUs, the result will be visible after the OS reboot within the same hypervisor instance.

RAM hotplug is supported. Note, that while the `pnpmem.sys` driver in use supports RAM hot-remove, the RAM being unplugged has to be not in use and have no reserved pages. In most cases it means, hot-remove won't work. Same as with the CPU hot-remove, when `ch-remote` is invoked to reduce the RAM size, the result will be visible after the OS reboot.

Network device hotplug and hot-remove are supported.

Disk hotplug and hot-remove are supported. After the device has been hotplugged, it will need to be onlined from within the guest. Among other tools, powershell applets `Get-Disk` and `Set-Disk` can be used for the disk configuration and activation.

## Debugging

The Windows guest debugging process relies heavily on QEMU and [socat](http://www.dest-unreach.org/socat/). The procedure requires two Windows VMs:

- A debugger VM running under QEMU.
- A debuggee, a Windows VM that has been created in the previous steps, running under Cloud Hypervisor or QEMU.

The connection between both guests happens over TCP, whereby on the guest side it is automatically translated to a COM port. Because the VMs are connected through TCP, the debugging infrastructure can be distributed over the network. The serial port, while slowly transferring data, is common enough to support a wide range of cases and tools.

In this exercise, [WinDbg](https://docs.microsoft.com/en-us/windows-hardware/drivers/debugger/) is used. Any other debugger of choice with the ability to use serial connection can be used instead. 

### Debugger and Debuggee

#### WinDbg VM

For simplicity, the debugger VM is supposed to be only running under QEMU. It will require VGA and doesn't necessarily depend on UEFI. As an OS, it can carry any supported Windows OS where the debugger of choice can be installed. The simplest way is to follow the image preparation instructions from the previous chapter, but avoid using the OVMF firmware. It is also not required to use VirtIO drivers, whereby it might be useful in some case. Though, while creating the image file for the debugger VM, be sure to choose a sufficient disk size that counts in the need to save the corresponding debug symbols and sources.

To create the debugger Windows VM, the following command can be used:

```shell
qemu-system-x86_64 \
	-machine q35,accel=kvm \
	-cpu host \
	-smp 1 \
	-m 4G \
	-cdrom ./$WIN_ISO_FILE \
	-drive file=./$VIRTIO_ISO_FILE,index=0,media=cdrom
	-drive if=none,id=root,file=./windbg-disk.raw \
	-device virtio-blk-pci,drive=root,disable-legacy=on \
	-device virtio-net-pci,netdev=mynet0,disable-legacy=on \
	-netdev user,id=mynet0,net=192.168.178.0/24,host=192.168.178.1,dhcpstart=192.168.178.64,hostname=windbg-host \
	-vga std
```

A non server Windows OS like Windows 10 can be used to carry the debugging tools in the debugger VM.

#### Debuggee VM

The debuggee VM is the one that we've learned to configure and run in the first section. There might be various reasons to debug. For example, there could be an issue in the Windows guest with an emulated device or an included driver. Or, we might want to develop a custom feature like a kernel driver to be available in the guest.

Note, that there are several ways to debug Windows, not all of them need to be enabled at the same time. For example, if developing a kernel module, the only useful options would be to configure for the serial debugging and enable the kernel debug. In that case, any crash or misbehavior in the boot loader or kernel would be ignored. The commands below must be run as administrator on the debuggee guest VM.

##### Turn On Serial Debugging

This will configure the debugging to be enabled and instruct to use the serial port for it.

```cmd
bcdedit /dbgsettings serial debugport:1 baudrate:115200
```

##### Turn On Kernel Debugging

```cmd
bcdedit /debug on
```

##### Turn On Boot Loader Debug

```cmd
bcdedit /bootdebug on
```

##### Turn on boot manager debug 

```cmd
bcdedit /set {bootmgr} bootdebug on
```

##### Disable Recovery Screen On Boot Failure

There could be a situation, where a crash is debugged. In such cases, the guest could be left in an inconsistent state. The default Windows behavior would be to boot into the recovery screen, however in some cases it might be not desired. To make Windows ignore failures and always proceed to booting the OS, use the command below:

```cmd
bcdedit /set {default} bootstatuspolicy ignoreallfailures
```

### Debugging Process

#### Invoke the WinDbg VM

```shell
qemu-system-x86_64 \
	-machine q35,accel=kvm \
	-cpu host \
	-smp 1 \
	-m 4G \
	-drive if=none,id=root,file=./windbg-disk.raw \
	-device virtio-blk-pci,drive=root,disable-legacy=on \
	-serial tcp::4445,server,nowait \
	-device virtio-net-pci,netdev=mynet0,disable-legacy=on \
	-netdev user,id=mynet0,net=192.168.178.0/24,host=192.168.178.1,dhcpstart=192.168.178.64,hostname=windbg-host \
	-vga std
```

Note, this VM has the networking enabled. It is needed, because symbols and sources might need to be fetched from a network location.

Also, notice the `-serial` parameter - that's what does the magic on exposing the serial port to the guest while connecting the debugger VM with a client VM through the network. SAC/EMS needs to be disabled in the debugger VM, as otherwise the COM device might be blocked.

Hereafter, WinDbg can be started using a command below:

```cmd
set _NT_DEBUG_PORT=com1
set _NT_DEBUG_BAUD_RATE=115200

windbg -v -d -k
```

Once started, WinDbg will wait for an incoming connection which is going to be initialized by the debuggee VM started in the next section.

#### Invoke the Debuggee VM

##### Under QEMU

Essentially it would be the command like depicted in the guest preparation sections, with a few modifications:
```shell
qemu-system-x86_64 \
	-machine q35,accel=kvm \
	-cpu host \
	-m 4G \
	-bios ./$OVMF_DIR/OVMF_CODE.fd \
	-cdrom ./$WIN_ISO_FILE \
	-drive file=./$VIRTIO_ISO_FILE,index=0,media=cdrom
	-drive if=none,id=root,file=./$IMG_FILE \
	-device virtio-blk-pci,drive=root,disable-legacy=on \
	-device virtio-net-pci,netdev=mynet0,disable-legacy=on \
	-netdev user,id=mynet0 \
	-serial tcp:127.0.0.1:4445 \
	-vga std
```

It is to see, that `-serial` parameter is used here, to establish the connection with the debugger VM.

To disable HPET, attach `--no-hpet`. To enable hypervisor reference timer, use `-cpu host,hv-time`. These and other options can be used to achieve better [Hyper-V compatibility](https://archive.fosdem.org/2019/schedule/event/vai_enlightening_kvm/attachments/slides/2860/export/events/attachments/vai_enlightening_kvm/slides/2860/vkuznets_fosdem2019_enlightening_kvm.pdf).

##### Cloud Hypervisor

The `socat` tool is used to establish the QEMU compatible behavior. Here as well, the Cloud Hypervisor command used to run the Windows guest is to be used. Put the command into a shell script:

`socat SYSTEM:"./ch-script",openpty,raw,echo=0 TCP:localhost:4445`

The reason to pack the command into the shell script is that the command might contain a comma. When using SYSTEM, the shell command can't contain `,` or `!!`.

## Links

- [Fedora VirtIO guide for Windows](https://docs.fedoraproject.org/en-US/quick-docs/creating-windows-virtual-machines-using-virtio-drivers/)
- [VirtIO driver binaries](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/)
- [VirtIO driver sources](https://github.com/virtio-win/kvm-guest-drivers-windows)
- [Emergency Management Services](https://docs.microsoft.com/en-us/previous-versions/windows/it-pro/windows-server-2003/cc787940(v=ws.10))
- [OpenSSH server/client configuration](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse)
- [Windows guest debugging under KVM](https://www.linux-kvm.org/page/WindowsGuestDrivers/GuestDebugging)
- ["ENLIGHTENING" KVM](https://archive.fosdem.org/2019/schedule/event/vai_enlightening_kvm/attachments/slides/2860/export/events/attachments/vai_enlightening_kvm/slides/2860/vkuznets_fosdem2019_enlightening_kvm.pdf)
