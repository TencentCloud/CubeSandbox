// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func commandOutputFrame(stream, text string) []byte {
	return connectEnvelope(0, fmt.Sprintf(`{"event":{"data":{%q:%q}}}`,
		stream, base64.StdEncoding.EncodeToString([]byte(text))))
}

func TestCommandsRunStreamsOutputBeforeCompletion(t *testing.T) {
	stdoutReceived := make(chan struct{})
	stderrReceived := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(commandOutputFrame("stdout", "building "))
		w.(http.Flusher).Flush()
		// Do not send completion until the caller has consumed live output.
		// A parser that buffers callbacks until EOF cannot pass this handshake.
		select {
		case <-stdoutReceived:
		case <-r.Context().Done():
			return
		}
		w.Write(commandOutputFrame("stderr", "warning\n"))
		w.(http.Flusher).Flush()
		select {
		case <-stderrReceived:
		case <-r.Context().Done():
			return
		}
		w.Write(commandOutputFrame("stdout", "done\n"))
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":7}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()
	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test"})
	defer client.Close()
	sb := &Sandbox{client: client, SandboxID: "sb-stream"}

	var events []string
	result, err := sb.Commands().Run(ctx, "go test ./...", CommandOptions{
		OnStdout: func(chunk string) {
			events = append(events, "stdout:"+chunk)
			if chunk == "building " {
				close(stdoutReceived)
			}
		},
		OnStderr: func(chunk string) {
			events = append(events, "stderr:"+chunk)
			close(stderrReceived)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"stdout:building ", "stderr:warning\n", "stdout:done\n"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("callbacks=%q, want %q", events, want)
	}
	if result.Stdout != "building done\n" || result.Stderr != "warning\n" || result.ExitCode != 7 {
		t.Fatalf("result=%+v; callbacks must preserve captured output and non-zero exit codes", result)
	}
}

func TestProcessOutputCallbacksAreOptional(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout bool
		stderr bool
	}{
		{name: "neither"},
		{name: "stdout only", stdout: true},
		{name: "stderr only", stderr: true},
		{name: "both", stdout: true, stderr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			var onStdout, onStderr func(string)
			if tc.stdout {
				onStdout = func(chunk string) { out.WriteString(chunk) }
			}
			if tc.stderr {
				onStderr = func(chunk string) { errOut.WriteString(chunk) }
			}
			wire := append(commandOutputFrame("stdout", "first\nsecond\x00"), commandOutputFrame("stderr", "诊断\n")...)
			wire = append(wire, connectEnvelope(0, `{"event":{"end":{"exitCode":0}}}`)...)
			wire = append(wire, connectEnvelope(connectEndStreamFlag, `{}`)...)
			result, err := parseProcessStartStream(strings.NewReader(string(wire)), onStdout, onStderr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if result.Stdout != "first\nsecond\x00" || result.Stderr != "诊断\n" || result.ExitCode != 0 {
				t.Fatalf("result=%+v", result)
			}
			if tc.stdout && out.String() != result.Stdout {
				t.Fatalf("stdout callback=%q, result=%q", out.String(), result.Stdout)
			}
			if tc.stderr && errOut.String() != result.Stderr {
				t.Fatalf("stderr callback=%q, result=%q", errOut.String(), result.Stderr)
			}
		})
	}
}

func TestProcessOutputCallbacksPreserveStreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail []byte
		want string
	}{
		{name: "missing end", want: "without EndEvent"},
		{name: "invalid output", tail: connectEnvelope(0, `{"event":{"data":{"stderr":"!"}}}`), want: "decode stderr"},
		{name: "protocol error", tail: connectEnvelope(connectEndStreamFlag, `{"error":{"code":"internal","message":"stream failed"}}`), want: "stream failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := append(commandOutputFrame("stdout", "partial output"), tc.tail...)
			var received string
			result, err := parseProcessStartStream(strings.NewReader(string(wire)), func(chunk string) {
				received += chunk
			}, func(chunk string) {
				t.Errorf("unexpected stderr callback: %q", chunk)
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) || result != nil {
				t.Fatalf("result=%+v, err=%v; want %q", result, err, tc.want)
			}
			if received != "partial output" {
				t.Fatalf("callback=%q; valid output must arrive before the error", received)
			}
		})
	}
}

func TestCommandsRunCancellationWithOutputCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(commandOutputFrame("stdout", "started\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test"})
	defer client.Close()
	sb := &Sandbox{client: client, SandboxID: "sb-cancel"}
	var output string
	result, err := sb.Commands().Run(ctx, "long-running-command", CommandOptions{
		OnStdout: func(chunk string) {
			output += chunk
			cancel()
		},
	})
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("result=%+v, err=%v; want context.Canceled", result, err)
	}
	if output != "started\n" {
		t.Fatalf("stdout=%q", output)
	}
}
