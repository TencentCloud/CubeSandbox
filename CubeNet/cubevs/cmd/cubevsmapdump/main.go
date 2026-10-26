package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

type mapListFlag []string

func (f *mapListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *mapListFlag) Set(value string) error {
	for _, item := range strings.Split(value, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		*f = append(*f, name)
	}
	return nil
}

func main() {
	var mapNames mapListFlag
	var ifindexArg string
	var compact bool
	var listMaps bool

	flag.Var(&mapNames, "map", "business map to dump; may be repeated or comma-separated; use all for every supported map")
	flag.StringVar(&ifindexArg, "ifindex", "", "filter sandbox ifindex; accepts numeric ifindex or interface name")
	flag.BoolVar(&compact, "compact", false, "print compact JSON")
	flag.BoolVar(&listMaps, "list-maps", false, "list supported business map names and exit")
	flag.Parse()

	if listMaps {
		for _, name := range cubevs.BusinessMapNames() {
			fmt.Println(name)
		}
		return
	}

	opts := cubevs.DumpOptions{MapNames: mapNames}
	if ifindexArg != "" {
		ifindex, err := parseIfindex(ifindexArg)
		if err != nil {
			fatalf("parse ifindex failed: %v", err)
		}
		opts.FilterIfindex = true
		opts.Ifindex = ifindex
	}

	dump, err := cubevs.DumpBusinessMaps(opts)
	if err != nil {
		fatalf("dump CubeVS business maps failed: %v", err)
	}

	encoder := json.NewEncoder(os.Stdout)
	if !compact {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(dump); err != nil {
		fatalf("encode dump failed: %v", err)
	}
}

func parseIfindex(value string) (uint32, error) {
	ifindex, err := strconv.ParseUint(value, 10, 32)
	if err == nil {
		return uint32(ifindex), nil
	}

	iface, err := net.InterfaceByName(value)
	if err != nil {
		return 0, err
	}
	return uint32(iface.Index), nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
