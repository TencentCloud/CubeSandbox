// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"errors"
	"log"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	macAddr = "20:90:6f:fc:fc:fc"
	ipAddr  = "203.0.113.16"
)

func getARPEntries(name string) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		log.Fatal(err)
	}

	neighs, err := netlink.NeighList(link.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		log.Fatal(err)
	}

	for _, neigh := range neighs {
		log.Println(neigh.String())
	}
}

func addARPEntry(name string) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		log.Fatal(err)
	}

	mac, err := net.ParseMAC(macAddr)
	if err != nil {
		log.Fatal(err)
	}

	err = netlink.NeighAdd(&netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       netlink.FAMILY_V4,
		State:        netlink.NUD_PERMANENT,
		Type:         unix.RTN_UNSPEC,
		Flags:        0,
		IP:           net.ParseIP(ipAddr),
		HardwareAddr: mac,
	})

	if err == nil {
		return
	}

	if errors.Is(err, unix.EEXIST) {
		log.Println("ARP entry exists")

		return
	}

	log.Fatal(err)
}

func main() {
	name := os.Args[1]
	addARPEntry(name)
	getARPEntries(name)
}
