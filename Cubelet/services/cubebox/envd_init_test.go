// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// startEnvdStub starts an httptest server acting as the guest envd /init
// endpoint, repoints envdServerPort at it, and returns the host the sandbox IP
// should be set to. The original port is restored when the test ends.
func startEnvdStub(t *testing.T, handler http.HandlerFunc) (server *httptest.Server, host string) {
	t.Helper()
	server = httptest.NewServer(handler)
	t.Cleanup(server.Close)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	h, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split stub host: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse stub port: %v", err)
	}

	orig := envdServerPort
	envdServerPort = port
	t.Cleanup(func() { envdServerPort = orig })

	return server, h
}

func newEnvSandbox(annotationValue, ip string) (*cubeboxstore.CubeBox, *cubeboxstore.Container) {
	annotations := map[string]string{}
	if annotationValue != "" {
		annotations[createEnvVarsAnnotation] = annotationValue
	}
	sb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			SandboxID:   "sb-test",
			Annotations: annotations,
		},
		IP: ip,
	}
	ci := &cubeboxstore.Container{IP: ip}
	return sb, ci
}

func TestSyncCreateEnvToEnvdNoAnnotationIsNoop(t *testing.T) {
	var called int32
	_, host := startEnvdStub(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	sb, ci := newEnvSandbox("", host)
	l := &local{}
	if err := l.syncCreateEnvToEnvd(context.Background(), sb, ci); err != nil {
		t.Fatalf("expected no-op nil, got %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatalf("envd /init must not be called without the annotation")
	}
}

func TestSyncCreateEnvToEnvdInvalidJSON(t *testing.T) {
	sb, ci := newEnvSandbox("not-json", "127.0.0.1")
	l := &local{}
	if err := l.syncCreateEnvToEnvd(context.Background(), sb, ci); err == nil {
		t.Fatal("expected error for invalid env_vars annotation JSON")
	}
}

func TestSyncCreateEnvToEnvdEmptyIP(t *testing.T) {
	sb, ci := newEnvSandbox(`{"FOO":"bar"}`, "")
	l := &local{}
	if err := l.syncCreateEnvToEnvd(context.Background(), sb, ci); err == nil {
		t.Fatal("expected error when sandbox IP is empty")
	}
}

func TestSyncCreateEnvToEnvdSuccessSendsEnvVars(t *testing.T) {
	var got envdInitBody
	_, host := startEnvdStub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	})

	sb, ci := newEnvSandbox(`{"FOO":"bar","TOKEN":"x"}`, host)
	l := &local{}
	if err := l.syncCreateEnvToEnvd(context.Background(), sb, ci); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if got.EnvVars["FOO"] != "bar" || got.EnvVars["TOKEN"] != "x" {
		t.Fatalf("envd /init received unexpected envVars: %+v", got.EnvVars)
	}
	if got.Timestamp == "" {
		t.Fatal("envd /init request must carry a timestamp")
	}
}

func TestSyncCreateEnvToEnvdRetriesThenSucceeds(t *testing.T) {
	var attempts int32
	_, host := startEnvdStub(t, func(w http.ResponseWriter, r *http.Request) {
		// Fail the first attempt to exercise the retry loop, then succeed.
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	sb, ci := newEnvSandbox(`{"FOO":"bar"}`, host)
	l := &local{}
	if err := l.syncCreateEnvToEnvd(context.Background(), sb, ci); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if atomic.LoadInt32(&attempts) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempts)
	}
}
