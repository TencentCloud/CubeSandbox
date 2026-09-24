// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
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

	if header[0]&^(connectCompressedFlag|connectEndStreamFlag) != 0 {
		return 0, nil, fmt.Errorf("response is not a Connect envelope (first 5 bytes: %x); check whether CUBE_PROXY_NODE_IP is configured, the data-plane endpoint points to a web service, or the response was compressed", header)
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

// validateConnectResponse ensures the HTTP response represents a valid Connect stream.
// Non-200 responses are parsed into an APIError via apiErrorFromResponse, enriching
// HTML and 404 text pages with actionable hints while clearing false error
// classifications on 404 pages (preserving ErrAuthentication on 401/403). Legitimate CubeProxy
// 404 responses (on both control and data planes) arrive as JSON; an HTML or text 404 on a data-plane
// streaming endpoint indicates the request was misrouted (e.g. to a web UI or a Go
// net/http.NotFound gateway) rather than reaching CubeProxy.
// For 200 responses, text/html media types (such as from a misrouted Web UI or portal
// landing page) are immediately rejected with an *APIError without blocking on body reads.
// Other content types (and omitted headers) fall through to envelope parsing for maximum
// proxy tolerance.
func validateConnectResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return errors.New("nil response")
	}

	var mediaType string
	if rawCT := resp.Header.Get("Content-Type"); rawCT != "" {
		if mt, _, err := mime.ParseMediaType(rawCT); err == nil {
			mediaType = mt
		} else {
			mediaType = strings.ToLower(strings.TrimSpace(strings.Split(rawCT, ";")[0]))
		}
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := apiErrorFromResponse(resp).(*APIError)
		lowerMsg := strings.ToLower(strings.TrimSpace(apiErr.Message))
		isHTML := mediaType == "text/html" || strings.HasPrefix(lowerMsg, "<!doctype") || strings.HasPrefix(lowerMsg, "<html")
		isText404 := resp.StatusCode == http.StatusNotFound && strings.HasPrefix(mediaType, "text/")
		if isHTML || isText404 {
			const maxDiagnosticMsgLen = 200
			if len(apiErr.Message) > maxDiagnosticMsgLen {
				end := maxDiagnosticMsgLen
				for end > 0 && !utf8.RuneStart(apiErr.Message[end]) {
					end--
				}
				apiErr.Message = apiErr.Message[:end] + "..."
			}
			apiErr.Message += "; response may be an HTML/text page instead of a Connect stream; check whether CUBE_PROXY_NODE_IP is configured or if the data-plane endpoint points to a web service"
			if resp.StatusCode == http.StatusNotFound {
				apiErr.Kind = apiErrorKindAPI
			}
		}
		if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
			if location := resp.Header.Get("Location"); location != "" {
				apiErr.Message += "; redirect to " + location
			}
		}
		return apiErr
	}

	if mediaType == "text/html" {
		return &APIError{
			Kind:    apiErrorKindAPI,
			Message: fmt.Sprintf("unexpected content-type %q (received HTML instead of Connect stream; check whether CUBE_PROXY_NODE_IP is configured or if the data-plane endpoint points to a web service)", resp.Header.Get("Content-Type")),
		}
	}
	return nil
}
