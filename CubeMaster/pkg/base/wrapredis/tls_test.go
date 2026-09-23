// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package wrapredis

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// captureServerName accepts one connection, reads the TLS ClientHello and
// reports the server name (SNI) the client sent. The handshake is aborted right
// after, so the listener needs no certificate.
func captureServerName(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	sni := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_ = tls.Server(conn, &tls.Config{
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				sni <- hello.ServerName
				return nil, errors.New("server name captured")
			},
		}).Handshake()
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port, sni
}

func assertServerName(t *testing.T, sni <-chan string, want string) {
	t.Helper()
	select {
	case got := <-sni:
		assert.Equal(t, want, got)
	case <-time.After(3 * time.Second):
		t.Fatal("no TLS ClientHello received")
	}
}

// With tls set, the master dial must send a ClientHello whose server name is
// the host of the dial address; certificate verification checks that name.
// Go never sends an IP literal as SNI, so the address here is a host name.
func TestNewConnTLSSendsServerName(t *testing.T) {
	port, sni := captureServerName(t)
	_, err := newConn(net.JoinHostPort("localhost", port), "", 0, true)
	assert.Error(t, err)
	assertServerName(t, sni, "localhost")
}

// The Sentinel probe follows the same switch and derives the server name from
// the Sentinel address.
func TestSentinelGetMasterTLSSendsServerName(t *testing.T) {
	port, sni := captureServerName(t)
	_, err := sentinelGetMaster(net.JoinHostPort("localhost", port), "mymaster", "", true)
	assert.Error(t, err)
	assertServerName(t, sni, "localhost")
}
