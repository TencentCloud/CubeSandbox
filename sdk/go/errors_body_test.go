// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestReadErrorMessageTruncatesAtUTF8Boundary(t *testing.T) {
	for _, char := range []string{"é", "中", "😀"} {
		for included := 0; included <= len(char); included++ {
			t.Run(fmt.Sprintf("%s/bytes_%d", char, included), func(t *testing.T) {
				padding := strings.Repeat("x", maxErrorBodyBytes-included)
				body := padding + char + "tail"
				reader := strings.NewReader(body)
				resp := &http.Response{Body: io.NopCloser(reader)}
				defer resp.Body.Close()
				want := padding
				if included == len(char) {
					want += char
				}
				want += " [truncated]"
				got := readErrorMessage(resp)
				if got != want || !utf8.ValidString(got) {
					t.Fatalf("invalid truncation: length=%d, valid UTF-8=%v", len(got), utf8.ValidString(got))
				}
				if read := len(body) - reader.Len(); read != maxErrorBodyBytes+1 {
					t.Fatalf("read %d bytes, want %d", read, maxErrorBodyBytes+1)
				}
			})
		}
	}
}
