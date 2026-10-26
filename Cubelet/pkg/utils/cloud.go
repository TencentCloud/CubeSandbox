// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

const localInstanceType = "cubebox"

var ErrMetadataUnsupported = errors.New("cloud metadata is not available in the opensource cubelet build")

type HostIdentity struct {
	InstanceID   string
	LocalIPv4    string
	InstanceType string
	Region       string
}

var (
	hostIdentityOnce sync.Once
	hostIdentity     HostIdentity
	hostIdentityErr  error
)

func GetHostIdentity() (HostIdentity, error) {
	hostIdentityOnce.Do(func() {
		instanceID, localIPv4, err := resolveHostIdentity()
		if err != nil {
			hostIdentityErr = err
			return
		}
		hostIdentity = HostIdentity{
			InstanceID:   instanceID,
			LocalIPv4:    localIPv4,
			InstanceType: localInstanceType,
			Region:       "",
		}
	})

	if hostIdentityErr != nil {
		return HostIdentity{}, hostIdentityErr
	}
	return hostIdentity, nil
}

func GetInstanceID() (string, error) {
	identity, err := GetHostIdentity()
	if err != nil {
		return "", err
	}
	return identity.InstanceID, nil
}

func GetShortInstanceType() (string, error) {
	return localInstanceType, nil
}

func GetLocalIpv4() (string, error) {
	identity, err := GetHostIdentity()
	if err != nil {
		return "", err
	}
	return identity.LocalIPv4, nil
}

func GetRegion() (string, error) {
	return "", nil
}

func GetVPCIDByMAC(mac string) (string, error) {
	_ = mac
	return "", ErrMetadataUnsupported
}

func GetSubNetID(mac string) (string, error) {
	_ = mac
	return "", ErrMetadataUnsupported
}

// resolveHostIdentity separates stable NodeID (InstanceID) from the dialable
// endpoint address (LocalIPv4 / HostIP registration).
//
// InstanceID:
//  1. CUBE_SANDBOX_NODE_ID (may be nodeName or any stable id)
//  2. else CUBE_SANDBOX_NODE_IP (IPv4)
//  3. else auto-detected primary IPv4
//
// LocalIPv4:
//  1. CUBE_SANDBOX_ENDPOINT_IP (PodIP for Addresses / Redis HostIP)
//  2. else if InstanceID is an IPv4, use it (legacy single-identity behaviour)
//  3. else auto-detected primary IPv4
func resolveHostIdentity() (instanceID, localIPv4 string, err error) {
	if id := strings.TrimSpace(os.Getenv("CUBE_SANDBOX_NODE_ID")); id != "" {
		instanceID = id
	} else if ip, e := nodeIPFromEnv(); e == nil {
		instanceID = ip
	} else {
		ip, e := detectPrimaryIPv4()
		if e != nil {
			return "", "", e
		}
		instanceID = ip
	}

	if ep := strings.TrimSpace(os.Getenv("CUBE_SANDBOX_ENDPOINT_IP")); ep != "" {
		ip := net.ParseIP(ep)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() {
			return "", "", fmt.Errorf("invalid CUBE_SANDBOX_ENDPOINT_IP: %q", ep)
		}
		localIPv4 = ip.String()
		return instanceID, localIPv4, nil
	}

	if ip := net.ParseIP(instanceID); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
		return instanceID, ip.String(), nil
	}

	ip, e := detectPrimaryIPv4()
	if e != nil {
		return "", "", e
	}
	return instanceID, ip, nil
}

func detectPrimaryIPv4() (string, error) {
	if ip, err := nodeIPFromEnv(); err == nil {
		return ip, nil
	}

	if ifaceName, err := defaultRouteInterface(); err == nil && ifaceName != "" {
		if ip, err := firstIPv4ForInterface(ifaceName); err == nil {
			return ip, nil
		}
	}

	if ip, err := outboundIPv4(); err == nil {
		return ip, nil
	}

	if ip, err := firstNonLoopbackIPv4(); err == nil {
		return ip, nil
	}

	return "", fmt.Errorf("failed to detect a primary non-loopback IPv4 address")
}

func nodeIPFromEnv() (string, error) {
	value := strings.TrimSpace(os.Getenv("CUBE_SANDBOX_NODE_IP"))
	if value == "" {
		return "", fmt.Errorf("CUBE_SANDBOX_NODE_IP is empty")
	}
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() {
		return "", fmt.Errorf("invalid CUBE_SANDBOX_NODE_IP: %q", value)
	}
	return ip.String(), nil
}

func defaultRouteInterface() (string, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}

		flags, err := strconv.ParseInt(fields[3], 16, 64)
		if err != nil {
			continue
		}
		if flags&0x1 == 0 {
			continue
		}
		return fields[0], nil
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("default route interface not found")
}

func outboundIPv4() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr.IP == nil {
		return "", fmt.Errorf("failed to determine outbound IPv4 from local address")
	}

	ip := localAddr.IP.To4()
	if ip == nil || ip.IsLoopback() {
		return "", fmt.Errorf("outbound IPv4 is unavailable")
	}
	return ip.String(), nil
}

func firstNonLoopbackIPv4() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ip, err := firstIPv4ForInterface(iface.Name)
		if err == nil {
			return ip, nil
		}
	}

	return "", fmt.Errorf("no non-loopback IPv4 found")
}

func firstIPv4ForInterface(name string) (string, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		ip := ipFromAddr(addr)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		return ip.String(), nil
	}

	return "", fmt.Errorf("no IPv4 found for interface %s", name)
}

func ipFromAddr(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP.To4()
	case *net.IPAddr:
		return v.IP.To4()
	default:
		return nil
	}
}
