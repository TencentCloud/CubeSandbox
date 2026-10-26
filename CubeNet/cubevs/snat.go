package cubevs

import (
	"errors"
	"net"
	"slices"

	"github.com/cilium/ebpf"
)

const (
	mapSNATIPList = "snat_iplist"
)

const (
	maxPortStart = 30000
	maxSNATIPs   = 4
)

// SNATIP contains IP used for SNAT.
type SNATIP struct {
	Ifindex int
	IP      net.IP
}

type snatIP struct {
	Lock     uint32
	Ifindex  uint32
	IP       uint32
	MaxPort  uint16
	Reserved uint16
}

// SetSNATIPs sets the IPs that will be used for SNAT.
func SetSNATIPs(ips []*SNATIP) error {
	if len(ips) == 0 {
		return nil
	}

	ipList := make([]snatIP, len(ips))
	for i, ip := range ips {
		ipList[i].Ifindex = uint32(ip.Ifindex)
		ipList[i].IP = ipToUint32(ip.IP)
		ipList[i].MaxPort = maxPortStart
	}
	slices.SortFunc(ipList, func(a, b snatIP) int { return int(a.IP) - int(b.IP) })

	return setSNATIPs(ipList)
}

func setSNATIPs(ips []snatIP) error {
	m, err := loadPinnedMap(mapSNATIPList)
	if err != nil {
		return err
	}
	defer m.Close()

	var (
		l      = len(ips)
		outErr error
	)

	for i := range maxSNATIPs {
		key := uint32(i)
		newValue := ips[i%l]
		var oldValue snatIP

		err := m.Lookup(&key, &oldValue)
		if err != nil {
			outErr = errors.Join(outErr, err)
			continue
		}

		if oldValue.IP == newValue.IP && oldValue.Ifindex == newValue.Ifindex {
			// already set
			continue
		}

		err = m.Update(&key, &newValue, ebpf.UpdateAny)
		if err != nil {
			outErr = errors.Join(outErr, err)
		}
	}
	return outErr
}
