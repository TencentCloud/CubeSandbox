// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	api "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

type fakeTerminalStream struct {
	mu       sync.Mutex
	received []*api.TerminalMessage
	sent     []*api.TerminalMessage
}

func (f *fakeTerminalStream) Send(message *api.TerminalMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, message)
	return nil
}

func (f *fakeTerminalStream) Recv() (*api.TerminalMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return nil, io.EOF
	}
	message := f.received[0]
	f.received = f.received[1:]
	return message, nil
}

type fakeTerminalProcess struct {
	cols uint32
	rows uint32
}

func (f *fakeTerminalProcess) Resize(_ context.Context, cols, rows uint32) error {
	f.cols = cols
	f.rows = rows
	return nil
}

func TestValidateTerminalOpen(t *testing.T) {
	for name, open := range map[string]*api.TerminalOpen{
		"missing open":      nil,
		"missing sandbox":   {ContainerId: "container"},
		"missing container": {SandboxId: "sandbox"},
		"partial size":      {SandboxId: "sandbox", ContainerId: "container", Cols: 80},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateTerminalOpen(open); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if err := validateTerminalOpen(&api.TerminalOpen{SandboxId: "sandbox", ContainerId: "container", Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("valid open rejected: %v", err)
	}
}

func TestTerminalCleanupContextPreservesNamespace(t *testing.T) {
	ctx, cancel := terminalCleanupContext("sandbox-namespace")
	defer cancel()

	if namespace, ok := namespaces.Namespace(ctx); !ok || namespace != "sandbox-namespace" {
		t.Fatalf("cleanup namespace = %q, %v; want sandbox-namespace, true", namespace, ok)
	}
}

func TestTerminalEnvDefaultsTermAndPreservesExplicitValue(t *testing.T) {
	input := []string{"PATH=/usr/bin"}
	withDefault := terminalEnv(input)
	if got := withDefault[len(withDefault)-1]; got != "TERM=xterm-256color" {
		t.Fatalf("default terminal env = %q, want TERM=xterm-256color", got)
	}
	if len(input) != 1 {
		t.Fatalf("terminalEnv mutated its input: %+v", input)
	}

	explicit := terminalEnv([]string{"TERM=screen-256color", "LANG=C.UTF-8"})
	if len(explicit) != 2 || explicit[0] != "TERM=screen-256color" {
		t.Fatalf("explicit terminal env was not preserved: %+v", explicit)
	}
}

func TestReceiveTerminalInputWritesResizesAndCloses(t *testing.T) {
	stream := &fakeTerminalStream{received: []*api.TerminalMessage{
		{Message: &api.TerminalMessage_Input{Input: []byte("echo ok\n")}},
		{Message: &api.TerminalMessage_Resize{Resize: &api.TerminalResize{Cols: 132, Rows: 43}}},
		{Message: &api.TerminalMessage_Close{Close: &api.TerminalClose{}}},
	}}
	process := &fakeTerminalProcess{}
	reader, writer := io.Pipe()
	defer reader.Close()
	readDone := make(chan []byte, 1)
	go func() {
		payload := make([]byte, len("echo ok\n"))
		_, _ = io.ReadFull(reader, payload)
		readDone <- payload
	}()

	err := receiveTerminalInput(context.Background(), stream, process, writer)
	if !errors.Is(err, errTerminalClientClosed) {
		t.Fatalf("expected client close, got %v", err)
	}
	if payload := <-readDone; !bytes.Equal(payload, []byte("echo ok\n")) {
		t.Fatalf("unexpected stdin payload %q", payload)
	}
	if process.cols != 132 || process.rows != 43 {
		t.Fatalf("resize = %dx%d, want 132x43", process.cols, process.rows)
	}
}

func TestCopyTerminalOutputSendsBinaryPayload(t *testing.T) {
	stream := &fakeTerminalStream{}
	sender := &terminalStreamSender{stream: stream}
	err := copyTerminalOutput(bytes.NewBufferString("\x1b[32mok\x1b[0m"), sender)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	if len(stream.sent) != 1 || string(stream.sent[0].GetOutput()) != "\x1b[32mok\x1b[0m" {
		t.Fatalf("unexpected output messages: %+v", stream.sent)
	}
}
