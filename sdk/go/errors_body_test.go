// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestReadErrorMessageBound(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"message", `{"message":"denied"}`, "denied"},
		{"detail", `{"detail":"denied"}`, "denied"},
		{"text", "  denied  ", "denied"},
		{"exact_limit", strings.Repeat("x", maxErrorBodyBytes), strings.Repeat("x", maxErrorBodyBytes)},
		{"oversized", strings.Repeat("x", maxErrorBodyBytes+100), strings.Repeat("x", maxErrorBodyBytes) + " [truncated]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.body)
			resp := &http.Response{Body: io.NopCloser(reader)}
			defer resp.Body.Close()
			if got := readErrorMessage(resp); got != tc.want {
				t.Fatalf("message length=%d, want=%d", len(got), len(tc.want))
			}
			if read := len(tc.body) - reader.Len(); read > maxErrorBodyBytes+1 {
				t.Fatalf("read %d bytes", read)
			}
		})
	}
}
