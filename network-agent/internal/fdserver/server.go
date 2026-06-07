// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package fdserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
)

type tapFDRequest struct {
	Name      string `json:"name"`
	SandboxID string `json:"sandboxId"`
}

type Server struct {
	path     string
	provider service.TapFDProvider
	listener *net.UnixListener
	wg       sync.WaitGroup
}

func New(endpoint string, provider service.TapFDProvider) (*Server, error) {
	path, err := unixPath(endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepathDir(path), 0755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	lis, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	return &Server{
		path:     path,
		provider: provider,
		listener: lis,
	}, nil
}

func (s *Server) Start() error {
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			if ne, ok := err.(*net.OpError); ok && !ne.Temporary() {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			_ = handleConn(conn, s.provider)
		}()
	}
}

func (s *Server) Stop(_ context.Context) error {
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			return err
		}
	}
	s.wg.Wait()
	if s.path != "" {
		_ = os.Remove(s.path)
	}
	return nil
}

func handleConn(conn *net.UnixConn, provider service.TapFDProvider) error {
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	req := &tapFDRequest{}
	if err := json.Unmarshal(buf[:n], req); err != nil {
		return writeResponse(conn, []byte(`{"errCode":"1001","errMsg":"Parse json failed"}`), nil)
	}
	file, ifindex, err := provider.GetTapFile(req.SandboxID, req.Name)
	if err != nil {
		return writeResponse(conn, []byte(fmt.Sprintf(`{"errCode":"1002","errMsg":%q}`, err.Error())), nil)
	}
	return writeResponse(conn, []byte(fmt.Sprintf(`{"errCode":"0","errMsg":"Success","ifindex":%d}`, ifindex)), file)
}

func writeResponse(conn *net.UnixConn, payload []byte, file *os.File) error {
	var rights []byte
	if file != nil {
		rights = syscall.UnixRights(int(file.Fd()))
	}
	_, _, err := conn.WriteMsgUnix(payload, rights, nil)
	return err
}

func unixPath(endpoint string) (string, error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		return strings.TrimPrefix(endpoint, "unix://"), nil
	case strings.HasPrefix(endpoint, "/"):
		return endpoint, nil
	default:
		return "", fmt.Errorf("unsupported fd endpoint %q", endpoint)
	}
}

func filepathDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "."
	}
	return path[:idx]
}
