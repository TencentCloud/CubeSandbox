package runtime

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestConfigureTapFDAppliesVnetHeaderOffloadsAndOptionalFeature(t *testing.T) {
	oldSetVnetHeader := unixIoctlSetPointerInt
	oldSetOffload := unixIoctlSetTunOffload
	oldEnableFeature := enableTapTXTCPMangleIDSegmentation
	t.Cleanup(func() {
		unixIoctlSetPointerInt = oldSetVnetHeader
		unixIoctlSetTunOffload = oldSetOffload
		enableTapTXTCPMangleIDSegmentation = oldEnableFeature
	})

	var gotFD, gotVnetHeaderSize int
	var gotRequest uint
	var gotOffloads uintptr
	var gotFeatureTap string
	unixIoctlSetPointerInt = func(fd int, request uint, value int) error {
		gotFD = fd
		gotRequest = request
		gotVnetHeaderSize = value
		return nil
	}
	unixIoctlSetTunOffload = func(fd int, features uintptr) error {
		if fd != gotFD {
			t.Fatalf("offload fd=%d, want %d", fd, gotFD)
		}
		gotOffloads = features
		return nil
	}
	enableTapTXTCPMangleIDSegmentation = func(name string) {
		gotFeatureTap = name
	}

	if err := configureTapFD(41, "tap41"); err != nil {
		t.Fatal(err)
	}
	if gotFD != 41 || gotRequest != unix.TUNSETVNETHDRSZ || gotVnetHeaderSize != virtioNetHdrSize {
		t.Fatalf("vnet header config fd=%d request=%d size=%d", gotFD, gotRequest, gotVnetHeaderSize)
	}
	wantOffloads := uintptr(unix.TUN_F_CSUM | unix.TUN_F_TSO4 | unix.TUN_F_TSO6)
	if gotOffloads != wantOffloads {
		t.Fatalf("offloads=%#x, want %#x", gotOffloads, wantOffloads)
	}
	if gotFeatureTap != "tap41" {
		t.Fatalf("optional ethtool feature tap=%q, want tap41", gotFeatureTap)
	}
}

func TestConfigureTapFDStopsWhenRequiredOffloadConfigurationFails(t *testing.T) {
	oldSetVnetHeader := unixIoctlSetPointerInt
	oldSetOffload := unixIoctlSetTunOffload
	oldEnableFeature := enableTapTXTCPMangleIDSegmentation
	t.Cleanup(func() {
		unixIoctlSetPointerInt = oldSetVnetHeader
		unixIoctlSetTunOffload = oldSetOffload
		enableTapTXTCPMangleIDSegmentation = oldEnableFeature
	})

	wantErr := errors.New("offload failed")
	unixIoctlSetPointerInt = func(int, uint, int) error { return nil }
	unixIoctlSetTunOffload = func(int, uintptr) error { return wantErr }
	featureCalled := false
	enableTapTXTCPMangleIDSegmentation = func(string) {
		featureCalled = true
	}

	err := configureTapFD(42, "tap42")
	if !errors.Is(err, wantErr) {
		t.Fatalf("configureTapFD error=%v, want %v", err, wantErr)
	}
	if featureCalled {
		t.Fatal("optional ethtool feature ran after required offload configuration failed")
	}
}
