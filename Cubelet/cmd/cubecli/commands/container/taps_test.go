package container

import (
	"testing"

	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
)

func TestNetworkRuntimeTapsURL(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    string
	}{
		{name: "default", want: defaultCubeletHTTPAddress + "/v1/network/taps"},
		{name: "host port without scheme", address: "127.0.0.1:9998", want: "http://127.0.0.1:9998/v1/network/taps"},
		{name: "custom base path overwritten", address: "http://127.0.0.1:9998/old", want: "http://127.0.0.1:9998/v1/network/taps"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := networkRuntimeTapsURL(tt.address)
			if err != nil {
				t.Fatalf("networkRuntimeTapsURL returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("networkRuntimeTapsURL(%q)=%q, want %q", tt.address, got, tt.want)
			}
		})
	}
}

func TestFormatNetworkRuntimePorts(t *testing.T) {
	got := formatNetworkRuntimePorts([]networkruntime.PortMapping{
		{HostPort: 20080, ContainerPort: 8080},
		{Protocol: "udp", HostIP: "0.0.0.0", HostPort: 20053, ContainerPort: 53},
	})
	want := "127.0.0.1:20080->8080/tcp,0.0.0.0:20053->53/udp"
	if got != want {
		t.Fatalf("formatNetworkRuntimePorts()=%q, want %q", got, want)
	}
	if empty := formatNetworkRuntimePorts(nil); empty != "-" {
		t.Fatalf("formatNetworkRuntimePorts(nil)=%q, want -", empty)
	}
}

func TestFormatNetworkRuntimeLastError(t *testing.T) {
	if got := formatNetworkRuntimeLastError(""); got != "-" {
		t.Fatalf("formatNetworkRuntimeLastError(empty)=%q, want -", got)
	}
	if got := formatNetworkRuntimeLastError(" cubeegress verify failed "); got != "cubeegress verify failed" {
		t.Fatalf("formatNetworkRuntimeLastError()=%q", got)
	}
}
