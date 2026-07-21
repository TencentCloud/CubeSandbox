// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	connectProtocolVersion = "1"
	connectContentType     = "application/connect+json"
	connectEndStreamFlag   = byte(0x02)
	connectCompressedFlag  = byte(0x01)
	maxConnectEnvelopeSize = 64 * 1024 * 1024
)

type connectEndStream struct {
	Error *connectError `json:"error,omitempty"`
}

type connectError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// encodeConnectEnvelope wraps payload in a Connect streaming message: a 5-byte
// header (1 flag byte + big-endian uint32 length) followed by the payload. Both
// request and response messages on a streaming Connect RPC use this framing.
func encodeConnectEnvelope(payload []byte) io.Reader {
	buf := bytes.NewBuffer(make([]byte, 0, 5+len(payload)))
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	buf.Write(header[:])
	buf.Write(payload)
	return buf
}

func readConnectEnvelope(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return 0, nil, err
		}
		return 0, nil, err
	}

	size := binary.BigEndian.Uint32(header[1:])
	if size > maxConnectEnvelopeSize {
		return 0, nil, fmt.Errorf("Connect stream message too large: %d bytes", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func parseConnectEndStream(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}

	var end connectEndStream
	if err := json.Unmarshal(raw, &end); err != nil {
		return fmt.Errorf("decode Connect end stream: %w", err)
	}
	if end.Error == nil {
		return nil
	}
	message := strings.TrimSpace(end.Error.Message)
	if message == "" {
		message = "Connect stream error"
	}
	if end.Error.Code != "" {
		return fmt.Errorf("%s: %s", end.Error.Code, message)
	}
	return fmt.Errorf("%s", message)
}
