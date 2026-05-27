// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
)

func main() {
	var (
		configPath = flag.String("conf", "/etc/cubecow/cubecow.toml", "path to cubecow.toml")
		volumeName = flag.String("name", fmt.Sprintf("cubecow-smoke-%d", time.Now().UnixNano()), "volume name")
		snapshot   = flag.String("snap", "", "snapshot name")
		sizeBytes  = flag.Uint64("size", 1<<30, "volume size in bytes")
		growBytes  = flag.Uint64("grow-by", 256<<20, "bytes added during resize step")
	)
	flag.Parse()

	snapshotName := *snapshot
	if snapshotName == "" {
		snapshotName = *volumeName + "-snap"
	}

	stepStart := time.Now()
	engine, err := cubecow.InitWithoutLogging(*configPath)
	if err != nil {
		fatalf("InitWithoutLogging", stepStart, err)
	}
	okf("InitWithoutLogging", stepStart, "config=%s", *configPath)
	defer engine.Close()

	stepStart = time.Now()
	devicePath, err := engine.CreateVolume(*volumeName, *sizeBytes)
	if err != nil {
		fatalf("CreateVolume", stepStart, err)
	}
	okf("CreateVolume", stepStart, "name=%s device=%s", *volumeName, devicePath)
	defer func() { _ = engine.DeleteVolume(*volumeName) }()

	stepStart = time.Now()
	info, err := engine.GetVolumeInfo(*volumeName)
	if err != nil {
		fatalf("GetVolumeInfo", stepStart, err)
	}
	okf("GetVolumeInfo", stepStart, "size=%d device=%s", info.SizeBytes, info.DevicePath)

	stepStart = time.Now()
	oldSize, newSize, err := engine.ResizeVolume(*volumeName, info.SizeBytes+*growBytes)
	if err != nil {
		fatalf("ResizeVolume", stepStart, err)
	}
	okf("ResizeVolume", stepStart, "old=%d new=%d", oldSize, newSize)

	stepStart = time.Now()
	snapDevice, err := engine.CreateSnapshot(*volumeName, snapshotName, true)
	if err != nil {
		fatalf("CreateSnapshot", stepStart, err)
	}
	okf("CreateSnapshot", stepStart, "name=%s device=%s", snapshotName, snapDevice)
	defer func() { _ = engine.DeleteSnapshot(snapshotName) }()

	stepStart = time.Now()
	snapshots, err := engine.ListSnapshots(*volumeName, 0, "")
	if err != nil {
		fatalf("ListSnapshots", stepStart, err)
	}
	okf("ListSnapshots", stepStart, "count=%d", len(snapshots.Snapshots))

	stepStart = time.Now()
	metrics, err := engine.GetMetrics()
	if err != nil {
		fatalf("GetMetrics", stepStart, err)
	}
	okf("GetMetrics", stepStart, "%s", summarizeMetrics(metrics))

	stepStart = time.Now()
	if err := engine.DeleteSnapshot(snapshotName); err != nil {
		fatalf("DeleteSnapshot", stepStart, err)
	}
	okf("DeleteSnapshot", stepStart, "name=%s", snapshotName)

	stepStart = time.Now()
	if err := engine.DeleteVolume(*volumeName); err != nil {
		fatalf("DeleteVolume", stepStart, err)
	}
	okf("DeleteVolume", stepStart, "name=%s", *volumeName)
}

func summarizeMetrics(metrics map[string]uint64) string {
	keys := make([]string, 0, len(metrics))
	for k := range metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 6 {
		keys = keys[:6]
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, metrics[k]))
	}
	return fmt.Sprintf("keys=%d [%s]", len(metrics), join(parts))
}

func join(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out += ", " + parts[i]
	}
	return out
}

func okf(step string, started time.Time, format string, args ...any) {
	fmt.Printf("OK   %-18s %8s %s\n", step, time.Since(started).Round(time.Millisecond), fmt.Sprintf(format, args...))
}

func fatalf(step string, started time.Time, err error) {
	fmt.Fprintf(os.Stderr, "FAIL %-18s %8s %v\n", step, time.Since(started).Round(time.Millisecond), err)
	os.Exit(1)
}
