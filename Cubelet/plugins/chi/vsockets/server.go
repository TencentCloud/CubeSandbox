// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package vsockets

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mdlayher/vsock"
)

func ParseAndListen(addrStr string) (net.Listener, error) {
	addr, err := url.Parse(addrStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse address %q: %w", addrStr, err)
	}

	switch addr.Scheme {
	case "vsock":
		_, port, err := net.SplitHostPort(addr.Host)
		if err != nil {
			return nil, fmt.Errorf("invalid vsock address %s format: %w", addr.Host, err)
		}
		p, err := strconv.ParseInt(port, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid vsock address %s port format: %w", addr.Host, err)
		}

		l, err := vsock.Listen(uint32(p), &vsock.Config{})
		if err != nil {
			return nil, fmt.Errorf("failed to listen vsock: %w", err)
		}
		return l, nil
	case "unix":
		socketPath, err := url.PathUnescape(addr.Path)
		if err != nil {
			return nil, fmt.Errorf("invalid unix socket path: %w", err)
		}

		if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
			return nil, fmt.Errorf("failed to create socket directory: %w", err)
		}

		if err := os.RemoveAll(socketPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to clean up old socket: %w", err)
		}

		l, err := net.Listen("unix", socketPath)
		if err != nil {
			return nil, fmt.Errorf("failed to listen unix socket: %w", err)
		}

		if err := os.Chmod(socketPath, 0666); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("failed to set socket permissions: %w", err)
		}
		return l, nil

	default:
		return nil, fmt.Errorf("unsupported address scheme: %s", addr.Scheme)
	}
}
