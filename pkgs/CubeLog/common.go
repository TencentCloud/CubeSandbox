// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"net"
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
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	var fallbackIP string

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					if iface.Name == "eth0" {
						return ipnet.IP.String()
					}
					if fallbackIP == "" {
						fallbackIP = ipnet.IP.String()
					}
				}
			}
		}
	}

	return fallbackIP
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
