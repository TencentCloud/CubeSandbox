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
	// maxRoutingPagePeekBytes bounds the error-body prefix buffered to tell a
	// JSON API error document apart from an HTML/text routing page.
	maxRoutingPagePeekBytes = 4096
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

// responseMediaType returns the lower-cased media type of the response's
// Content-Type header, or "" when the header is missing.
func responseMediaType(resp *http.Response) string {
	rawCT := resp.Header.Get("Content-Type")
	if rawCT == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(rawCT); err == nil {
		return mt
	}
	return strings.ToLower(strings.TrimSpace(strings.Split(rawCT, ";")[0]))
}

// looksLikeJSONErrorDocument reports whether body is a JSON object carrying a
// "message", "detail", or "error" string — the shapes API errors take in this
// project (readErrorMessage extracts "message"/"detail"; CubeProxy and CubeOps
// emit {"error": "..."}). Such a document is a genuine API error even when the
// Content-Type header says text/*, so it keeps its error classification.
func looksLikeJSONErrorDocument(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var doc struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Err     string `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return false
	}
	return strings.TrimSpace(doc.Message) != "" || strings.TrimSpace(doc.Detail) != "" || strings.TrimSpace(doc.Err) != ""
}

// misroutedHTMLDiagnostic reports an HTTP 200 text/html response as a routing
// diagnostic: a Connect stream is never labeled text/html, so such a response
// means the request landed on a Web UI or portal page instead of CubeProxy.
// It returns nil for anything else and never reads the body.
func misroutedHTMLDiagnostic(resp *http.Response) *APIError {
	if resp == nil || resp.StatusCode != http.StatusOK || responseMediaType(resp) != "text/html" {
		return nil
	}
	return &APIError{
		Kind:    apiErrorKindAPI,
		Message: fmt.Sprintf("unexpected content-type %q (received HTML instead of Connect stream; check whether CUBE_PROXY_NODE_IP is configured or if the data-plane endpoint points to a web service)", resp.Header.Get("Content-Type")),
	}
}

// validateConnectResponse ensures the HTTP response represents a valid Connect stream.
// Non-200 responses are parsed into an APIError via apiErrorFromResponse, enriching
// HTML and 404 text pages with actionable hints while clearing false error
// classifications on 404 routing pages (preserving ErrAuthentication on 401/403 and
// genuine JSON API error documents, which keep their classification even when served
// with a text/* content type; a body that looks like truncated JSON is left
// untouched rather than risk misclassification). Legitimate CubeProxy 404 responses (on both control and
// data planes) arrive as JSON; an HTML or text 404 on a data-plane streaming endpoint
// indicates the request was misrouted (e.g. to a web UI or a Go net/http.NotFound
// gateway) rather than reaching CubeProxy.
// For 200 responses, text/html media types (such as from a misrouted Web UI or portal
// landing page) are immediately rejected with an *APIError without blocking on body reads.
// Other content types (and omitted headers) fall through to envelope parsing for maximum
// proxy tolerance.
func validateConnectResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return errors.New("nil response")
	}

	if resp.StatusCode != http.StatusOK {
		// Peek at a bounded prefix of the body so a genuine JSON API error
		// document can be told apart from an HTML/text routing page without
		// buffering a whole misrouted page into memory.
		var peek []byte
		peek, _ = io.ReadAll(io.LimitReader(resp.Body, maxRoutingPagePeekBytes))
		lowerPeek := bytes.ToLower(bytes.TrimSpace(peek))
		bodyLooksHTML := bytes.HasPrefix(lowerPeek, []byte("<!doctype")) || bytes.HasPrefix(lowerPeek, []byte("<html"))
		jsonErrorDoc := looksLikeJSONErrorDocument(peek)
		// If the peek was cut off at the limit and starts like a JSON document,
		// it may be a large JSON error document we cannot fully parse: leave
		// the server's error untouched rather than risk misclassification.
		trimmedPeek := bytes.TrimSpace(peek)
		undecidedJSON := !jsonErrorDoc && len(peek) == maxRoutingPagePeekBytes && len(trimmedPeek) > 0 && trimmedPeek[0] == '{'

		mediaType := responseMediaType(resp)
		isHTML := mediaType == "text/html" || bodyLooksHTML
		isText404 := resp.StatusCode == http.StatusNotFound && strings.HasPrefix(mediaType, "text/")
		routingPage := (isHTML || isText404) && !jsonErrorDoc && !undecidedJSON
		if routingPage {
			// Only the truncated prefix below is used; drop the rest of a
			// possibly multi-megabyte page instead of buffering it.
			resp.Body = io.NopCloser(bytes.NewReader(peek))
		} else {
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peek), resp.Body))
		}

		var apiErr *APIError
		if err := apiErrorFromResponse(resp); !errors.As(err, &apiErr) {
			// apiErrorFromResponse always returns *APIError today; degrade
			// gracefully instead of panicking if that ever changes.
			return err
		}
		if routingPage {
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

	if diag := misroutedHTMLDiagnostic(resp); diag != nil {
		return diag
	}
	return nil
}
