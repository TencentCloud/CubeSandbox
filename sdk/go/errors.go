// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	apiErrorKindAPI              = "api"
	apiErrorKindAuthentication   = "authentication"
	apiErrorKindSandboxNotFound  = "sandbox_not_found"
	apiErrorKindTemplateNotFound = "template_not_found"
)

var (
	ErrAuthentication   = errors.New("cubesandbox: authentication failed")
	ErrSandboxNotFound  = errors.New("cubesandbox: sandbox not found")
	ErrTemplateNotFound = errors.New("cubesandbox: template not found")
)

// APIError describes an HTTP error returned by the CubeSandbox API.
type APIError struct {
	StatusCode int
	Message    string
	Kind       string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.StatusCode == 0 {
		return e.Message
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.StatusCode)
}

func (e *APIError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrAuthentication:
		return e.Kind == apiErrorKindAuthentication
	case ErrSandboxNotFound:
		return e.Kind == apiErrorKindSandboxNotFound
	case ErrTemplateNotFound:
		return e.Kind == apiErrorKindTemplateNotFound
	default:
		return false
	}
}

func apiErrorFromResponse(resp *http.Response) error {
	message := readErrorMessage(resp)
	return apiErrorFromStatus(resp.StatusCode, message)
}

func apiErrorFromStatus(statusCode int, message string) *APIError {
	message = strings.TrimSpace(message)
	if message == "" {
		message = fmt.Sprintf("HTTP %d", statusCode)
	}

	kind := apiErrorKindAPI
	lowerMessage := strings.ToLower(message)
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = apiErrorKindAuthentication
	case http.StatusNotFound:
		if strings.Contains(lowerMessage, "template") {
			kind = apiErrorKindTemplateNotFound
		} else {
			kind = apiErrorKindSandboxNotFound
		}
	}
	if kind == apiErrorKindAPI && strings.Contains(lowerMessage, "not found") {
		switch {
		case strings.Contains(lowerMessage, "template"):
			kind = apiErrorKindTemplateNotFound
		case strings.Contains(lowerMessage, "sandbox"):
			kind = apiErrorKindSandboxNotFound
		}
	}

	return &APIError{
		StatusCode: statusCode,
		Message:    message,
		Kind:       kind,
	}
}

func readErrorMessage(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return ""
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err == nil {
		for _, key := range []string{"message", "detail"} {
			if value, ok := body[key].(string); ok && strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return string(raw)
}
