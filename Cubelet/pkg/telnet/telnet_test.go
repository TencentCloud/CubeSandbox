// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package telnet

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestDoTCPDetect(t *testing.T) {
	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	err = doTCPDetect(addr, defaultDialTimeOut)
	assert.NoError(t, err)

	err = doTCPDetect("localhost:7760", defaultDialTimeOut)
	assert.Error(t, err)
}

func testGetPort(addr string) int32 {
	ipports := strings.Split(addr, ":")
	port, _ := strconv.Atoi(ipports[1])
	return int32(port)
}

func TestTelnet(t *testing.T) {
	initT := 0
	timeout := 50
	period := 10
	cfg := &ProbeConfig{
		Addr: "localhost",

		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 1,
		FailureThreshold: 1,
	}

	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	cfg.Port = testGetPort(addr)

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Nil(t, bErr)
	assert.False(t, bTimeout)
}

func TestTelnetCtxTimeout(t *testing.T) {
	initT := 0
	timeout := 50
	period := 10
	cfg := &ProbeConfig{
		Addr: "localhost",

		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 8,
		FailureThreshold: 1,
	}

	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	cfg.Port = testGetPort(addr)
	tSum := cfg.InitialDelay + cfg.Timeout
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, _ := wait(ctx, retCh)
	assert.True(t, bTimeout)
}

func TestTelnetTimeout(t *testing.T) {
	initT := 0
	timeout := 50
	period := 10
	cfg := &ProbeConfig{
		Addr: "localhost",

		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 8,
		FailureThreshold: 1,
	}

	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	cfg.Port = testGetPort(addr)
	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
}

func wait(ctx context.Context, retCh chan error) (bTimeout bool, err error) {
	bTimeout = false
	select {
	case <-ctx.Done():
		bTimeout = true
	case ret := <-retCh:
		err = ret
	}
	return
}

func TestTelnet_exitDetect_succ(t *testing.T) {
	initT := 0
	timeout := 60
	period := 10
	cfg := &ProbeConfig{
		Addr: "localhost",

		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 5,
		FailureThreshold: 1,
	}

	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	cfg.Port = testGetPort(addr)

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Nil(t, bErr)
	assert.False(t, bTimeout)
}

func TestTelnet_exitDetect_fail(t *testing.T) {
	initT := 0
	timeout := 50
	period := 10
	cfg := &ProbeConfig{
		Addr:             "localhost",
		Port:             7778,
		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 5,
		FailureThreshold: 5,
	}

	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
}

func makeTestingHttpProbeConfig(port int32) *ProbeConfig {
	initT := 0
	timeout := 50
	period := 10
	cfg := &ProbeConfig{
		Addr:             "localhost",
		Port:             port,
		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 1,
		FailureThreshold: 4,
		Action:           ActionHTTPGet,
		HttpGetRequest: &http.Request{
			Method: "GET",
			URL:    &url.URL{Path: "/healthz", Scheme: "http", Host: fmt.Sprintf("localhost:%d", port)},
		},
	}
	return cfg
}

func TestHttpProbeSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	cfg := makeTestingHttpProbeConfig(testGetPort(addr))
	srv := &http.Server{Handler: mux}
	go func() {

		srv.Serve(l)
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Nil(t, bErr)
	assert.False(t, bTimeout)
}

func TestHttpConnFail(t *testing.T) {
	cfg := makeTestingHttpProbeConfig(7781)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: "localhost:7782", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
}

func TestHttpProbeFailWithBody(t *testing.T) {
	cfg := makeTestingHttpProbeConfig(7783)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errcode": 130451, "errmsg": "error"}`))
	})
	srv := &http.Server{Addr: "localhost:7783", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
	status, ok := ret.FromError(bErr)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_ContainerStateExitedByUser, status.Code())
}

func TestHttpProbeFailWithoutBody(t *testing.T) {
	cfg := makeTestingHttpProbeConfig(7784)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	srv := &http.Server{Addr: "localhost:7784", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
	status, ok := ret.FromError(bErr)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, status.Code())
}

func TestHttpProbeFailWith500(t *testing.T) {
	cfg := makeTestingHttpProbeConfig(7785)
	cfg.FailureThreshold = 100
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errcode": 130451, "errmsg": "error"}`))
	})
	srv := &http.Server{Addr: "localhost:7785", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
	status, ok := ret.FromError(bErr)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_ContainerStateExitedByUser, status.Code())
}

func TestTelnetForCubebox(t *testing.T) {
	initT := 0
	timeout := 50
	period := 10
	cfg := &ProbeConfig{
		Addr:             "localhost",
		InitialDelay:     time.Duration(initT) * time.Millisecond,
		Timeout:          time.Duration(timeout) * time.Millisecond,
		Period:           time.Duration(period) * time.Millisecond,
		SuccessThreshold: 1,
		FailureThreshold: 1,
		InstanceType:     "cubebox",
	}

	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer l.Close()
	addr := l.Addr().String()
	t.Logf("telnet: %s", addr)
	cfg.Port = testGetPort(addr)

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Nil(t, bErr)
	assert.False(t, bTimeout)
}
func randomBetween400And600() int {
	return 400 + rand.Intn(200)
}

func TestHttpProbeForCubeboxWith400Plus(t *testing.T) {
	cfg := makeTestingHttpProbeConfig(7786)
	cfg.InstanceType = "cubebox"
	cfg.FailureThreshold = 8
	cfg.InitialDelay = time.Second
	cfg.Period = 500 * time.Millisecond
	cfg.Timeout = 5 * time.Second
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(randomBetween400And600())
		w.Write([]byte(`{"errcode": 130452, "errmsg": "bad request"}`))
	})
	srv := &http.Server{Addr: "localhost:7786", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.Error(t, bErr)
	assert.False(t, bTimeout)
	status, ok := ret.FromError(bErr)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, status.Code())
	t.Logf("%s", bErr.Error())
	assert.True(t, strings.Contains(bErr.Error(), "statuscode"))
}

func randomBetween200And400() int {
	return 200 + rand.Intn(199)
}
func TestHttpProbeForCubeboxWith200To400(t *testing.T) {
	cfg := makeTestingHttpProbeConfig(7787)
	cfg.InstanceType = "cubebox"
	cfg.FailureThreshold = 100
	cfg.InitialDelay = time.Second
	cfg.Period = 500 * time.Millisecond
	cfg.Timeout = 5 * time.Second

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(randomBetween200And400())
		w.Write([]byte(`{"errcode": 0, "errmsg": "success"}`))
	})
	srv := &http.Server{Addr: "localhost:7787", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	tSum := cfg.InitialDelay + cfg.Timeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	retCh := Telnet(ctx, cfg)
	bTimeout, bErr := wait(ctx, retCh)
	assert.NoError(t, bErr)
	assert.False(t, bTimeout)
	status, ok := ret.FromError(bErr)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_Success, status.Code())
}
