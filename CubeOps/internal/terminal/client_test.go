// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"
	"strings"
	"testing"
)

func TestStartPTYReadsPIDAndOutput(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(1234)
		w.data("hello ")
		w.data("world")
		w.end(0)
	}

	c := NewClient("cube.app")
	stream, err := c.StartPTY(context.Background(), "sbx-abc", 24, 80)
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	if stream.PID != 1234 {
		t.Fatalf("PID = %d, want 1234", stream.PID)
	}

	var got strings.Builder
	for chunk := range stream.Output {
		got.Write(chunk)
	}
	<-stream.Done()

	if got.String() != "hello world" {
		t.Fatalf("output = %q, want %q", got.String(), "hello world")
	}
	exited, code, streamErr := stream.Result()
	if !exited || code != 0 || streamErr != nil {
		t.Fatalf("Result = (%v, %d, %v), want (true, 0, nil)", exited, code, streamErr)
	}
}

// cube-proxy routes by Host, so the header must be <port>-<sandboxID>.<domain>.
func TestStartPTYSetsProxyHost(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(1)
		w.end(0)
	}

	c := NewClient("cube.example")
	stream, err := c.StartPTY(context.Background(), "sbx-xyz", 24, 80)
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	for range stream.Output {
	}
	<-stream.Done()

	_, _, hosts, _ := f.snapshot()
	if len(hosts) == 0 || hosts[0] != "49983-sbx-xyz.cube.example" {
		t.Fatalf("Host = %v, want 49983-sbx-xyz.cube.example", hosts)
	}
}

func TestStartPTYPropagatesHTTPError(t *testing.T) {
	f := newFakeEnvd(t)
	f.startStatus = 500

	c := NewClient("cube.app")
	if _, err := c.StartPTY(context.Background(), "sbx-abc", 24, 80); err == nil {
		t.Fatal("StartPTY succeeded on HTTP 500")
	}
}

func TestStreamReportsNonZeroExit(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(7)
		w.end(130)
	}

	c := NewClient("cube.app")
	stream, err := c.StartPTY(context.Background(), "sbx-abc", 24, 80)
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	for range stream.Output {
	}
	<-stream.Done()

	exited, code, _ := stream.Result()
	if !exited || code != 130 {
		t.Fatalf("Result = (%v, %d), want (true, 130)", exited, code)
	}
}

// A stream that dies without an end event is an error, not a clean exit —
// the UI needs to tell the user the terminal broke.
func TestStreamReportsInterruptedTransport(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(9)
		w.data("partial")
		// Handler returns: the response body ends without an end frame.
	}

	c := NewClient("cube.app")
	stream, err := c.StartPTY(context.Background(), "sbx-abc", 24, 80)
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	for range stream.Output {
	}
	<-stream.Done()

	exited, _, streamErr := stream.Result()
	if exited {
		t.Fatal("stream reported a clean exit, want interruption")
	}
	if streamErr == nil {
		t.Fatal("Result error is nil, want an interruption error")
	}
}

func TestUnaryControlCalls(t *testing.T) {
	f := newFakeEnvd(t)
	c := NewClient("cube.app")
	ctx := context.Background()

	if err := c.SendInput(ctx, "sbx-abc", 42, []byte("ls -la\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := c.Resize(ctx, "sbx-abc", 42, 40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := c.Kill(ctx, "sbx-abc", 42); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	inputs, resizes, _, signals := f.snapshot()
	if len(inputs) != 1 || inputs[0] != "ls -la\n" {
		t.Fatalf("inputs = %q, want [\"ls -la\\n\"]", inputs)
	}
	if len(resizes) != 1 || resizes[0] != "40x120" {
		t.Fatalf("resizes = %q, want [\"40x120\"]", resizes)
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want 1", signals)
	}
}

// envd returns 415 when a unary endpoint receives a streaming content-type,
// so the unary path must send plain application/json without an envelope.
func TestUnarySendsPlainJSON(t *testing.T) {
	f := newFakeEnvd(t)
	var gotContentType, gotBody string
	f.srv.Config.Handler = wrapCapture(f.srv.Config.Handler, &gotContentType, &gotBody)

	c := NewClient("cube.app")
	if err := c.SendInput(context.Background(), "sbx-abc", 42, []byte("x")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if !strings.HasPrefix(gotBody, "{") {
		t.Fatalf("body = %q, want raw JSON with no Connect envelope", gotBody)
	}
}
