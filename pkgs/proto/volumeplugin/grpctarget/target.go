// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package grpctarget normalizes volume plugin gRPC dial/listen addresses.
//
// Supported forms:
//   - unix:///run/cube/plugin.sock
//   - /run/cube/plugin.sock          (implicit unix)
//   - tcp://127.0.0.1:9100
//   - 127.0.0.1:9100                 (implicit tcp)
package grpctarget

import "strings"

// Normalize returns a gRPC dial target (unix://… or host:port).
func Normalize(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if strings.Contains(addr, "://") {
		return addr
	}
	if strings.HasPrefix(addr, "/") {
		return "unix://" + addr
	}
	return addr
}

// ParseListen splits a listen address into network and address for net.Listen.
func ParseListen(addr string) (network, host string) {
	addr = strings.TrimSpace(addr)
	switch {
	case strings.HasPrefix(addr, "unix://"):
		return "unix", strings.TrimPrefix(addr, "unix://")
	case strings.HasPrefix(addr, "tcp://"):
		return "tcp", strings.TrimPrefix(addr, "tcp://")
	case strings.HasPrefix(addr, "/"):
		return "unix", addr
	default:
		return "tcp", addr
	}
}
