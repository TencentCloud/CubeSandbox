// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package redact masks sensitive fields before logging.
//
// Secret is safe-by-default: all its formatters return the redacted marker,
// so it can be passed to a logger without an outer redact.Value() wrapper.
// Use Reveal() to read the plaintext at the outbound API boundary.
package redact

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
)

// redacted is the placeholder written in place of a sensitive value.
const redacted = "***REDACTED***"

// Secret wraps a credential so formatters always redact it. Call Reveal() to
// access the plaintext at the outbound API boundary.
type Secret string

func (s Secret) String() string {
	return redacted
}

func (s Secret) GoString() string {
	return redacted
}

// MarshalJSON emits the redacted marker so cubelog's jsoniter codec never
// leaks the plaintext through a WithFields payload.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(redacted)
}

// UnmarshalJSON discards input. Secret is write-only from the JSON
// perspective; inbound credentials must be assigned via Secret(s).
func (s *Secret) UnmarshalJSON([]byte) error {
	*s = ""
	return nil
}

func (s Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// Reveal returns the plaintext. Call only at the outbound API boundary;
// never log the result.
func (s Secret) Reveal() string {
	return string(s)
}

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

// Value returns a redacted copy of v. Maps and slices of any concrete type are
// traversed (reflection covers e.g. []map[string]interface{}); other types
// pass through unchanged, except Secret leaves, which the type check catches
// even when the key name is innocuous.
//
// The function is non-mutating: nested maps and slices are deep-copied
// so the caller's data is not modified. Cycles are not supported
// (input is expected to be a freshly-decoded JSON value).
func Value(v interface{}) interface{} {
	switch t := v.(type) {
	case Secret:
		return redacted
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
		return valueReflect(v)
	}
}

// valueReflect covers container types the fast paths above miss, e.g.
// []map[string]interface{}; without it, nested Secret or sensitive keys
// would survive into the log line.
func valueReflect(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.IsNil() || rv.Type().Key().Kind() != reflect.String {
			return v
		}
		out := make(map[string]interface{}, rv.Len())
		for _, k := range rv.MapKeys() {
			name := k.String()
			if sensitiveKey(name) {
				out[name] = redacted
			} else {
				out[name] = Value(rv.MapIndex(k).Interface())
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		// []byte is a JSON string, not a container; leave it alone.
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return v
		}
		out := make([]interface{}, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = Value(rv.Index(i).Interface())
		}
		return out
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return v
		}
		return Value(rv.Elem().Interface())
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
