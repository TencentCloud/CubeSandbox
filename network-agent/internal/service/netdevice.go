// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var netlinkRouteReplace = netlink.RouteReplace
var netlinkRouteListFiltered = netlink.RouteListFiltered
var netlinkLinkByIndex = netlink.LinkByIndex
var netlinkLinkByName = netlink.LinkByName
var netlinkLinkList = netlink.LinkList
var netlinkLinkDel = netlink.LinkDel
var unixOpen = unix.Open
var unixClose = unix.Close
var unixIoctlIfreq = unix.IoctlIfreq
var unixIoctlSetInt = unix.IoctlSetInt

const (
	tapNamePrefix    = "z"
	cubeDevName      = "cube-dev"
	virtioNetHdrSize = 12
	txQLen           = 1000
	tunDevicePath    = "/dev/net/tun"
)

type machineDevice struct {
	Index      int
	Name       string
	IP         net.IP
	Mac        net.HardwareAddr
	GatewayMac net.HardwareAddr
}

type cubeDev struct {
	Index int
	Name  string
	IP    net.IP
	Mac   net.HardwareAddr
}

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

func disableGRO(ifName string) error {
	const (
		SIOCETHTOOL  = 0x8946
		ETHTOOL_SGRO = 0x0000002c
	)

	type ethtoolValue struct {
		Cmd  uint32
		Data uint32
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("open socket for ethtool: %w", err)
	}
	defer unix.Close(fd)

	value := ethtoolValue{Cmd: ETHTOOL_SGRO, Data: 0}

	var ifr [40]byte
	copy(ifr[:], ifName)
	*(*uintptr)(unsafe.Pointer(&ifr[16])) = uintptr(unsafe.Pointer(&value))

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr[0])))
	if errno != 0 {
		return fmt.Errorf("disable GRO on %s: %w", ifName, errno)
	}
	return nil
}

func getGatewayMacAddr(ifName string) (string, error) {
	link, err := netlinkLinkByName(ifName)
	if err != nil {
		return "", err
	}
	neighs, err := netlink.NeighList(link.Attrs().Index, 0)
	if err != nil {
		return "", err
	}
	for _, neigh := range neighs {
		if neigh.Family == netlink.FAMILY_V4 && neigh.State == unix.NUD_REACHABLE {
			return neigh.HardwareAddr.String(), nil
		}
	}
	return "", fmt.Errorf("reachable gateway mac not found on %s", ifName)
}

func getMachineDevice(ifName string) (*machineDevice, error) {
	link, err := netlinkLinkByName(ifName)
	if err != nil {
		return nil, err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	if len(addrs) != 1 {
		return nil, fmt.Errorf("ipv4 address on %s is not unique", ifName)
	}
	gwMac, err := getGatewayMacAddr(ifName)
	if err != nil {
		return nil, err
	}
	gatewayMac, err := net.ParseMAC(gwMac)
	if err != nil {
		return nil, err
	}
	return &machineDevice{
		Index:      link.Attrs().Index,
		Name:       link.Attrs().Name,
		IP:         addrs[0].IP,
		Mac:        link.Attrs().HardwareAddr,
		GatewayMac: gatewayMac,
	}, nil
}

func getOrCreateCubeDev(ip net.IP, mask, mtu int, macAddr string) (*cubeDev, error) {
	desiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(mask, 32),
		},
	}
	link, err := netlinkLinkByName(cubeDevName)
	if err == nil {
		dummy, ok := link.(*netlink.Dummy)
		if !ok {
			return nil, fmt.Errorf("%s is not dummy", cubeDevName)
		}
		addrs, err := netlink.AddrList(dummy, netlink.FAMILY_V4)
		if err != nil {
			return nil, err
		}
		hasDesiredAddr := false
		for _, addr := range addrs {
			if addr.IPNet != nil && addr.IPNet.IP.Equal(ip) {
				ones, _ := addr.IPNet.Mask.Size()
				if ones == mask {
					hasDesiredAddr = true
					continue
				}
			}
			if err := netlink.AddrDel(dummy, &addr); err != nil {
				return nil, err
			}
		}
		if !hasDesiredAddr {
			if err := netlink.AddrAdd(dummy, desiredAddr); err != nil && !errors.Is(err, syscall.EEXIST) {
				return nil, err
			}
		}
		if dummy.Attrs().Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(dummy); err != nil {
				return nil, err
			}
		}
		if dummy.Attrs().MTU != mtu {
			if err := netlink.LinkSetMTU(dummy, mtu); err != nil {
				return nil, err
			}
		}
		return &cubeDev{
			Index: dummy.Index,
			Name:  cubeDevName,
			IP:    ip,
			Mac:   dummy.HardwareAddr,
		}, nil
	}
	gwAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		return nil, err
	}
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name:         cubeDevName,
			HardwareAddr: gwAddr,
			TxQLen:       txQLen,
		},
	}
	if err := netlink.LinkAdd(dummy); err != nil {
		return nil, err
	}
	if err := netlink.AddrAdd(dummy, desiredAddr); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(dummy); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetMTU(dummy, mtu); err != nil {
		return nil, err
	}
	return &cubeDev{
		Index: dummy.Index,
		Name:  cubeDevName,
		IP:    ip,
		Mac:   dummy.HardwareAddr,
	}, nil
}

func addARPEntry(ip net.IP, mac string, cubeDevIndex int) error {
	macAddr, err := net.ParseMAC(mac)
	if err != nil {
		return err
	}
	return netlink.NeighSet(&netlink.Neigh{
		Family:       netlink.FAMILY_V4,
		IP:           ip,
		HardwareAddr: macAddr,
		LinkIndex:    cubeDevIndex,
		State:        unix.NUD_PERMANENT,
		Type:         unix.RTN_UNSPEC,
	})
}

func ensureRouteToCubeDev(cidr string, dev *cubeDev) error {
	if dev == nil || dev.Index == 0 {
		return fmt.Errorf("cube-dev is not initialized")
	}
	_, dst, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse mvm cidr %q: %w", cidr, err)
	}
	filter := &netlink.Route{
		LinkIndex: dev.Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_STATIC,
	}
	routes, err := netlinkRouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("list route for %s via %s: %w", dst.String(), dev.Name, err)
	}
	for _, route := range routes {
		if route.Dst != nil && route.Dst.String() == dst.String() && route.LinkIndex == dev.Index {
			return nil
		}
	}
	return netlinkRouteReplace(filter)
}

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
	logger.Infof("network-agent newTap begin: name=%s ip=%s mtu=%d cube_dev_idx=%d flags=0x%x queues=%d",
		name, ip.String(), mtu, cubeDevIdx, tapConfig.Flags, tapConfig.Queues)
	if err := netlink.LinkAdd(tapConfig); err != nil {
		logger.Warnf("network-agent newTap link add failed: name=%s err=%v", name, err)
		return nil, err
	}
	defer func() {
		if retErr != nil {
			logger.Warnf("network-agent newTap cleanup after failure: name=%s ifindex=%d err=%v", name, tapConfig.Index, retErr)
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
		logger.Warnf("network-agent newTap missing fd: name=%s ifindex=%d", tap.Name, tap.Index)
		return nil, fmt.Errorf("tap(%s) fd is empty", tap.Name)
	}
	tap.File = tapConfig.Fds[0]
	logger.Infof("network-agent newTap link add done: name=%s ifindex=%d fd=%d", tap.Name, tap.Index, tap.File.Fd())
	size := virtioNetHdrSize
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, tap.File.Fd(), uintptr(unix.TUNSETVNETHDRSZ), uintptr(unsafe.Pointer(&size))); errno != 0 {
		logger.Warnf("network-agent newTap set vnet hdr failed: name=%s fd=%d size=%d errno=%v", tap.Name, tap.File.Fd(), size, errno)
		return nil, fmt.Errorf("set tap(%s) vnet hdr failed: %v", tap.Name, errno)
	}
	logger.Infof("network-agent newTap set vnet hdr done: name=%s fd=%d size=%d", tap.Name, tap.File.Fd(), size)
	if err := netlink.LinkSetUp(tapConfig); err != nil {
		logger.Warnf("network-agent newTap link set up failed: name=%s ifindex=%d err=%v", tap.Name, tap.Index, err)
		return nil, err
	}
	logger.Infof("network-agent newTap link set up done: name=%s ifindex=%d", tap.Name, tap.Index)
	if err := cubevs.AttachFilter(uint32(tap.Index)); err != nil {
		logger.Warnf("network-agent newTap attach filter failed: name=%s ifindex=%d err=%v", tap.Name, tap.Index, err)
		return nil, err
	}
	logger.Infof("network-agent newTap attach filter done: name=%s ifindex=%d", tap.Name, tap.Index)
	if err := netlink.LinkSetMTU(tapConfig, mtu); err != nil {
		logger.Warnf("network-agent newTap set mtu failed: name=%s ifindex=%d mtu=%d err=%v", tap.Name, tap.Index, mtu, err)
		return nil, err
	}
	logger.Infof("network-agent newTap set mtu done: name=%s ifindex=%d mtu=%d", tap.Name, tap.Index, mtu)
	if err := addARPEntry(ip, mvmMacAddr, cubeDevIdx); err != nil && err != syscall.EEXIST {
		logger.Warnf("network-agent newTap add arp failed: name=%s ifindex=%d ip=%s mac=%s cube_dev_idx=%d err=%v",
			tap.Name, tap.Index, ip.String(), mvmMacAddr, cubeDevIdx, err)
		return nil, err
	}
	logger.Infof("network-agent newTap ready: name=%s ifindex=%d ip=%s fd=%d arp_mac=%s",
		tap.Name, tap.Index, ip.String(), tap.File.Fd(), mvmMacAddr)
	return tap, nil
}

type ifReq struct {
	Name  [16]byte
	Flags uint16
}

func getTapFd(name string) (*os.File, error) {
	link, err := netlinkLinkByName(name)
	if err != nil {
		return nil, err
	}
	tap, ok := link.(*netlink.Tuntap)
	if !ok {
		return nil, fmt.Errorf("%s is not tap", name)
	}

	fd, err := unixOpen(tunDevicePath, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	var req ifReq
	copy(req.Name[:15], tap.Name)
	req.Flags = unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR | unix.IFF_ONE_QUEUE

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		unixClose(fd)
		return nil, fmt.Errorf("set tap(%s) TUNSETIFF failed, errno: %+v", tap.Name, errno)
	}

	size := virtioNetHdrSize
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETVNETHDRSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		unixClose(fd)
		return nil, fmt.Errorf("set tap(%s) vnet hdr failed, errno: %+v", tap.Name, errno)
	}

	return os.NewFile(uintptr(fd), tunDevicePath), nil
}

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

	if restored.File == nil {
		restored.File, err = getTapFd(name)
		if err != nil {
			return nil, err
		}
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
	if err := addARPEntry(restored.IP, mvmMacAddr, cubeDevIdx); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, err
	}
	return restored, nil
}

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

func destroyTap(ifIdx int) error {
	link, err := netlinkLinkByIndex(ifIdx)
	if err != nil {
		return err
	}
	if tap, ok := link.(*netlink.Tuntap); ok {
		if err := deletePersistentTapByName(tap.Name); err == nil {
			return nil
		}
	}
	return netlinkLinkDel(link)
}

func isTapMissingError(err error) bool {
	if err == nil {
		return false
	}
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound)
}

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

func tapName(ip string) string {
	return tapNamePrefix + ip
}

func extractIP(name string) (string, error) {
	if len(name) <= len(tapNamePrefix) || name[:len(tapNamePrefix)] != tapNamePrefix {
		return "", fmt.Errorf("not cube tap: %s", name)
	}
	return name[len(tapNamePrefix):], nil
}
