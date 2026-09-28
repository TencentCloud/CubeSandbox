// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package vsockets

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	VSockSocketScheme     = "vsock"
	HybridVSockScheme     = "hvsock"
	MockHybridVSockScheme = "mock"

	retryTime = 100 * time.Millisecond
)

type HybridVSock struct {
	UdsPath   string
	ContextID uint64
	Port      uint32
}

func (s *HybridVSock) String() string {
	return fmt.Sprintf("%s://%s:%d", HybridVSockScheme, s.UdsPath, s.Port)
}

func parseGrpcHybridVSockAddr(sock string) (string, uint32, error) {
	sp := strings.Split(sock, ":")

	if len(sp) < 2 {
		return "", 0, grpcStatus.Errorf(codes.InvalidArgument, "Invalid hybrid vsock address: %s", sock)
	}
	if sp[0] != HybridVSockScheme {
		return "", 0, grpcStatus.Errorf(codes.InvalidArgument, "Invalid hybrid vsock URL scheme: %s", sock)
	}

	port := uint32(0)

	if len(sp) == 3 {
		p, err := strconv.ParseUint(sp[2], 10, 32)
		if err == nil {
			port = uint32(p)
		}
	}

	return sp[1], port, nil
}

type wrapConn struct {
	net.Conn
	localPort             string
	log                   *log.CubeWrapperLogEntry
	readCount, writeCount int64
}

func newWrapConn(conn net.Conn, localport string) *wrapConn {
	c := &wrapConn{
		Conn:      conn,
		localPort: localport,
	}

	c.log = log.L.WithField("localport", localport)
	c.log.Tracef("Created hybrid vsock connection")
	return c
}
func (w *wrapConn) Close() error {
	w.log.WithFields(CubeLog.Fields{
		"readCount":  w.readCount,
		"writeCount": w.writeCount,
	}).Tracef("Closing hybrid vsock connection %s", w.localPort)

	return w.Conn.Close()
}

func (w *wrapConn) Read(p []byte) (n int, err error) {
	n, err = w.Conn.Read(p)
	if err != nil {
		w.log.Tracef("Failed to read from hybrid vsock connection %s: %v", w.localPort, err)
		return
	}
	w.readCount += int64(n)
	return
}

func (w *wrapConn) Write(p []byte) (n int, err error) {
	n, err = w.Conn.Write(p)
	if err != nil {
		w.log.Tracef("Failed to write to hybrid vsock connection %s: %v", w.localPort, err)
		return
	}
	w.writeCount += int64(n)
	return
}

func HybridVSockDialer(sock string, timeout time.Duration) (net.Conn, error) {
	udsPath, port, err := parseGrpcHybridVSockAddr(sock)
	if err != nil {
		return nil, err
	}

	dialFunc := func() (net.Conn, error) {
		handshakeTimeout := 2 * time.Second
		dialer := &net.Dialer{
			Timeout:       handshakeTimeout,
			KeepAlive:     handshakeTimeout,
			FallbackDelay: 300 * time.Millisecond,
		}
		conn, err := dialer.Dial("unix", udsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to dial to %s: %v", udsPath, err)
		}

		deadline := time.Now().Add(handshakeTimeout)
		conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})

		if _, err = conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to write CONNECT to %s with port %d: %v", udsPath, port, err)
		}
		reader := bufio.NewReader(conn)
		var (
			response string
		)

		err = wait.PollUntilContextTimeout(context.Background(), 10*time.Millisecond, handshakeTimeout, false, func(ctx context.Context) (bool, error) {
			response, err = reader.ReadString('\n')
			if err != nil {
				return false, err
			}
			if strings.Contains(response, "OK") {
				after, _ := strings.CutPrefix(response, "OK ")
				localport := strings.TrimSpace(after)
				conn = newWrapConn(conn, localport)
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("HybridVsock failed to read handshake response from %s with port %d: %v", udsPath, port, err)
		}
		return conn, nil

	}

	timeoutErr := grpcStatus.Errorf(codes.DeadlineExceeded, "timed out connecting to hybrid vsocket %s", sock)
	return commonDialer(timeout, dialFunc, timeoutErr)
}

func commonDialer(timeout time.Duration, dialFunc func() (net.Conn, error), timeoutErrMsg error) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ch := make(chan net.Conn)
	var err error
	go func() {
		for {
			select {
			case <-ctx.Done():

				return
			default:
			}

			var conn net.Conn
			conn, err = dialFunc()
			if err == nil {

				ch <- conn
				return
			}

			errstr := err.Error()
			if strings.Contains(errstr, "no such file or directory") ||
				strings.Contains(errstr, "connection refused") ||
				strings.Contains(errstr, "resource temporarily unavailable") {
				ch <- nil
				return
			}
			if strings.Contains(errstr, "EOF") {
				time.Sleep(retryTime)
				continue
			}
			log.L.Debugf("Failed to connect to hybrid vsocket, will be retry later: %s", err)
			time.Sleep(retryTime)
		}
	}()

	select {
	case conn := <-ch:
		if conn == nil {
			return nil, err
		}
		return conn, nil
	case <-ctx.Done():
		log.L.Debugf("Timed out connecting to hybrid vsocket: %s", string(debug.Stack()))
		if err != nil {
			return nil, err
		}
		return nil, timeoutErrMsg
	}
}
