// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// frame builds a single Connect envelope: [flags][4-byte len][payload].
func frame(flags byte, v any) []byte {
	payload, _ := json.Marshal(v)
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

func TestReadPtyStartPID(t *testing.T) {
	var stream bytes.Buffer
	// A keepalive before the start event must be skipped.
	stream.Write(frame(0, processStartResponse{Event: &processEvent{Keepalive: &struct{}{}}}))
	stream.Write(frame(0, processStartResponse{Event: &processEvent{Start: &processStartEvent{PID: 4321}}}))

	pid, err := readPtyStartPID(&stream)
	if err != nil {
		t.Fatalf("readPtyStartPID: %v", err)
	}
	if pid != 4321 {
		t.Fatalf("pid = %d, want 4321", pid)
	}
}

func TestReadPtyEventDataAndEnd(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("hello\r\n"))
	var stream bytes.Buffer
	stream.Write(frame(0, processStartResponse{Event: &processEvent{Data: &processDataEvent{PTY: payload}}}))
	code := 7
	stream.Write(frame(0, processStartResponse{Event: &processEvent{End: &processEndEvent{ExitCode: &code}}}))
	stream.Write(frame(connectEndStreamFlag, struct{}{}))

	ev, eos, err := readPtyEvent(&stream)
	if err != nil || eos {
		t.Fatalf("data frame: eos=%v err=%v", eos, err)
	}
	if ev == nil || ev.Data == nil || ev.Data.PTY != payload {
		t.Fatalf("unexpected data frame: %+v", ev)
	}

	ev, eos, err = readPtyEvent(&stream)
	if err != nil || eos {
		t.Fatalf("end frame: eos=%v err=%v", eos, err)
	}
	if ev == nil || ev.End == nil || ev.End.ExitCode == nil || *ev.End.ExitCode != 7 {
		t.Fatalf("unexpected end frame: %+v", ev)
	}

	_, eos, err = readPtyEvent(&stream)
	if err != nil {
		t.Fatalf("trailer: unexpected err %v", err)
	}
	if !eos {
		t.Fatalf("expected end-of-stream on trailer")
	}
}

func TestReadPtyEventEndStreamError(t *testing.T) {
	trailer := map[string]any{
		"error": map[string]any{"code": "internal", "message": "boom"},
	}
	var stream bytes.Buffer
	stream.Write(frame(connectEndStreamFlag, trailer))

	_, eos, err := readPtyEvent(&stream)
	if !eos {
		t.Fatalf("expected eos")
	}
	if err == nil {
		t.Fatalf("expected error from end-stream trailer")
	}
}

func TestReadConnectEnvelopeRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0)
	oversize := uint32(maxConnectEnvelopeSize + 1)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], oversize)
	buf.Write(hdr[:])

	if _, _, err := readConnectEnvelope(&buf); err == nil {
		t.Fatalf("expected oversize frame to be rejected")
	}
}
