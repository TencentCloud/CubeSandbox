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
	apiErrorKindVolumeNotFound   = "volume_not_found"
	apiErrorKindVolumeInUse      = "volume_in_use"
)

var (
	ErrAuthentication   = errors.New("cubesandbox: authentication failed")
	ErrSandboxNotFound  = errors.New("cubesandbox: sandbox not found")
	ErrTemplateNotFound = errors.New("cubesandbox: template not found")
	ErrVolumeNotFound   = errors.New("cubesandbox: volume not found")
	// ErrVolumeInUse matches the HTTP 409 the server returns when deleting a
	// volume that is still mounted by one or more sandboxes.
	ErrVolumeInUse = errors.New("cubesandbox: volume is still in use")
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
	case ErrVolumeNotFound:
		return e.Kind == apiErrorKindVolumeNotFound
	case ErrVolumeInUse:
		return e.Kind == apiErrorKindVolumeInUse
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
		switch {
		case strings.Contains(lowerMessage, "template"):
			kind = apiErrorKindTemplateNotFound
		// Match the exact server phrasing ("volume not found: <id>" from
		// CubeMaster's volume handlers) rather than the bare word "volume",
		// so a sandbox-related 404 that merely mentions a volume cannot be
		// misclassified as ErrVolumeNotFound.
		case strings.Contains(lowerMessage, "volume not found"):
			kind = apiErrorKindVolumeNotFound
		default:
			kind = apiErrorKindSandboxNotFound
		}
	case http.StatusConflict:
		// CubeMaster refuses to delete a mounted volume with
		// "volume <id> is in use by <n> node(s); ..." relayed as HTTP 409.
		// This mapping is coupled to that wording (handleDeleteVolume in
		// CubeMaster) — keep the two in sync. If the server rewords the
		// message, errors.Is(err, ErrVolumeInUse) degrades to false and the
		// error surfaces as a generic 409 APIError. The duplicate-name 409
		// ("volume already exists") intentionally stays generic.
		if strings.Contains(lowerMessage, "volume") && strings.Contains(lowerMessage, "in use") {
			kind = apiErrorKindVolumeInUse
		}
	}
	if kind == apiErrorKindAPI && strings.Contains(lowerMessage, "not found") {
		switch {
		case strings.Contains(lowerMessage, "template"):
			kind = apiErrorKindTemplateNotFound
		case strings.Contains(lowerMessage, "volume not found"):
			kind = apiErrorKindVolumeNotFound
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
