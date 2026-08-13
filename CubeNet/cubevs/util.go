package cubevs

import (
	"bytes"
	"encoding/binary"
	"net"
)

// uint32ToIP converts big endian integer(0x04030201) to net.IP(1.2.3.4).
func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	ip[0] = byte(n)
	ip[1] = byte(n >> 8)
	ip[2] = byte(n >> 16)
	ip[3] = byte(n >> 24)

	return ip
}

// ipToUint32 converts net.IP(1.2.3.4) to big endian integer(0x04030201).
func ipToUint32(ip net.IP) uint32 {
	if len(ip) == net.IPv6len {
		return uint32(ip[12]) | uint32(ip[13])<<8 | uint32(ip[14])<<16 | uint32(ip[15])<<24
	}

	return uint32(ip[0]) | uint32(ip[1])<<8 | uint32(ip[2])<<16 | uint32(ip[3])<<24
}

// ipMaskToUint32 converts an IPv4 mask to the same byte layout as ipToUint32.
func ipMaskToUint32(mask net.IPMask) uint32 {
	return uint32(mask[0]) | uint32(mask[1])<<8 | uint32(mask[2])<<16 | uint32(mask[3])<<24
}

// hardwareAddrToUint32 converts the first 4 bytes of MAC address to a uint32.
func hardwareAddrToUint32(addr net.HardwareAddr) uint32 {
	return uint32(addr[0]) | uint32(addr[1])<<8 | uint32(addr[2])<<16 | uint32(addr[3])<<24
}

// hardwareAddrToUint16 converts the last 2 bytes of MAC address to a uint16.
func hardwareAddrToUint16(addr net.HardwareAddr) uint16 {
	return uint16(addr[4]) | uint16(addr[5])<<8
}

// htons converts a uint16 to its network byte order representation.
func htons(n uint16) uint16 {
	buf := bytes.NewBuffer(nil)
	_ = binary.Write(buf, binary.LittleEndian, n)

	return binary.BigEndian.Uint16(buf.Bytes())
}

// ntohs converts a uint16 to its host byte order representation.
// NOTE: This works because we are on x86_64 which is little endian.
var ntohs = htons

// bytesToString converts a null-terminated byte slice to string.
func bytesToString(arr []byte) string {
	l := len(arr)
	for i := 0; i < l; i++ {
		if arr[i] == 0 {
			l = i

			break
		}
	}

	return string(arr[:l])
}

// stringToByteArray converts a string to a null-terminated byte array.
func stringToByteArray(s string) [64]byte {
	var arr [64]byte
	for i, b := range []byte(s) {
		arr[i] = b
	}

	return arr
}
