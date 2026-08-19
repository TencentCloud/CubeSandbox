// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/systemnet"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Dump-style netlink reads retry on ErrDumpInterrupted (see systemnet.WithDumpRetry).
// Hot-path create prefers openTapFdByName to avoid these reads entirely; cleaner
// and destroy/restore paths still need them under concurrent TAP churn.
var netlinkLinkByIndex = func(index int) (netlink.Link, error) {
	return systemnet.WithDumpRetry(func() (netlink.Link, error) {
		return netlink.LinkByIndex(index)
	})
}
var netlinkLinkByName = func(name string) (netlink.Link, error) {
	return systemnet.WithDumpRetry(func() (netlink.Link, error) {
		return netlink.LinkByName(name)
	})
}
var netlinkLinkList = func() ([]netlink.Link, error) {
	return systemnet.WithDumpRetry(netlink.LinkList)
}
var netlinkLinkDel = netlink.LinkDel
var deleteTAPDevicePolicyMaps = cubevs.DeleteTAPDevicePolicyMaps
var unixOpen = unix.Open
var unixClose = unix.Close
var unixIoctlIfreq = unix.IoctlIfreq
var unixIoctlSetInt = unix.IoctlSetInt
var unixIoctlSetPointerInt = unix.IoctlSetPointerInt
var unixIoctlSetTunOffload = func(fd int, features uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETOFFLOAD), features)
	if errno != 0 {
		return errno
	}
	return nil
}
var enableTapTXTCPMangleIDSegmentation = enableTXTCPMangleIDSegmentation

const (
	// TAP names are derived from sandbox IPs so recovery can match live devices
	// back to allocator addresses without a separate registry.
	tapNamePrefix    = "z"
	virtioNetHdrSize = 12
	tunDevicePath    = "/dev/net/tun"
)

// tapDevice is the runtime's live view of one host TAP. File is optional: a TAP
// can be known from netlink/recovery while its fd is held by another process or
// closed while idle in the pool.
type tapDevice struct {
	Index        int
	Name         string
	IP           net.IP
	InUse        bool
	File         *os.File
	PortMappings []PortMapping
	FailureCount int
	LastError    string
	LastStage    string
}

// TapDeviceAdapter isolates privileged netlink/TUN operations from controller
// logic so tests can exercise state transitions without creating real devices.
type TapDeviceAdapter interface {
	Create(ip net.IP, mvmMacAddr string, mtu, cubeDevIdx int) (*tapDevice, error)
	Restore(tap *tapDevice, mtu int, mvmMacAddr string, cubeDevIdx int) (*tapDevice, error)
	Open(name string) (*os.File, error)
	Close(file *os.File)
	List() (map[string]*tapDevice, error)
	GetByName(name string) (*tapDevice, error)
	Destroy(ifIdx int) error
}

// realTapDeviceAdapter forwards calls to the Linux TAP implementation below.
type realTapDeviceAdapter struct{}

// newRealTapDeviceAdapter returns the production TAP adapter.
func newRealTapDeviceAdapter() TapDeviceAdapter {
	return realTapDeviceAdapter{}
}

func (realTapDeviceAdapter) Create(ip net.IP, mvmMacAddr string, mtu, cubeDevIdx int) (*tapDevice, error) {
	return newTap(ip, mvmMacAddr, mtu, cubeDevIdx)
}

func (realTapDeviceAdapter) Restore(tap *tapDevice, mtu int, mvmMacAddr string, cubeDevIdx int) (*tapDevice, error) {
	return restoreTap(tap, mtu, mvmMacAddr, cubeDevIdx)
}

func (realTapDeviceAdapter) Open(name string) (*os.File, error) {
	return openTapFdByName(name)
}

func (realTapDeviceAdapter) Close(file *os.File) {
	closeTapFile(file)
}

func (realTapDeviceAdapter) List() (map[string]*tapDevice, error) {
	return listCubeTaps()
}

func (realTapDeviceAdapter) GetByName(name string) (*tapDevice, error) {
	return getTapByName(name)
}

func (realTapDeviceAdapter) Destroy(ifIdx int) error {
	return destroyTap(ifIdx)
}

// newTap creates a persistent one-queue TAP, configures virtio-net header size,
// attaches CubeVS filters, sets MTU, and installs the ARP entry needed by the
// sandbox gateway path.
func newTap(ip net.IP, mvmMacAddr string, mtu, cubeDevIdx int) (_ *tapDevice, retErr error) {
	logger := CubeLog.WithContext(context.Background())
	name := tapName(ip.String())
	tapConfig := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name:  name,
			Flags: net.FlagUp,
		},
		Mode:   netlink.TUNTAP_MODE_TAP,
		Flags:  unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR | unix.IFF_ONE_QUEUE,
		Queues: 1,
	}
	logger.Infof("network runtime newTap begin: name=%s ip=%s mtu=%d cube_dev_idx=%d flags=0x%x queues=%d",
		name, ip.String(), mtu, cubeDevIdx, tapConfig.Flags, tapConfig.Queues)
	if err := netlink.LinkAdd(tapConfig); err != nil {
		logger.Warnf("network runtime newTap link add failed: name=%s err=%v", name, err)
		return nil, err
	}
	defer func() {
		if retErr != nil {
			logger.Warnf("network runtime newTap cleanup after failure: name=%s ifindex=%d err=%v", name, tapConfig.Index, retErr)
			for _, file := range tapConfig.Fds {
				closeTapFile(file)
			}
			_ = destroyTap(tapConfig.Index)
		}
	}()
	tap := &tapDevice{
		IP:    ip,
		Name:  name,
		Index: tapConfig.Index,
		InUse: true,
	}
	if len(tapConfig.Fds) == 0 {
		logger.Warnf("network runtime newTap missing fd: name=%s ifindex=%d", tap.Name, tap.Index)
		return nil, fmt.Errorf("tap(%s) fd is empty", tap.Name)
	}
	tap.File = tapConfig.Fds[0]
	logger.Infof("network runtime newTap link add done: name=%s ifindex=%d fd=%d", tap.Name, tap.Index, tap.File.Fd())
	if err := configureTapFD(int(tap.File.Fd()), tap.Name); err != nil {
		logger.Warnf("network runtime newTap configure fd failed: name=%s fd=%d err=%v", tap.Name, tap.File.Fd(), err)
		return nil, err
	}
	logger.Infof("network runtime newTap configure fd done: name=%s fd=%d", tap.Name, tap.File.Fd())
	if err := netlink.LinkSetUp(tapConfig); err != nil {
		logger.Warnf("network runtime newTap link set up failed: name=%s ifindex=%d err=%v", tap.Name, tap.Index, err)
		return nil, err
	}
	logger.Infof("network runtime newTap link set up done: name=%s ifindex=%d", tap.Name, tap.Index)
	if err := cubevs.AttachFilter(uint32(tap.Index)); err != nil {
		logger.Warnf("network runtime newTap attach filter failed: name=%s ifindex=%d err=%v", tap.Name, tap.Index, err)
		return nil, err
	}
	logger.Infof("network runtime newTap attach filter done: name=%s ifindex=%d", tap.Name, tap.Index)
	if err := netlink.LinkSetMTU(tapConfig, mtu); err != nil {
		logger.Warnf("network runtime newTap set mtu failed: name=%s ifindex=%d mtu=%d err=%v", tap.Name, tap.Index, mtu, err)
		return nil, err
	}
	logger.Infof("network runtime newTap set mtu done: name=%s ifindex=%d mtu=%d", tap.Name, tap.Index, mtu)
	if err := systemnet.AddARPEntry(ip, mvmMacAddr, cubeDevIdx); err != nil && err != syscall.EEXIST {
		logger.Warnf("network runtime newTap add arp failed: name=%s ifindex=%d ip=%s mac=%s cube_dev_idx=%d err=%v",
			tap.Name, tap.Index, ip.String(), mvmMacAddr, cubeDevIdx, err)
		return nil, err
	}
	logger.Infof("network runtime newTap ready: name=%s ifindex=%d ip=%s fd=%d arp_mac=%s",
		tap.Name, tap.Index, ip.String(), tap.File.Fd(), mvmMacAddr)
	return tap, nil
}

// configureTapFD applies the complete per-fd TAP contract. The ethtool feature
// is device-wide and optional, but keeping it here ensures fresh and lazily
// reopened descriptors follow the same setup path.
func configureTapFD(fd int, name string) error {
	if err := unixIoctlSetPointerInt(fd, unix.TUNSETVNETHDRSZ, virtioNetHdrSize); err != nil {
		return fmt.Errorf("set tap(%s) vnet hdr failed: %w", name, err)
	}
	offload := uintptr(unix.TUN_F_CSUM | unix.TUN_F_TSO4 | unix.TUN_F_TSO6)
	if err := unixIoctlSetTunOffload(fd, offload); err != nil {
		return fmt.Errorf("set tap(%s) TUNSETOFFLOAD failed: %w", name, err)
	}
	// tx-tcp-mangleid-segmentation is optional; the helper logs unsupported
	// kernels/drivers and intentionally does not fail TAP creation or reopen.
	enableTapTXTCPMangleIDSegmentation(name)
	return nil
}

// openTapFdByName opens a fresh fd for an already-existing, already-configured
// tap device identified by name, WITHOUT any netlink/rtnl lookup. It is the hot
// path used when the caller already knows the device exists and is fully set up
// (e.g. a pooled tap whose fd was closed while idle). Compared to restoreTap it
// avoids netlinkLinkByName (an rtnl read), LinkSetUp/SetMTU, the TC AttachFilter
// and the ARP entry, all of which were already applied when the tap was created.
// For recovering taps of unknown state (e.g. after a restart) use restoreTap.
func openTapFdByName(name string) (*os.File, error) {
	// Use unix.Ifreq (a properly-sized struct ifreq) rather than the local
	// 18-byte ifReq + raw unsafe.Pointer syscall: TUNSETIFF copies the full
	// sizeof(struct ifreq) (~40 bytes) from userspace, so a short struct makes
	// the kernel read past it. unix.NewIfreq also validates the name length.
	// This mirrors deletePersistentTapByName below.
	req, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	req.SetUint16(uint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR | unix.IFF_ONE_QUEUE))

	fd, err := unixOpen(tunDevicePath, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	if err := unixIoctlIfreq(fd, unix.TUNSETIFF, req); err != nil {
		unixClose(fd)
		return nil, fmt.Errorf("set tap(%s) TUNSETIFF failed: %w", name, err)
	}

	if err := configureTapFD(fd, name); err != nil {
		unixClose(fd)
		return nil, err
	}

	return os.NewFile(uintptr(fd), tunDevicePath), nil
}

// restoreTap reconciles a live TAP discovered after restart. Recovery never
// opens a TAP fd: fd ownership is acquired lazily by GetTapFile or by the
// explicit reusable probe after cleanup.
func restoreTap(tap *tapDevice, mtu int, mvmMacAddr string, cubeDevIdx int) (*tapDevice, error) {
	if tap == nil {
		return nil, fmt.Errorf("tap is nil")
	}
	if tap.IP == nil {
		return nil, fmt.Errorf("tap %q missing ip", tap.Name)
	}
	name := tap.Name
	if name == "" {
		name = tapName(tap.IP.String())
	}

	link, err := netlinkLinkByName(name)
	if err != nil {
		return nil, err
	}
	sysTap, ok := link.(*netlink.Tuntap)
	if !ok {
		return nil, fmt.Errorf("%s is not tap", name)
	}

	restored := &tapDevice{
		Name:         name,
		Index:        sysTap.Index,
		IP:           tap.IP.To4(),
		InUse:        link.Attrs().RawFlags&unix.IFF_LOWER_UP > 0,
		File:         tap.File,
		PortMappings: append([]PortMapping(nil), tap.PortMappings...),
	}

	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, err
		}
	}
	if sysTap.MTU != mtu {
		if err := netlink.LinkSetMTU(sysTap, mtu); err != nil {
			return nil, err
		}
	}
	if err := cubevs.AttachFilter(uint32(restored.Index)); err != nil {
		return nil, err
	}
	if err := systemnet.AddARPEntry(restored.IP, mvmMacAddr, cubeDevIdx); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, err
	}
	return restored, nil
}

// listCubeTaps returns live TAP devices whose names follow the runtime's
// IP-derived naming convention, keyed by sandbox IP string.
func listCubeTaps() (map[string]*tapDevice, error) {
	links, err := netlinkLinkList()
	if err != nil {
		return nil, err
	}
	ipToTap := make(map[string]*tapDevice)
	for _, link := range links {
		tap, ok := link.(*netlink.Tuntap)
		if !ok || tap.Mode != netlink.TUNTAP_MODE_TAP {
			continue
		}
		ipStr, err := extractIP(tap.Name)
		if err != nil {
			continue
		}
		ip := net.ParseIP(ipStr).To4()
		if ip == nil {
			continue
		}
		ipToTap[ip.String()] = &tapDevice{
			Name:  tap.Name,
			Index: tap.Index,
			IP:    ip,
			InUse: link.Attrs().RawFlags&unix.IFF_LOWER_UP > 0,
		}
	}
	return ipToTap, nil
}

// isTapNotFound reports whether err means the named host link is absent.
func isTapNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound)
}

// getTapByName returns identity for one runtime-managed TAP by name.
func getTapByName(name string) (*tapDevice, error) {
	link, err := netlinkLinkByName(name)
	if err != nil {
		return nil, err
	}
	tap, ok := link.(*netlink.Tuntap)
	if !ok {
		return nil, fmt.Errorf("%s is not tap", name)
	}
	ipStr, err := extractIP(tap.Name)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return nil, fmt.Errorf("invalid tap ip for %s", name)
	}
	return &tapDevice{
		Name:  tap.Name,
		Index: tap.Index,
		IP:    ip,
		InUse: link.Attrs().RawFlags&unix.IFF_LOWER_UP > 0,
	}, nil
}

// destroyTap removes a TAP by ifindex. It first tries to clear TUNSETPERSIST via
// /dev/net/tun because persistent TAPs may survive netlink deletion alone.
// After the netdev is confirmed gone it best-effort deletes HashOfMaps outer
// policy keys so destroyed ifindexes cannot accumulate stale allow_out_v2 /
// deny_out / dns_allow inners. Policy cleanup never changes the destroy result:
// a missing BPF map must not leave pool/cleanup stuck on a dead netdev.
func destroyTap(ifIdx int) error {
	link, err := netlinkLinkByIndex(ifIdx)
	if err != nil {
		if isTapNotFound(err) {
			// Confirmed absent — drop orphaned outer keys. Transient lookup
			// errors must not wipe policy for a TAP that may still be up.
			cleanupDestroyedTapPolicyMaps(ifIdx)
		}
		return err
	}
	destroyed := false
	if tap, ok := link.(*netlink.Tuntap); ok {
		if err := deletePersistentTapByName(tap.Name); err == nil {
			destroyed = true
		}
	}
	if !destroyed {
		if err := netlinkLinkDel(link); err != nil {
			return err
		}
	}
	cleanupDestroyedTapPolicyMaps(ifIdx)
	return nil
}

// cleanupDestroyedTapPolicyMaps deletes HashOfMaps outer keys for ifIdx only
// when that ifindex no longer has a host netdev. If the index was reused by a
// new device between LinkDel and this call, cleanup is skipped.
func cleanupDestroyedTapPolicyMaps(ifIdx int) {
	_, err := netlinkLinkByIndex(ifIdx)
	switch {
	case err == nil:
		// ifindex reused (or delete raced); do not touch the new device's policy.
		return
	case !isTapNotFound(err):
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime tap policy map cleanup skipped: ifindex=%d lookup_err=%v",
			ifIdx, err,
		)
		return
	}
	if cleanErr := deleteTAPDevicePolicyMaps(uint32(ifIdx)); cleanErr != nil {
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime tap policy map cleanup failed: ifindex=%d err=%v",
			ifIdx, cleanErr,
		)
	}
}

// deletePersistentTapByName opens the TAP and clears TUNSETPERSIST, making the
// kernel delete it once the fd is closed.
func deletePersistentTapByName(name string) error {
	req, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	req.SetUint16(uint16(netlink.TUNTAP_MODE_TAP) | uint16(unix.IFF_TAP) | uint16(unix.IFF_NO_PI) | uint16(unix.IFF_VNET_HDR) | uint16(unix.IFF_ONE_QUEUE))
	fd, err := unixOpen(tunDevicePath, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unixClose(fd)
	if err := unixIoctlIfreq(fd, unix.TUNSETIFF, req); err != nil {
		return err
	}
	if err := unixIoctlSetInt(fd, unix.TUNSETPERSIST, 0); err != nil {
		return err
	}
	return nil
}

// tapName derives the deterministic host TAP name for a sandbox IP.
func tapName(ip string) string {
	return tapNamePrefix + ip
}

// extractIP reverses tapName and rejects unrelated host TAP devices.
func extractIP(name string) (string, error) {
	if len(name) <= len(tapNamePrefix) || name[:len(tapNamePrefix)] != tapNamePrefix {
		return "", fmt.Errorf("not cube tap: %s", name)
	}
	return name[len(tapNamePrefix):], nil
}
