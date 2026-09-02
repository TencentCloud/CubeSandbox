package runtime

import (
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
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

func TestDestroyTapSkipsPolicyCleanupOnTransientLookupError(t *testing.T) {
	origLookup := netlinkLinkByIndex
	origDelete := deleteTAPDevicePolicyMaps
	t.Cleanup(func() {
		netlinkLinkByIndex = origLookup
		deleteTAPDevicePolicyMaps = origDelete
	})

	transient := errors.New("dump interrupted")
	netlinkLinkByIndex = func(int) (netlink.Link, error) { return nil, transient }
	deleted := false
	deleteTAPDevicePolicyMaps = func(uint32) error {
		deleted = true
		return nil
	}

	err := destroyTap(77)
	if !errors.Is(err, transient) {
		t.Fatalf("destroyTap error=%v, want %v", err, transient)
	}
	if deleted {
		t.Fatal("policy maps must not be deleted on transient lookup errors")
	}
}

func TestDestroyTapCleansPolicyWhenLinkAlreadyGone(t *testing.T) {
	origLookup := netlinkLinkByIndex
	origDelete := deleteTAPDevicePolicyMaps
	t.Cleanup(func() {
		netlinkLinkByIndex = origLookup
		deleteTAPDevicePolicyMaps = origDelete
	})

	notFound := netlink.LinkNotFoundError{}
	netlinkLinkByIndex = func(int) (netlink.Link, error) { return nil, notFound }
	var gotIfindex uint32
	deleteTAPDevicePolicyMaps = func(ifindex uint32) error {
		gotIfindex = ifindex
		return nil
	}

	err := destroyTap(88)
	if !errors.As(err, &netlink.LinkNotFoundError{}) {
		t.Fatalf("destroyTap error=%v, want LinkNotFoundError", err)
	}
	if gotIfindex != 88 {
		t.Fatalf("policy cleanup ifindex=%d, want 88", gotIfindex)
	}
}

func TestDestroyTapPolicyCleanupFailureDoesNotFailDestroy(t *testing.T) {
	origLookup := netlinkLinkByIndex
	origDelete := deleteTAPDevicePolicyMaps
	origDel := netlinkLinkDel
	t.Cleanup(func() {
		netlinkLinkByIndex = origLookup
		deleteTAPDevicePolicyMaps = origDelete
		netlinkLinkDel = origDel
	})

	lookups := 0
	netlinkLinkByIndex = func(int) (netlink.Link, error) {
		lookups++
		if lookups == 1 {
			return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 99, Name: "z192.168.0.99"}}, nil
		}
		return nil, netlink.LinkNotFoundError{}
	}
	netlinkLinkDel = func(netlink.Link) error { return nil }
	deleteTAPDevicePolicyMaps = func(uint32) error {
		return errors.New("bpf map missing")
	}

	if err := destroyTap(99); err != nil {
		t.Fatalf("destroyTap error=%v, want nil when only policy cleanup fails", err)
	}
}

func TestTapNameIsBareIPv4(t *testing.T) {
	if got := tapName("192.168.195.183"); got != "192.168.195.183" {
		t.Fatalf("tapName=%q, want bare IPv4", got)
	}
}

func TestParseCubeTapIPAcceptsCurrentAndLegacyNames(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{name: "192.168.0.10", want: "192.168.0.10"},
		{name: "z192.168.0.10", want: "192.168.0.10"},
		{name: "10.1.2.3", want: "10.1.2.3"},
		{name: "z10.1.2.3", want: "10.1.2.3"},
	}
	for _, tc := range cases {
		got, err := parseCubeTapIP(tc.name)
		if err != nil {
			t.Fatalf("parseCubeTapIP(%q): %v", tc.name, err)
		}
		if got.String() != tc.want {
			t.Fatalf("parseCubeTapIP(%q)=%s, want %s", tc.name, got, tc.want)
		}
	}
	for _, name := range []string{"eth0", "zeth0", "cube-dev", "zz192.168.0.1", "2001:db8::1"} {
		if _, err := parseCubeTapIP(name); err == nil {
			t.Fatalf("parseCubeTapIP(%q) succeeded, want error", name)
		}
	}
}

func TestCandidateTapNamesCurrentThenLegacy(t *testing.T) {
	got := candidateTapNames("192.168.0.10")
	want := []string{"192.168.0.10", "z192.168.0.10"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("candidateTapNames=%v, want %v", got, want)
	}
}

func TestListCubeTapsAcceptsBothNamesAndFiltersCIDR(t *testing.T) {
	origList := netlinkLinkList
	t.Cleanup(func() { netlinkLinkList = origList })
	netlinkLinkList = func() ([]netlink.Link, error) {
		return []netlink.Link{
			&netlink.Tuntap{
				LinkAttrs: netlink.LinkAttrs{Name: "z192.168.0.10", Index: 10},
				Mode:      netlink.TUNTAP_MODE_TAP,
			},
			&netlink.Tuntap{
				LinkAttrs: netlink.LinkAttrs{Name: "192.168.0.11", Index: 11},
				Mode:      netlink.TUNTAP_MODE_TAP,
			},
			&netlink.Tuntap{
				LinkAttrs: netlink.LinkAttrs{Name: "10.0.0.1", Index: 12},
				Mode:      netlink.TUNTAP_MODE_TAP,
			},
			&netlink.Tuntap{
				LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 13},
				Mode:      netlink.TUNTAP_MODE_TAP,
			},
			&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "192.168.0.12", Index: 14}},
		}, nil
	}

	_, cidr, err := net.ParseCIDR("192.168.0.0/18")
	if err != nil {
		t.Fatal(err)
	}
	got, err := listCubeTaps(cidr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listCubeTaps got %d devices: %+v", len(got), got)
	}
	if tap := got["192.168.0.10"]; tap == nil || tap.Name != "z192.168.0.10" || tap.Index != 10 {
		t.Fatalf("legacy z+IP tap: %+v", tap)
	}
	if tap := got["192.168.0.11"]; tap == nil || tap.Name != "192.168.0.11" || tap.Index != 11 {
		t.Fatalf("current IP tap: %+v", tap)
	}

	empty, err := listCubeTaps(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("nil CIDR must not claim host TAPs, got %+v", empty)
	}
}
