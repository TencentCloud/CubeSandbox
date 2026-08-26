// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"flag"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/urfave/cli"
)

// newGlobalContext builds a cli.Context with global flags set (address/port/timeout).
func newGlobalContext(t *testing.T, address, port string, timeout time.Duration) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	for _, f := range globalFlags() {
		f.Apply(set)
	}
	_ = set.Set("address", address)
	_ = set.Set("port", port)
	ctx := cli.NewContext(nil, set, nil)
	return ctx
}

func globalFlags() []cli.Flag {
	return []cli.Flag{
		cli.StringFlag{Name: "address"},
		cli.StringFlag{Name: "port"},
		cli.DurationFlag{Name: "timeout"},
	}
}

func TestGetServerAddrs(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want []string
	}{
		{"single", "10.0.0.1", []string{"10.0.0.1"}},
		{"comma-separated", "10.0.0.1,10.0.0.2", []string{"10.0.0.1", "10.0.0.2"}},
		{"with spaces", " 10.0.0.1 , 10.0.0.2 ", []string{"10.0.0.1", "10.0.0.2"}},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newGlobalContext(t, tc.addr, "3010", 35*time.Second)
			got := getServerAddrs(c)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildURL(t *testing.T) {
	tests := []struct {
		host, port, path string
		want             string
	}{
		{"10.0.0.1", "3010", "/internal/v1/nodes", "http://10.0.0.1:3010/internal/v1/nodes"},
		{"localhost", "8080", "/", "http://localhost:8080/"},
		{"[::1]", "3010", "/health", "http://[::1]:3010/health"},
		{"::1", "3010", "/health", "http://[::1]:3010/health"},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			port = tc.port
			got := buildURL(tc.host, tc.path)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMarshalBody(t *testing.T) {
	t.Run("nil returns empty reader", func(t *testing.T) {
		r := marshalBody(nil)
		data, err := io.ReadAll(r)
		assert.NoError(t, err)
		assert.Empty(t, data)
	})

	t.Run("struct returns json", func(t *testing.T) {
		r := marshalBody(map[string]string{"k": "v"})
		data, err := io.ReadAll(r)
		assert.NoError(t, err)
		assert.JSONEq(t, `{"k":"v"}`, string(data))
	})
}

func TestPickHost(t *testing.T) {
	t.Run("empty returns error", func(t *testing.T) {
		serverList = nil
		_, err := pickHost()
		assert.Error(t, err)
	})

	t.Run("returns a host from list", func(t *testing.T) {
		serverList = []string{"10.0.0.1", "10.0.0.2"}
		got, err := pickHost()
		assert.NoError(t, err)
		assert.Contains(t, serverList, got)
	})
}
