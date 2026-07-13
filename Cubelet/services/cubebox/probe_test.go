// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/proto"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"k8s.io/utils/pointer"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProbeErrIp(t *testing.T) {
	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        100,
			PeriodMs:         10,
			SuccessThreshold: 1,
			FailureThreshold: 1,
			ProbeHandler: &cubebox.ProbeHandler{
				TcpSocket: &cubebox.TCPSocketAction{},
			},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{
		NetworkInfo: &proto.ShimNetReq{
			Interfaces: []*proto.Interface{
				{
					IPAddr: net.ParseIP("invalid"),
				},
			},
		},
	}
	ci := &cubeboxstore.Container{}
	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	err, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_CreateNetworkFailed, err.Code())
	metrics := createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.NotNil(t, metrics[0].Error())
}

func TestProbeErrTimeout(t *testing.T) {
	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        -1,
			PeriodMs:         10,
			SuccessThreshold: 1,
			FailureThreshold: 1,
			ProbeHandler: &cubebox.ProbeHandler{
				TcpSocket: &cubebox.TCPSocketAction{},
			},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{
		NetworkInfo: &proto.ShimNetReq{
			Interfaces: []*proto.Interface{
				{
					IPAddr: net.ParseIP("127.0.0.1"),
				},
			},
		},
	}
	ci := &cubeboxstore.Container{
		IP: "127.0.0.1",
	}
	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	err, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, err.Code())
}

func TestProbeErrAction(t *testing.T) {
	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        -1,
			PeriodMs:         10,
			SuccessThreshold: 1,
			FailureThreshold: 1,
			ProbeHandler:     &cubebox.ProbeHandler{},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{
		NetworkInfo: &proto.ShimNetReq{
			Interfaces: []*proto.Interface{
				{
					IPAddr: net.ParseIP("127.0.0.1"),
				},
			},
		},
	}
	ci := &cubeboxstore.Container{
		IP: "127.0.0.1",
	}
	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	err, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, err.Code())
}

func TestDoCreateTimeEnvdInitPostsEnvVars(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/init" {
			t.Fatalf("path=%q, want /init", r.URL.Path)
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	u, err := neturl.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationComponentEnvdVersion: "0.2.0",
			constants.MasterAnnotationCreateTimeEnvVars:    `{"SESSION_ID":"user-session-test","USER_ID":"42"}`,
		},
	}
	sandBox := &cubeboxstore.CubeBox{IP: "127.0.0.1"}

	l := &local{envdHTTPClient: server.Client(), envdInitPort: port}
	if err := l.doCreateTimeEnvdInit(context.Background(), req, sandBox); err != nil {
		t.Fatalf("doCreateTimeEnvdInit err=%v", err)
	}
	envVars, ok := gotBody["envVars"].(map[string]any)
	if !ok {
		t.Fatalf("envVars payload missing: %#v", gotBody)
	}
	if envVars["SESSION_ID"] != "user-session-test" {
		t.Fatalf("SESSION_ID=%v, want user-session-test", envVars["SESSION_ID"])
	}
	if envVars["USER_ID"] != "42" {
		t.Fatalf("USER_ID=%v, want 42", envVars["USER_ID"])
	}
}

func TestDoCreateTimeEnvdInitFailsOnHTTPError(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		http.Error(w, "envd refused init", http.StatusInternalServerError)
	}))
	defer server.Close()

	u, err := neturl.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationComponentEnvdVersion: "0.2.0",
			constants.MasterAnnotationCreateTimeEnvVars:    `{"SESSION_ID":"user-session-test"}`,
		},
	}
	sandBox := &cubeboxstore.CubeBox{IP: "127.0.0.1"}

	l := &local{envdHTTPClient: server.Client(), envdInitPort: port}
	retErr := l.doCreateTimeEnvdInit(context.Background(), req, sandBox)
	if retErr == nil {
		t.Fatal("expected init failure")
	}
	errInfo, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_ExecCommandInSandboxFailed, errInfo.Code())
	assert.Contains(t, errInfo.Message(), "envd refused init")
	assert.Equal(t, 1, callCount)
}

func TestDoCreateTimeEnvdInitRetriesTransientHTTPError(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < envdInitMaxAttempts {
			http.Error(w, "envd warming up", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	u, err := neturl.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationComponentEnvdVersion: "0.2.0",
			constants.MasterAnnotationCreateTimeEnvVars:    `{"SESSION_ID":"user-session-test"}`,
		},
	}
	sandBox := &cubeboxstore.CubeBox{IP: "127.0.0.1"}

	l := &local{envdHTTPClient: server.Client(), envdInitPort: port}
	if err := l.doCreateTimeEnvdInit(context.Background(), req, sandBox); err != nil {
		t.Fatalf("doCreateTimeEnvdInit err=%v", err)
	}
	assert.Equal(t, envdInitMaxAttempts, callCount)
}

func TestDoCreateTimeEnvdInitRetriesTransportError(t *testing.T) {
	callCount := 0
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount < envdInitMaxAttempts {
				return nil, fmt.Errorf("dial tcp %s: connection refused", r.URL.Host)
			}
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Request:    r,
			}, nil
		}),
	}

	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationComponentEnvdVersion: "0.2.0",
			constants.MasterAnnotationCreateTimeEnvVars:    `{"SESSION_ID":"user-session-test"}`,
		},
	}
	sandBox := &cubeboxstore.CubeBox{IP: "127.0.0.1"}

	l := &local{envdHTTPClient: client, envdInitPort: 49983}
	if err := l.doCreateTimeEnvdInit(context.Background(), req, sandBox); err != nil {
		t.Fatalf("doCreateTimeEnvdInit err=%v", err)
	}
	assert.Equal(t, envdInitMaxAttempts, callCount)
}

func TestDoCreateTimeEnvdInitFallsBackWithoutEnvdSupportAnnotation(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/init" {
			t.Fatalf("path=%q, want /init", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	u, err := neturl.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationCreateTimeEnvVars: `{"SESSION_ID":"user-session-test"}`,
		},
	}
	sandBox := &cubeboxstore.CubeBox{IP: "127.0.0.1"}

	l := &local{envdHTTPClient: server.Client(), envdInitPort: port}
	if err := l.doCreateTimeEnvdInit(context.Background(), req, sandBox); err != nil {
		t.Fatalf("doCreateTimeEnvdInit err=%v", err)
	}
	if !called {
		t.Fatal("expected missing envd support annotation to still issue envd init request")
	}
}

func TestDoCreateTimeEnvdInitFailsWithoutEnvdSupportAnnotationWhenEnvdUnavailable(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial tcp %s: connection refused", r.URL.Host)
		}),
	}
	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationCreateTimeEnvVars: `{"SESSION_ID":"user-session-test"}`,
		},
	}
	sandBox := &cubeboxstore.CubeBox{IP: "127.0.0.1"}

	l := &local{envdHTTPClient: client, envdInitPort: 49983}
	retErr := l.doCreateTimeEnvdInit(context.Background(), req, sandBox)
	if retErr == nil {
		t.Fatal("expected init failure when envd init cannot be reached without envd support annotation")
	}
	errInfo, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_ExecCommandInSandboxFailed, errInfo.Code())
	assert.Contains(t, errInfo.Message(), "connection refused")
}

func TestProbe(t *testing.T) {
	testPort := 7997
	testHost := "127.0.0.1"
	list, err := net.Listen("tcp", fmt.Sprintf("%s:%d", testHost, testPort))
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer list.Close()

	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        100,
			PeriodMs:         10,
			SuccessThreshold: 0,
			FailureThreshold: 0,
			ProbeHandler: &cubebox.ProbeHandler{
				TcpSocket: &cubebox.TCPSocketAction{
					Port: int32(testPort),
				},
			},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{}
	createInfo.NetworkInfo = &proto.ShimNetReq{
		Interfaces: []*proto.Interface{
			{
				IPAddr: net.ParseIP("127.0.0.1"),
			},
		},
	}
	ci := &cubeboxstore.Container{
		ExitCh: make(chan containerd.ExitStatus),
		IP:     "127.0.0.1",
	}
	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	e, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_Success, e.Code())
	metrics := createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.Nil(t, metrics[0].Error())
}

func TestProbeReqTimeout(t *testing.T) {
	testPort := 7998
	testHost := "127.0.0.1"
	list, err := net.Listen("tcp", fmt.Sprintf("%s:%d", testHost, testPort))
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer list.Close()

	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        50,
			PeriodMs:         10,
			SuccessThreshold: 10,
			FailureThreshold: 1,
			ProbeHandler: &cubebox.ProbeHandler{
				TcpSocket: &cubebox.TCPSocketAction{
					Port: int32(testPort),
				},
			},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{}
	createInfo.NetworkInfo = &proto.ShimNetReq{
		Interfaces: []*proto.Interface{
			{
				IPAddr: net.ParseIP(testHost),
			},
		},
	}
	exitCh := make(chan containerd.ExitStatus, 1)
	ci := &cubeboxstore.Container{
		ExitCh: func() <-chan containerd.ExitStatus { return exitCh }(),
		IP:     testHost,
	}

	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	e, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, e.Code())
	metrics := createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.NotNil(t, metrics[0].Error())

	ctx1, cancel1 := context.WithTimeout(context.Background(), tSum)
	defer cancel1()
	exitCh <- *containerd.NewExitStatus(containerd.UnknownExitStatus, time.Now(), fmt.Errorf("nil"))
	retErr = l.doProbe(ctx1, cnt, ci)
	e, _ = ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, e.Code())
	metrics = createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.NotNil(t, metrics[0].Error())

	ctx2, cancel2 := context.WithTimeout(context.Background(), tSum)
	defer cancel2()
	exitCh <- *containerd.NewExitStatus(containerd.UnknownExitStatus, time.Now(), nil)
	retErr = l.doProbe(ctx2, cnt, ci)
	e, _ = ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, e.Code())
	metrics = createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.NotNil(t, metrics[0].Error())

	ctx3, cancel3 := context.WithTimeout(context.Background(), tSum)
	defer cancel3()
	cnt.Probe.ProbeHandler.TcpSocket.Port = 7778
	retErr = l.doProbe(ctx3, cnt, ci)
	e, _ = ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, e.Code())
	metrics = createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.NotNil(t, metrics[0].Error())
}

func TestHttpProbe(t *testing.T) {
	testPort := 7999
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: "localhost:7999", Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        100,
			PeriodMs:         10,
			SuccessThreshold: 1,
			FailureThreshold: 10,
			ProbeHandler: &cubebox.ProbeHandler{
				HttpGet: &cubebox.HTTPGetAction{
					Port: int32(testPort),
					Path: pointer.String("/healthz"),
					HttpHeaders: []*cubebox.HTTPHeader{
						{
							Name:  pointer.String("Content-Type"),
							Value: pointer.String("application/json"),
						},
					},
				},
			},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{}
	createInfo.NetworkInfo = &proto.ShimNetReq{
		Interfaces: []*proto.Interface{
			{
				IPAddr: net.ParseIP("127.0.0.1"),
			},
		},
	}
	ci := &cubeboxstore.Container{
		ExitCh: make(chan containerd.ExitStatus),
		IP:     "127.0.0.1",
	}
	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	e, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_Success, e.Code())
	metrics := createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.Nil(t, metrics[0].Error())
}

func TestProbeTimeoutMsDefault(t *testing.T) {
	testPort := 8001
	testHost := "127.0.0.1"
	list, err := net.Listen("tcp", fmt.Sprintf("%s:%d", testHost, testPort))
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer list.Close()

	tests := []struct {
		name           string
		probeTimeoutMs int32
		expectDefault  bool
	}{
		{
			name:           "ProbeTimeoutMs为0时使用默认值",
			probeTimeoutMs: 0,
			expectDefault:  true,
		},
		{
			name:           "ProbeTimeoutMs为负数时使用默认值",
			probeTimeoutMs: -1,
			expectDefault:  true,
		},
		{
			name:           "ProbeTimeoutMs为5时使用默认值",
			probeTimeoutMs: 5,
			expectDefault:  true,
		},
		{
			name:           "ProbeTimeoutMs为6时使用设定值",
			probeTimeoutMs: 6,
			expectDefault:  false,
		},
		{
			name:           "ProbeTimeoutMs为正常值时使用设定值",
			probeTimeoutMs: 200,
			expectDefault:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cnt := &cubebox.ContainerConfig{
				Probe: &cubebox.Probe{
					InitialDelayMs:   0,
					TimeoutMs:        100,
					PeriodMs:         10,
					SuccessThreshold: 1,
					FailureThreshold: 1,
					ProbeTimeoutMs:   tt.probeTimeoutMs,
					ProbeHandler: &cubebox.ProbeHandler{
						TcpSocket: &cubebox.TCPSocketAction{
							Port: int32(testPort),
						},
					},
				},
			}
			req := &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					cnt,
				},
			}
			createInfo := &workflow.CreateContext{}
			createInfo.NetworkInfo = &proto.ShimNetReq{
				Interfaces: []*proto.Interface{
					{
						IPAddr: net.ParseIP(testHost),
					},
				},
			}
			ci := &cubeboxstore.Container{
				ExitCh: make(chan containerd.ExitStatus),
				IP:     testHost,
			}
			tSum := getProbeDuration(req)
			ctx, cancel := context.WithTimeout(context.Background(), tSum)
			defer cancel()
			ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

			l := &local{}
			retErr := l.doProbe(ctx, cnt, ci)
			e, _ := ret.FromError(retErr)
			assert.Equal(t, errorcode.ErrorCode_Success, e.Code())

			if tt.expectDefault {
				assert.Equal(t, int32(100), cnt.Probe.ProbeTimeoutMs, "ProbeTimeoutMs应该被设置为默认值100")
			} else {
				assert.Equal(t, tt.probeTimeoutMs, cnt.Probe.ProbeTimeoutMs, "ProbeTimeoutMs应该保持原值")
			}

			metrics := createInfo.GetMetric()
			require.LessOrEqual(t, 1, len(metrics), "at least one metric")
			assert.Nil(t, metrics[0].Error())
		})
	}
}

func TestProbeTimeoutMsWithHttpProbe(t *testing.T) {
	testPort := 8002
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: fmt.Sprintf("localhost:%d", testPort), Handler: mux}
	go func() {
		srv.ListenAndServe()
	}()
	defer srv.Close()

	time.Sleep(100 * time.Millisecond)

	cnt := &cubebox.ContainerConfig{
		Probe: &cubebox.Probe{
			InitialDelayMs:   0,
			TimeoutMs:        100,
			PeriodMs:         10,
			SuccessThreshold: 1,
			FailureThreshold: 1,
			ProbeTimeoutMs:   50,
			ProbeHandler: &cubebox.ProbeHandler{
				HttpGet: &cubebox.HTTPGetAction{
					Port: int32(testPort),
					Path: pointer.String("/health"),
				},
			},
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			cnt,
		},
	}
	createInfo := &workflow.CreateContext{}
	createInfo.NetworkInfo = &proto.ShimNetReq{
		Interfaces: []*proto.Interface{
			{
				IPAddr: net.ParseIP("127.0.0.1"),
			},
		},
	}
	ci := &cubeboxstore.Container{
		ExitCh: make(chan containerd.ExitStatus),
		IP:     "127.0.0.1",
	}
	tSum := getProbeDuration(req)
	ctx, cancel := context.WithTimeout(context.Background(), tSum)
	defer cancel()
	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	l := &local{}
	retErr := l.doProbe(ctx, cnt, ci)
	e, _ := ret.FromError(retErr)
	assert.Equal(t, errorcode.ErrorCode_Success, e.Code())
	assert.Equal(t, int32(50), cnt.Probe.ProbeTimeoutMs, "ProbeTimeoutMs应该保持设定值50")
	metrics := createInfo.GetMetric()
	require.LessOrEqual(t, 1, len(metrics), "at least one metric")
	assert.Nil(t, metrics[0].Error())
}

func TestProbeTimeoutMsWithPing(t *testing.T) {
	testHost := "127.0.0.1"

	tests := []struct {
		name           string
		probeTimeoutMs int32
		udp            bool
		expectDefault  bool
	}{
		{
			name:           "Ping探测ProbeTimeoutMs为0时使用默认值",
			probeTimeoutMs: 0,
			udp:            false,
			expectDefault:  true,
		},
		{
			name:           "Ping探测ProbeTimeoutMs为正常值",
			probeTimeoutMs: 150,
			udp:            false,
			expectDefault:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cnt := &cubebox.ContainerConfig{
				Probe: &cubebox.Probe{
					InitialDelayMs:   0,
					TimeoutMs:        500,
					PeriodMs:         10,
					SuccessThreshold: 1,
					FailureThreshold: 1,
					ProbeTimeoutMs:   tt.probeTimeoutMs,
					ProbeHandler: &cubebox.ProbeHandler{
						Ping: &cubebox.PingAction{
							Udp: tt.udp,
						},
					},
				},
			}
			req := &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					cnt,
				},
			}
			createInfo := &workflow.CreateContext{}
			createInfo.NetworkInfo = &proto.ShimNetReq{
				Interfaces: []*proto.Interface{
					{
						IPAddr: net.ParseIP(testHost),
					},
				},
			}
			ci := &cubeboxstore.Container{
				ExitCh: make(chan containerd.ExitStatus),
				IP:     testHost,
			}
			tSum := getProbeDuration(req)
			ctx, cancel := context.WithTimeout(context.Background(), tSum)
			defer cancel()
			ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

			l := &local{}
			retErr := l.doProbe(ctx, cnt, ci)
			e, _ := ret.FromError(retErr)
			assert.Equal(t, errorcode.ErrorCode_Success, e.Code())

			if tt.expectDefault {
				assert.Equal(t, int32(100), cnt.Probe.ProbeTimeoutMs, "ProbeTimeoutMs应该被设置为默认值100")
			} else {
				assert.Equal(t, tt.probeTimeoutMs, cnt.Probe.ProbeTimeoutMs, "ProbeTimeoutMs应该保持原值")
			}

			metrics := createInfo.GetMetric()
			require.LessOrEqual(t, 1, len(metrics), "at least one metric")
			assert.Nil(t, metrics[0].Error())
		})
	}
}

func TestProbeTimeoutFailure(t *testing.T) {
	tests := []struct {
		name           string
		setupProbe     func() *cubebox.ContainerConfig
		expectedErrMsg string
	}{
		{
			name: "TCP探测超时失败_端口不存在",
			setupProbe: func() *cubebox.ContainerConfig {
				return &cubebox.ContainerConfig{
					Probe: &cubebox.Probe{
						InitialDelayMs:   0,
						TimeoutMs:        50,
						PeriodMs:         10,
						SuccessThreshold: 1,
						FailureThreshold: 1,
						ProbeTimeoutMs:   10,
						ProbeHandler: &cubebox.ProbeHandler{
							TcpSocket: &cubebox.TCPSocketAction{
								Port: 19999,
							},
						},
					},
				}
			},
			expectedErrMsg: "connection refused",
		},
		{
			name: "HTTP探测超时失败_端口不存在",
			setupProbe: func() *cubebox.ContainerConfig {
				return &cubebox.ContainerConfig{
					Probe: &cubebox.Probe{
						InitialDelayMs:   0,
						TimeoutMs:        50,
						PeriodMs:         10,
						SuccessThreshold: 1,
						FailureThreshold: 1,
						ProbeTimeoutMs:   10,
						ProbeHandler: &cubebox.ProbeHandler{
							HttpGet: &cubebox.HTTPGetAction{
								Port: 19998,
								Path: pointer.String("/health"),
							},
						},
					},
				}
			},
			expectedErrMsg: "connection refused",
		},
		{
			name: "探测超时_FailureThreshold达到上限",
			setupProbe: func() *cubebox.ContainerConfig {
				return &cubebox.ContainerConfig{
					Probe: &cubebox.Probe{
						InitialDelayMs:   0,
						TimeoutMs:        100,
						PeriodMs:         10,
						SuccessThreshold: 1,
						FailureThreshold: 2,
						ProbeTimeoutMs:   5,
						ProbeHandler: &cubebox.ProbeHandler{
							TcpSocket: &cubebox.TCPSocketAction{
								Port: 19997,
							},
						},
					},
				}
			},
			expectedErrMsg: "connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cnt := tt.setupProbe()
			req := &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					cnt,
				},
			}
			createInfo := &workflow.CreateContext{}
			createInfo.NetworkInfo = &proto.ShimNetReq{
				Interfaces: []*proto.Interface{
					{
						IPAddr: net.ParseIP("127.0.0.1"),
					},
				},
			}
			ci := &cubeboxstore.Container{
				ExitCh: make(chan containerd.ExitStatus),
				IP:     "127.0.0.1",
			}
			tSum := getProbeDuration(req)
			ctx, cancel := context.WithTimeout(context.Background(), tSum)
			defer cancel()
			ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

			l := &local{}
			retErr := l.doProbe(ctx, cnt, ci)
			e, _ := ret.FromError(retErr)

			assert.Equal(t, errorcode.ErrorCode_PortBindingFailed, e.Code())

			assert.Contains(t, e.Message(), tt.expectedErrMsg)

			metrics := createInfo.GetMetric()
			require.LessOrEqual(t, 1, len(metrics), "at least one metric")
			assert.NotNil(t, metrics[0].Error())
		})
	}
}

func TestProbeConcurrent(t *testing.T) {

	basePort := 9000
	serverCount := 10
	servers := make([]*http.Server, serverCount)

	for i := 0; i < serverCount; i++ {
		port := basePort + i
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Addr: fmt.Sprintf("localhost:%d", port), Handler: mux}
		servers[i] = srv
		go func() {
			srv.ListenAndServe()
		}()
		defer srv.Close()
	}

	time.Sleep(200 * time.Millisecond)

	concurrentCount := 20
	errCh := make(chan error, concurrentCount)

	for i := 0; i < concurrentCount; i++ {
		go func(index int) {

			port := basePort + (index % serverCount)

			cnt := &cubebox.ContainerConfig{
				Probe: &cubebox.Probe{
					InitialDelayMs:   0,
					TimeoutMs:        200,
					PeriodMs:         10,
					SuccessThreshold: 1,
					FailureThreshold: 1,
					ProbeTimeoutMs:   50,
					ProbeHandler: &cubebox.ProbeHandler{
						HttpGet: &cubebox.HTTPGetAction{
							Port: int32(port),
							Path: pointer.String("/health"),
						},
					},
				},
			}
			req := &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					cnt,
				},
			}
			createInfo := &workflow.CreateContext{}
			createInfo.NetworkInfo = &proto.ShimNetReq{
				Interfaces: []*proto.Interface{
					{
						IPAddr: net.ParseIP("127.0.0.1"),
					},
				},
			}
			ci := &cubeboxstore.Container{
				ExitCh: make(chan containerd.ExitStatus),
				IP:     "127.0.0.1",
			}
			tSum := getProbeDuration(req)
			ctx, cancel := context.WithTimeout(context.Background(), tSum)
			defer cancel()
			ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

			l := &local{}
			retErr := l.doProbe(ctx, cnt, ci)
			errCh <- retErr
		}(i)
	}

	successCount := 0
	for i := 0; i < concurrentCount; i++ {
		retErr := <-errCh
		if retErr == nil {
			successCount++
		} else {
			e, _ := ret.FromError(retErr)
			t.Logf("probe %d failed: %v", i, e.Message())
		}
	}

	successRate := float64(successCount) / float64(concurrentCount)
	assert.GreaterOrEqual(t, successRate, 0.9, "并发探测成功率应该大于等于90%%")
	t.Logf("并发探测测试完成: 总数=%d, 成功=%d, 成功率=%.2f%%",
		concurrentCount, successCount, successRate*100)
}

func TestProbeConcurrentMixed(t *testing.T) {

	httpPort := 9100
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpSrv := &http.Server{Addr: fmt.Sprintf("localhost:%d", httpPort), Handler: mux}
	go func() {
		httpSrv.ListenAndServe()
	}()
	defer httpSrv.Close()

	tcpPort := 9101
	tcpListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tcpPort))
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	defer tcpListener.Close()

	time.Sleep(200 * time.Millisecond)

	concurrentCount := 15
	errCh := make(chan error, concurrentCount)

	for i := 0; i < concurrentCount; i++ {
		go func(index int) {
			var cnt *cubebox.ContainerConfig

			switch index % 3 {
			case 0:
				cnt = &cubebox.ContainerConfig{
					Probe: &cubebox.Probe{
						InitialDelayMs:   0,
						TimeoutMs:        200,
						PeriodMs:         10,
						SuccessThreshold: 1,
						FailureThreshold: 1,
						ProbeTimeoutMs:   50,
						ProbeHandler: &cubebox.ProbeHandler{
							HttpGet: &cubebox.HTTPGetAction{
								Port: int32(httpPort),
								Path: pointer.String("/health"),
							},
						},
					},
				}
			case 1:
				cnt = &cubebox.ContainerConfig{
					Probe: &cubebox.Probe{
						InitialDelayMs:   0,
						TimeoutMs:        200,
						PeriodMs:         10,
						SuccessThreshold: 1,
						FailureThreshold: 1,
						ProbeTimeoutMs:   50,
						ProbeHandler: &cubebox.ProbeHandler{
							TcpSocket: &cubebox.TCPSocketAction{
								Port: int32(tcpPort),
							},
						},
					},
				}
			case 2:
				cnt = &cubebox.ContainerConfig{
					Probe: &cubebox.Probe{
						InitialDelayMs:   0,
						TimeoutMs:        200,
						PeriodMs:         10,
						SuccessThreshold: 1,
						FailureThreshold: 1,
						ProbeTimeoutMs:   50,
						ProbeHandler: &cubebox.ProbeHandler{
							Ping: &cubebox.PingAction{
								Udp: false,
							},
						},
					},
				}
			}

			req := &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					cnt,
				},
			}
			createInfo := &workflow.CreateContext{}
			createInfo.NetworkInfo = &proto.ShimNetReq{
				Interfaces: []*proto.Interface{
					{
						IPAddr: net.ParseIP("127.0.0.1"),
					},
				},
			}
			ci := &cubeboxstore.Container{
				ExitCh: make(chan containerd.ExitStatus),
				IP:     "127.0.0.1",
			}
			tSum := getProbeDuration(req)
			ctx, cancel := context.WithTimeout(context.Background(), tSum)
			defer cancel()
			ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

			l := &local{}
			retErr := l.doProbe(ctx, cnt, ci)
			errCh <- retErr
		}(i)
	}

	successCount := 0
	failCount := 0
	for i := 0; i < concurrentCount; i++ {
		retErr := <-errCh
		if retErr == nil {
			successCount++
		} else {
			failCount++
			e, _ := ret.FromError(retErr)
			t.Logf("mixed probe %d failed: %v", i, e.Message())
		}
	}

	successRate := float64(successCount) / float64(concurrentCount)
	assert.GreaterOrEqual(t, successRate, 0.8, "混合并发探测成功率应该大于等于80%%")
	t.Logf("混合并发探测测试完成: 总数=%d, 成功=%d, 失败=%d, 成功率=%.2f%%",
		concurrentCount, successCount, failCount, successRate*100)
}
