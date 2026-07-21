// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package redact provides helpers to mask sensitive fields before logging
// or otherwise exposing structured data.
//
// The intended use case is logging request bodies that may contain
// credentials (e.g. cube_network_config carries an LLM API key in an
// egress rule's "secret" field). Logging the raw value at Info level
// leaks the credential into log aggregation systems (ELK, Loki, etc.).
//
// The masker is intentionally conservative: it only redacts fields whose
// name matches a known sensitive pattern. Unknown fields are passed
// through unchanged so the log remains useful for debugging.
package redact

import (
	"encoding/json"
	"strings"
)

// redacted is the placeholder written in place of a sensitive value.
const redacted = "***REDACTED***"

// sensitiveKeys is the lower-cased set of map keys that must never be
// logged in plaintext. The set is matched case-insensitively and against
// the suffix of the key (e.g. "LLMApiKey" and "llm_api_key" both match
// "apikey").
//
// Add new entries here when a new credential-shaped field is introduced.
var sensitiveKeys = []string{
	"secret",
	"password",
	"passwd",
	"apikey",
	"api_key",
	"token",
	"accesstoken",
	"access_token",
	"refreshtoken",
	"refresh_token",
	"authorization",
	"privatekey",
	"private_key",
	"clientsecret",
	"client_secret",
}

// sensitiveKey reports whether the given JSON field name (case-insensitive)
// should be redacted. The match is a "contains" check against any of the
// sensitive patterns, so "LlmApiKey", "llm_api_key" and "ApiKey" all
// match "apikey".
func sensitiveKey(name string) bool {
	n := strings.ToLower(name)
	for _, k := range sensitiveKeys {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// Value returns a redacted copy of v. Only the following Go types are
// traversed: map[string]interface{} and []interface{}. Other types are
// returned unchanged (the caller is expected to feed the result through
// json.Marshal so primitive leaves, including strings, are written
// verbatim).
//
// The function is non-mutating: nested maps and slices are deep-copied
// so the caller's data is not modified. Cycles are not supported
// (input is expected to be a freshly-decoded JSON value).
func Value(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			if sensitiveKey(k) {
				out[k] = redacted
			} else {
				out[k] = Value(val)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = Value(val)
		}
		return out
	default:
		return v
	}
}

// JSON is a convenience wrapper: it Value()s v and then json.Marshals
// the result. It returns the JSON bytes so the caller can pass them
// directly to a logger.
func JSON(v interface{}) ([]byte, error) {
	return json.Marshal(Value(v))
}
