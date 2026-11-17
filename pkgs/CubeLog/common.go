// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

type Region string

const (
	RegionGuangzhou     Region = "ap-guangzhou"
	RegionShanghai      Region = "ap-shanghai"
	RegionBeijing       Region = "ap-beijing"
	RegionHongKong      Region = "ap-hongkong"
	RegionChengdu       Region = "ap-chengdu"
	RegionIndia         Region = "ap-mumbai"
	RegionChongqing     Region = "ap-chongqing"
	RegionSeoul         Region = "ap-seoul"
	RegionSingapore     Region = "ap-singapore"
	RegionToronto       Region = "na-toronto"
	RegionSiliconValley Region = "na-siliconvalley"
	RegionFrankfurt     Region = "eu-frankfurt"
	RegionBangkok       Region = "ap-bangkok"
)

var defaultRegion Region
var cluster string
var moduleName string
var moduleVersion string
var reportLevel LogLevel

var LocalIP string

func init() {
	reportLevel = WARN
	LocalIP = getLocalIP()
}

func getLocalIP() string {
	ip, err := detectPrimaryIPv4()
	if err != nil {
		return ""
	}
	return ip
}

func detectPrimaryIPv4() (string, error) {
	if ip, err := nodeIPFromEnv(); err == nil {
		return ip, nil
	}

	if file, err := os.Open("/proc/net/route"); err == nil {
		ifaceName, routeErr := defaultRouteInterface(file)
		_ = file.Close()
		if routeErr == nil {
			if ip, addrErr := firstIPv4ForInterface(ifaceName); addrErr == nil {
				return ip, nil
			}
		}
	}

	if ip, err := outboundIPv4(); err == nil {
		return ip, nil
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	if ip := firstNonLoopbackIPv4(addrs); ip != "" {
		return ip, nil
	}

	return "", fmt.Errorf("failed to detect a primary non-loopback IPv4 address")
}

func nodeIPFromEnv() (string, error) {
	value := strings.TrimSpace(os.Getenv("CUBE_SANDBOX_NODE_IP"))
	ip := net.ParseIP(value)
	if value == "" || ip == nil || ip.To4() == nil || ip.IsLoopback() {
		return "", fmt.Errorf("invalid CUBE_SANDBOX_NODE_IP: %q", value)
	}
	return ip.String(), nil
}

func defaultRouteInterface(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	bestInterface := ""
	bestMetric := int64(^uint64(0) >> 1)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}

		flags, err := strconv.ParseInt(fields[3], 16, 64)
		if err != nil || flags&0x1 == 0 {
			continue
		}
		metric, err := strconv.ParseInt(fields[6], 10, 64)
		if err != nil || metric >= bestMetric {
			continue
		}
		bestInterface = fields[0]
		bestMetric = metric
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}
	if bestInterface == "" {
		return "", fmt.Errorf("default route interface not found")
	}
	return bestInterface, nil
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
	if ip := firstNonLoopbackIPv4(addrs); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no IPv4 found for interface %s", name)
}

func outboundIPv4() (string, error) {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
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

func firstNonLoopbackIPv4(addrs []net.Addr) string {
	for _, addr := range addrs {
		var ip net.IP
		switch value := addr.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
			return ip4.String()
		}
	}
	return ""
}

func SetRegion(r Region) {
	defaultRegion = r
}

func SetCluster(c string) {
	cluster = c
}

func SetModuleName(n string) {
	moduleName = n
}

func GetModuleName() string {
	return moduleName
}

func SetVersion(version string) {
	moduleVersion = version
}

func (r Region) String() string {
	return string(r)
}
