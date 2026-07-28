// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestEnvdPTYClientUsesSandboxProxyContract(t *testing.T) {
	var paths []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Host != "49983-sandbox-a.cube.test" {
			t.Errorf("Host = %q", r.Host)
		}
		if r.Header.Get("Authorization") != envdAuth {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Connect-Protocol-Version") != envdConnectProtocolVersion {
			t.Errorf("Connect-Protocol-Version = %q", r.Header.Get("Connect-Protocol-Version"))
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if r.URL.Path == "/process.Process/Start" {
			if r.Header.Get("Content-Type") != connectJSON {
				t.Errorf("start Content-Type = %q", r.Header.Get("Content-Type"))
			}
			if len(body) < 5 || int(binary.BigEndian.Uint32(body[1:5])) != len(body)-5 {
				t.Errorf("invalid Connect envelope: %x", body)
				return
			}
			var request map[string]interface{}
			if err := json.Unmarshal(body[5:], &request); err != nil {
				t.Errorf("decode start request: %v", err)
				return
			}
			process, _ := request["process"].(map[string]interface{})
			if process["cmd"] != "/bin/bash" {
				t.Errorf("start command = %v", process["cmd"])
			}
			w.Header().Set("Content-Type", connectJSON)
			writeEnvdTestFrame(t, w, 0, map[string]interface{}{
				"event": map[string]interface{}{"start": map[string]interface{}{"pid": 42}},
			})
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unary Content-Type = %q", r.Header.Get("Content-Type"))
		}
	}))
	defer proxy.Close()

	client := NewEnvdPTYClient(proxy.Client(), proxy.URL, "sandbox-a", "cube.test")
	stream, err := client.Start(context.Background(), EnvdPTYStartOptions{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	event, err := ReadEnvdPTYEvent(stream)
	_ = stream.Close()
	if err != nil || !event.Started || event.PID != 42 {
		t.Fatalf("start event = %+v, %v", event, err)
	}
	if err := client.SendInput(context.Background(), 42, []byte("echo ok\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := client.Resize(context.Background(), 42, 30, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := client.Kill(context.Background(), 42); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	want := []string{
		"/process.Process/Start",
		"/process.Process/SendInput",
		"/process.Process/Update",
		"/process.Process/SendSignal",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestReadEnvdPTYEvent(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	writeEnvdTestFrame(t, stream, 0, map[string]interface{}{
		"event": map[string]interface{}{"start": map[string]interface{}{"pid": "42"}},
	})
	writeEnvdTestFrame(t, stream, 0, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"pty": base64.StdEncoding.EncodeToString([]byte("hello"))}},
	})
	writeEnvdTestFrame(t, stream, 0, map[string]interface{}{
		"event": map[string]interface{}{"end": map[string]interface{}{"exitCode": 0}},
	})

	started, err := ReadEnvdPTYEvent(stream)
	if err != nil || !started.Started || started.PID != 42 {
		t.Fatalf("start event = %+v, err = %v", started, err)
	}
	output, err := ReadEnvdPTYEvent(stream)
	if err != nil || string(output.Output) != "hello" {
		t.Fatalf("output event = %+v, err = %v", output, err)
	}
	exited, err := ReadEnvdPTYEvent(stream)
	if err != nil || !exited.Exited || exited.ExitCode == nil || *exited.ExitCode != 0 {
		t.Fatalf("exit event = %+v, err = %v", exited, err)
	}
}

func TestReadEnvdPTYEndStreamError(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	writeEnvdTestFrame(t, stream, envdConnectEndStreamFlag, map[string]interface{}{
		"error": map[string]interface{}{"code": "internal", "message": "failed"},
	})
	if _, err := ReadEnvdPTYEvent(stream); err == nil {
		t.Fatal("end-stream error should be returned")
	}
}

func TestEnvdExitCodeFromStatus(t *testing.T) {
	for status, want := range map[string]int{
		"exit status 7":          7,
		"exited with code 9":     9,
		"terminated by signal 2": 130,
		"exited":                 0,
	} {
		got, ok := envdExitCodeFromStatus(status)
		if !ok || got != want {
			t.Fatalf("envdExitCodeFromStatus(%q) = %d, %v; want %d", status, got, ok, want)
		}
	}
}

func writeEnvdTestFrame(t *testing.T, writer io.Writer, flags byte, value interface{}) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte{flags})
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	_, _ = writer.Write(header)
	_, _ = writer.Write(payload)
}
