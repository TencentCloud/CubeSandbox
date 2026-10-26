// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"log"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func getRawFlags(name string) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		log.Fatal(err)
	}

	flags := link.Attrs().RawFlags
	log.Printf("raw flags: 0x%x\n", flags)

	if flags&unix.IFF_LOWER_UP != 0 {
		log.Println("IFF_LOWER_UP is on")
	}
}

func main() {
	name := os.Args[1]
	getRawFlags(name)
}
