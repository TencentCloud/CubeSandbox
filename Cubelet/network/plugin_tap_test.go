// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"testing"
)

func TestGetGwIPAndMask(t *testing.T) {
	tests := []struct {
		name     string
		cidr     string
		wantIP   string
		wantMask int
		wantErr  bool
	}{
		{
			name:     "standard-class-c",
			cidr:     "192.168.1.0/24",
			wantIP:   "192.168.1.1",
			wantMask: 24,
			wantErr:  false,
		},
		{
			name:     "non-canonical-cidr",
			cidr:     "192.168.1.99/24",
			wantIP:   "192.168.1.1",
			wantMask: 24,
			wantErr:  false,
		},
		{
			name:     "small-subnet",
			cidr:     "10.0.0.0/30",
			wantIP:   "10.0.0.1",
			wantMask: 30,
			wantErr:  false,
		},
		{
			name:    "invalid-cidr",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
		{
			name:    "ipv6-unsupported",
			cidr:    "2001:db8::/64",
			wantErr: true,
		},
		{
			name:    "broadcast-slash-32",
			cidr:    "255.255.255.255/32",
			wantErr: true,
		},
		{
			name:    "mask-too-small",
			cidr:    "10.0.0.0/7",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIP, gotMask, err := getGwIPAndMask(tt.cidr)
			if (err != nil) != tt.wantErr {
				t.Errorf("getGwIPAndMask() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if gotIP.String() != tt.wantIP {
				t.Errorf("getGwIPAndMask() gotIP = %v, want %v", gotIP, tt.wantIP)
			}
			if gotMask != tt.wantMask {
				t.Errorf("getGwIPAndMask() gotMask = %v, want %v", gotMask, tt.wantMask)
			}
		})
	}
}
