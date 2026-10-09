// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
)

// captureServerName accepts connections, reads each TLS ClientHello and reports
// the server name (SNI) of the first one. Every handshake is aborted right
// after, so the listener needs no certificate and the client's retries fail
// fast.
func captureServerName(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	sni := make(chan string, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_ = tls.Server(conn, &tls.Config{
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					select {
					case sni <- hello.ServerName:
					default:
					}
					return nil, errors.New("server name captured")
				},
			}).Handshake()
			conn.Close()
		}
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

func ping(t *testing.T, client redis.UniversalClient) error {
	t.Helper()
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return client.Ping(ctx).Err()
}

// tlsConfig leaves ServerName empty, so certificate verification checks the
// host of the dial address, which crypto/tls sends as the server name. Go never
// sends an IP literal as SNI, so the addresses here are host names.
func TestNewTLSSendsServerName(t *testing.T) {
	port, sni := captureServerName(t)
	cfg := config.Default()
	cfg.RedisAddr = net.JoinHostPort("localhost", port)
	cfg.RedisTLS = true
	assert.Error(t, ping(t, New(cfg)))
	assertServerName(t, sni, "localhost")
}

// In Sentinel mode the switch also covers the Sentinel dial, whose server name
// comes from the Sentinel address.
func TestNewSentinelTLSSendsServerName(t *testing.T) {
	port, sni := captureServerName(t)
	cfg := config.Default()
	cfg.RedisAddr = ""
	cfg.RedisMasterName = "mymaster"
	cfg.RedisSentinelAddrs = []string{net.JoinHostPort("localhost", port)}
	cfg.RedisTLS = true
	assert.Error(t, ping(t, New(cfg)))
	assertServerName(t, sni, "localhost")
}

func TestNewPlaintextHasNoTLSConfig(t *testing.T) {
	standalone := config.Default()
	standalone.RedisAddr = "localhost:6379"

	sentinel := config.Default()
	sentinel.RedisAddr = ""
	sentinel.RedisMasterName = "mymaster"
	sentinel.RedisSentinelAddrs = []string{"localhost:26379"}

	for name, cfg := range map[string]*config.Config{"standalone": standalone, "sentinel": sentinel} {
		client, ok := New(cfg).(*redis.Client)
		require.True(t, ok, name)
		assert.Nil(t, client.Options().TLSConfig, name)
		_ = client.Close()
	}
}
