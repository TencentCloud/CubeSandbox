package cubevs

import (
	"net"
	"testing"
)

func TestIPMaskToUint32(t *testing.T) {
	tests := []struct {
		name string
		mask net.IPMask
		want uint32
	}{
		{name: "slash zero", mask: net.CIDRMask(0, 32), want: 0x00000000},
		{name: "slash sixteen", mask: net.CIDRMask(16, 32), want: 0x0000ffff},
		{name: "slash twenty four", mask: net.CIDRMask(24, 32), want: 0x00ffffff},
		{name: "slash thirty two", mask: net.CIDRMask(32, 32), want: 0xffffffff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ipMaskToUint32(tt.mask)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("ipMaskToUint32(%v) = %#08x, want %#08x", tt.mask, got, tt.want)
			}
		})
	}
}

func TestIPMaskToUint32RejectsNonIPv4Mask(t *testing.T) {
	if _, err := ipMaskToUint32(net.CIDRMask(64, 128)); err == nil {
		t.Fatal("ipMaskToUint32 accepted an IPv6 mask")
	}
	if _, err := ipMaskToUint32(net.IPMask{255, 0, 255, 0}); err == nil {
		t.Fatal("ipMaskToUint32 accepted a non-contiguous IPv4 mask")
	}
}
