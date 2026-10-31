// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package hostquota shares the node's published quota policy with local admission.
package hostquota

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"k8s.io/apimachinery/pkg/api/resource"
)

func CPU(c *config.HostConf, cpuCount int, fallbacks ...int64) int64 {
	if c != nil && c.Quota.Cpu > 0 {
		return int64(c.Quota.Cpu)
	}
	for _, value := range fallbacks {
		if value > 0 {
			return value
		}
	}
	if cpuCount <= 0 {
		return 0
	}
	return int64(cpuCount) * 1000 * 2
}

func Memory(c *config.HostConf, readTotalMB func() (int64, error), fallbacks ...int64) (int64, error) {
	if c != nil && c.Quota.Mem != "" {
		q, err := resource.ParseQuantity(c.Quota.Mem)
		if err != nil {
			return 0, fmt.Errorf("invalid host memory quota: %w", err)
		}
		mb := q.Value() / (1024 * 1024)
		if mb <= 0 {
			return 0, fmt.Errorf("invalid host memory quota: explicit limit must be at least 1 MiB")
		}
		return mb, nil
	}
	for _, value := range fallbacks {
		if value > 0 {
			return value, nil
		}
	}
	mb, err := readTotalMB()
	if err != nil {
		return 0, err
	}
	if mb <= 0 {
		return 0, fmt.Errorf("host memory total must be positive")
	}
	return (mb*5 + 3) / 4, nil
}

func DetectMemoryTotalMB() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "MemTotal:" {
			continue
		}
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemTotal format")
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal: %w", err)
		}
		return (kb + 1023) / 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
