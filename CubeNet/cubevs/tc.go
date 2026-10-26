package cubevs

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/florianl/go-tc"
	"golang.org/x/sys/unix"
)

func tcMakeHandle(major, minor uint32) uint32 {
	return (major & tcHandleMajMask) | (minor & tcHandleMinMask)
}

func createQdisc(ifindex uint32) error {
	tcnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return fmt.Errorf("tc.Open failed: %w", err)
	}
	defer tcnl.Close()

	qdisc := tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: ifindex,
			Handle:  tcMakeHandle(tc.HandleIngress, 0),
			Parent:  tcHandleClsact,
			Info:    0,
		},
		Attribute: tc.Attribute{
			Kind: tcAttrKindClsact,
		},
	}
	err = tcnl.Qdisc().Add(&qdisc)
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("tcnl.Qdisc.Add failed: %w, ifindex: %d", err, ifindex)
	}

	return nil
}

func attachFilter(ifindex uint32, progFD uint32, progName string, direction TCDirection) error {
	tcnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return fmt.Errorf("tc.Open failed: %w", err)
	}
	defer tcnl.Close()

	flags := uint32(tcFlagDirectAction)
	filter := tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: ifindex,
			Handle:  tcFilterHandle,
			Parent:  tcMakeHandle(tcHandleClsact, uint32(direction)),
			Info:    tcMakeHandle(tcFilterPriority<<16, uint32(htons(syscall.ETH_P_ALL))),
		},
		Attribute: tc.Attribute{
			Kind: tcAttrKindBPF,
			BPF: &tc.Bpf{
				FD:    &progFD,
				Name:  &progName,
				Flags: &flags,
			},
		},
	}
	err = tcnl.Filter().Replace(&filter)
	if err != nil {
		return fmt.Errorf("tcnl.Filter.Replace failed: %w, ifindex: %d, progFD: %d, progName: %s",
			err, ifindex, progFD, progName)
	}

	return nil
}
