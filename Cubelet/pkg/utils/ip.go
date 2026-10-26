// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"math/big"
	"net"
	"strings"
)

func InetNtoAv4(ip uint) string {
	return fmt.Sprintf("%d.%d.%d.%d",
		byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
}

func InetAtoNv4(ip string) (uint, error) {
	ret := big.NewInt(0)
	ipBytes := net.ParseIP(ip).To4()
	if ipBytes == nil {
		return 0, fmt.Errorf("invalid ip")
	}
	ret.SetBytes(ipBytes)
	return uint(ret.Int64()), nil
}

func GenLocalMAC(ipStr string) string {
	hw := make(net.HardwareAddr, 6)

	hw[0] = 0x02

	hw[1] = 0x42

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}

	copy(hw[2:], ip.To4())
	return hw.String()
}

func ReadIPFromStrInterface(i interface{}) net.IP {
	if i == nil {
		return nil
	}
	ipStr, ok := i.(string)
	if !ok {
		return nil
	}
	return net.ParseIP(ipStr)
}

func GenGatewayIP(ip string, subnetMask string) net.IP {
	ipParts := strings.Split(ip, ".")
	subnetMaskParts := strings.Split(subnetMask, ".")

	var gatewayIP []string
	for i := 0; i < 4; i++ {
		ipPart := ipParts[i]
		subnetMaskPart := subnetMaskParts[i]

		ipInt := byteToInt(ipPart)
		subnetMaskInt := byteToInt(subnetMaskPart)

		gatewayPart := ipInt & subnetMaskInt
		gatewayIP = append(gatewayIP, fmt.Sprint(gatewayPart))
	}

	gatewayIP[3] = "1"

	return net.ParseIP(strings.Join(gatewayIP, "."))
}

func byteToInt(byteStr string) int {
	var result int
	fmt.Sscanf(byteStr, "%d", &result)
	return result
}
