// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateVolumeWithPrivateDataSendsOpaqueInput(t *testing.T) {
	for _, input := range []string{"", "  opaque 桶 '\" \\ $()\n ", strings.Repeat("a", 1024), strings.Repeat("界", 341) + "a", "é中\u2028\uFFFF\ufffd", `literal \ud83d\ude00`} {
		var got map[string]any
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != http.MethodPost || r.URL.Path != "/v2/volumes" {
				t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("missing existing auth header")
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"volumeID":"vol-pd","name":"vol-pd","token":"tok"}`)
		}))
		client := NewClient(Config{APIURL: server.URL, APIKey: "test-key"})
		volume, err := client.CreateVolumeWithPrivateData(context.Background(), CreateVolumeWithPrivateDataOptions{
			Name: "vol-pd", Driver: "s3", PrivateData: input,
		})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || got["private_data"] != input || got["name"] != "vol-pd" || got["driver"] != "s3" || len(got) != 3 {
			t.Fatal("versioned request did not preserve its fields")
		}
		if volume.VolumeID != "vol-pd" || volume.Token != "tok" {
			t.Fatal("unexpected existing volume response")
		}
	}
}

func TestCreateVolumeWithPrivateDataRejectsInvalidInputBeforeRequest(t *testing.T) {
	client := newNoRequestClient(t)
	for _, input := range []string{strings.Repeat("a", 1025), strings.Repeat("界", 342), "😀", "𠀀", "private-data-sentinel\x00tail"} {
		_, err := client.CreateVolumeWithPrivateData(context.Background(), CreateVolumeWithPrivateDataOptions{PrivateData: input})
		if err == nil || !strings.Contains(err.Error(), "private_data") || strings.Contains(err.Error(), input) || strings.Contains(err.Error(), "private-data-sentinel") {
			t.Fatalf("expected a sanitized validation error, got %v", err)
		}
	}
	if _, err := client.CreateVolumeWithPrivateData(context.Background(), CreateVolumeWithPrivateDataOptions{Name: "bad/name"}); err == nil {
		t.Fatal("expected existing name validation")
	}
}

func TestCreateVolumeWithPrivateDataRejectsSupplementaryCharactersBeforeRequest(t *testing.T) {
	client := newNoRequestClient(t)
	for _, input := range []string{"😀", "𠀀", "\U00010000"} {
		_, err := client.CreateVolumeWithPrivateData(context.Background(), CreateVolumeWithPrivateDataOptions{PrivateData: input})
		if err == nil || err.Error() != "private_data must not contain characters outside the BMP" {
			t.Errorf("input %q error = %v, want fixed BMP error", input, err)
		}
	}
}

func TestCreateVolumeWithPrivateDataRejectsInvalidUTF8BeforeRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"invalid_byte", "\xff"},
		{"unexpected_continuation", "\x80"},
		{"truncated_sequence", "\xe7\x95"},
		{"overlong_sequence", "\xc0\xaf"},
		{"surrogate", "\xed\xa0\x80"},
		{"beyond_unicode_range", "\xf4\x90\x80\x80"},
		{"embedded", "private-data-sentinel\xfftail"},
		{"replacement_expands_past_limit", strings.Repeat("\xff", 342)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newNoRequestClient(t)
			volume, err := client.CreateVolumeWithPrivateData(context.Background(), CreateVolumeWithPrivateDataOptions{PrivateData: tc.input})
			if err == nil || err.Error() != "private_data must be valid UTF-8" {
				t.Fatalf("expected fixed UTF-8 validation error, got %v", err)
			}
			if volume != nil {
				t.Fatal("invalid input must not return a volume")
			}
		})
	}
}

func TestCreateVolumeWithPrivateDataDoesNotFallback(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v2/volumes" {
			t.Error("must not fall back to the legacy route")
		}
		http.Error(w, "route not found", http.StatusNotFound)
	}))
	defer server.Close()
	_, err := NewClient(Config{APIURL: server.URL}).CreateVolumeWithPrivateData(context.Background(), CreateVolumeWithPrivateDataOptions{PrivateData: "input-A"})
	if err == nil || calls != 1 {
		t.Fatal("unsupported servers must return an error after one request")
	}
}
