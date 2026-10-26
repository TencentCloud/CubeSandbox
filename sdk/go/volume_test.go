// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateVolumeSendsPayloadAndParsesResponse(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/volumes" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Fatalf("Authorization=%q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"volumeID":"my-data","name":"my-data","token":"tok-123"}`)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, APIKey: "test-key"})
	volume, err := client.CreateVolume(context.Background(), CreateVolumeOptions{Name: "my-data", Driver: "cos"})
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	if volume.VolumeID != "my-data" || volume.Name != "my-data" || volume.Token != "tok-123" {
		t.Fatalf("volume mismatch: %#v", volume)
	}

	assertString(t, got, "name", "my-data")
	assertString(t, got, "driver", "cos")
}

func TestCreateVolumeOmitsDriverAndAllowsEmptyName(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"volumeID":"3e9b6f2a-0000-4000-8000-000000000000","name":"3e9b6f2a-0000-4000-8000-000000000000","token":""}`)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL})
	volume, err := client.CreateVolume(context.Background(), CreateVolumeOptions{})
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	if volume.VolumeID == "" || volume.Token != "" {
		t.Fatalf("volume mismatch: %#v", volume)
	}

	// Mirror the Python SDK's wire format: name is always sent (empty string
	// requests a server-generated UUID), driver is omitted when unset.
	assertString(t, got, "name", "")
	if _, ok := got["driver"]; ok {
		t.Fatalf("driver should be omitted, got %#v", got["driver"])
	}
}

func TestCreateVolumeValidatesName(t *testing.T) {
	client := newNoRequestClient(t)
	tests := []struct {
		name       string
		volumeName string
	}{
		{"too long", strings.Repeat("a", MaxVolumeNameLen+1)},
		{"invalid characters", "my volume!"},
		{"path separator", "a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := client.CreateVolume(context.Background(), CreateVolumeOptions{Name: tt.volumeName}); err == nil {
				t.Fatalf("CreateVolume(%q) expected validation error", tt.volumeName)
			}
		})
	}
}

func TestListVolumes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/volumes" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"volumeID":"vol-a","name":"vol-a"},{"volumeID":"vol-b","name":"vol-b"}]`)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL})
	volumes, err := client.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes returned error: %v", err)
	}
	if len(volumes) != 2 || volumes[0].VolumeID != "vol-a" || volumes[1].VolumeID != "vol-b" {
		t.Fatalf("volumes mismatch: %#v", volumes)
	}
	// Tokens are only surfaced on create / get-single.
	if volumes[0].Token != "" || volumes[1].Token != "" {
		t.Fatalf("list entries should have empty tokens: %#v", volumes)
	}
}

func TestGetVolumeReturnsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/volumes/my-data" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"volumeID":"my-data","name":"my-data","token":"tok-456"}`)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL})
	volume, err := client.GetVolume(context.Background(), "my-data")
	if err != nil {
		t.Fatalf("GetVolume returned error: %v", err)
	}
	if volume.VolumeID != "my-data" || volume.Token != "tok-456" {
		t.Fatalf("volume mismatch: %#v", volume)
	}
}

func TestDeleteVolume(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/volumes/my-data" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL})
	if err := client.DeleteVolume(context.Background(), "my-data"); err != nil {
		t.Fatalf("DeleteVolume returned error: %v", err)
	}
}

func TestVolumeErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		target     error
		call       func(*Client) error
	}{
		{
			name:       "get volume not found",
			statusCode: http.StatusNotFound,
			body:       `{"message":"volume not found: my-data"}`,
			target:     ErrVolumeNotFound,
			call: func(c *Client) error {
				_, err := c.GetVolume(context.Background(), "my-data")
				return err
			},
		},
		{
			name:       "delete volume not found",
			statusCode: http.StatusNotFound,
			body:       `{"message":"volume not found: my-data"}`,
			target:     ErrVolumeNotFound,
			call: func(c *Client) error {
				return c.DeleteVolume(context.Background(), "my-data")
			},
		},
		{
			name:       "delete volume in use",
			statusCode: http.StatusConflict,
			body:       `{"message":"conflict: volume my-data is in use by 2 node(s); destroy the sandboxes using it before deleting"}`,
			target:     ErrVolumeInUse,
			call: func(c *Client) error {
				return c.DeleteVolume(context.Background(), "my-data")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			client := NewClient(Config{APIURL: server.URL})
			err := tt.call(client)
			if !errors.Is(err, tt.target) {
				t.Fatalf("errors.Is(%v, %v)=false", err, tt.target)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.statusCode {
				t.Fatalf("APIError mismatch: %#v", err)
			}
		})
	}
}

func TestVolumeIDValidationRejectsUnsafeIDs(t *testing.T) {
	client := newNoRequestClient(t)
	for _, volumeID := range []string{"", "../other", "a/b", "a b", "%2e%2e"} {
		if _, err := client.GetVolume(context.Background(), volumeID); err == nil {
			t.Fatalf("GetVolume(%q) expected validation error", volumeID)
		}
		if err := client.DeleteVolume(context.Background(), volumeID); err == nil {
			t.Fatalf("DeleteVolume(%q) expected validation error", volumeID)
		}
	}
}

func TestCreateWithVolumeMountsSendsPayload(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-test"))
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-test"})
	_, err := client.Create(context.Background(), CreateOptions{
		VolumeMounts: []VolumeMount{
			{Name: "my-data", Path: "/workspace"},
			{Name: "shared-cache", Path: "/cache", ReadOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	mounts, ok := got["volumeMounts"].([]any)
	if !ok || len(mounts) != 2 {
		t.Fatalf("volumeMounts=%#v", got["volumeMounts"])
	}
	first, ok := mounts[0].(map[string]any)
	if !ok || first["name"] != "my-data" || first["path"] != "/workspace" {
		t.Fatalf("volumeMounts[0]=%#v", mounts[0])
	}
	// Python-compatible wire format: readOnly is omitted when false.
	if _, ok := first["readOnly"]; ok {
		t.Fatalf("volumeMounts[0].readOnly should be omitted, got %#v", first["readOnly"])
	}
	second, ok := mounts[1].(map[string]any)
	if !ok || second["name"] != "shared-cache" || second["path"] != "/cache" || second["readOnly"] != true {
		t.Fatalf("volumeMounts[1]=%#v", mounts[1])
	}
}

func TestCreateOmitsVolumeMountsWhenEmpty(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-test"))
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-test"})
	if _, err := client.Create(context.Background(), CreateOptions{}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, ok := got["volumeMounts"]; ok {
		t.Fatalf("volumeMounts should be omitted, got %#v", got["volumeMounts"])
	}
}

func TestCreateValidatesVolumeMounts(t *testing.T) {
	client := newNoRequestClient(t)
	tests := []struct {
		name  string
		mount VolumeMount
	}{
		{"empty volume name", VolumeMount{Name: "", Path: "/workspace"}},
		{"unsafe volume name", VolumeMount{Name: "../other", Path: "/workspace"}},
		{"empty path", VolumeMount{Name: "my-data", Path: ""}},
		{"relative path", VolumeMount{Name: "my-data", Path: "workspace"}},
		{"dot-dot segment", VolumeMount{Name: "my-data", Path: "/workspace/../etc"}},
		{"dot segment", VolumeMount{Name: "my-data", Path: "/./workspace"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.Create(context.Background(), CreateOptions{
				TemplateID:   "tpl-test",
				VolumeMounts: []VolumeMount{tt.mount},
			})
			if err == nil {
				t.Fatalf("Create with mount %#v expected validation error", tt.mount)
			}
		})
	}
}

// newNoRequestClient returns a client whose backend fails the test if any
// HTTP request is made — for asserting client-side validation short-circuits.
func newNoRequestClient(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return NewClient(Config{APIURL: server.URL, TemplateID: "tpl-test"})
}

func TestVolumeInfoStringMasksToken(t *testing.T) {
	volume := VolumeInfo{VolumeID: "my-data", Name: "my-data", Token: "tok-secret-123"}
	for _, formatted := range []string{
		volume.String(),
		fmt.Sprintf("%v", volume),
		fmt.Sprintf("%+v", volume),
		fmt.Sprintf("%#v", volume),
		fmt.Sprintf("%v", &volume),
	} {
		if strings.Contains(formatted, "tok-secret-123") {
			t.Fatalf("token leaked into formatted output: %s", formatted)
		}
		if !strings.Contains(formatted, `"***"`) {
			t.Fatalf("masked token marker missing: %s", formatted)
		}
		if !strings.Contains(formatted, "my-data") {
			t.Fatalf("volume ID missing from formatted output: %s", formatted)
		}
	}

	// An absent token renders as empty, not as the mask — mirroring Python.
	empty := VolumeInfo{VolumeID: "no-token", Name: "no-token"}
	if s := fmt.Sprintf("%#v", empty); strings.Contains(s, "***") {
		t.Fatalf("empty token should not render a mask: %s", s)
	}
}

func TestVolumeNotFoundMappingRequiresExactPhrase(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		message    string
		target     error
	}{
		{
			name:       "exact phrase maps to volume",
			statusCode: http.StatusNotFound,
			message:    "volume not found: my-data",
			target:     ErrVolumeNotFound,
		},
		{
			name:       "sandbox 404 mentioning a volume stays sandbox",
			statusCode: http.StatusNotFound,
			message:    "failed to mount volume: sandbox not found",
			target:     ErrSandboxNotFound,
		},
		{
			name:       "backend 500 with embedded volume not found",
			statusCode: http.StatusInternalServerError,
			message:    "CubeMaster returned error code 130404: volume not found: my-data",
			target:     ErrVolumeNotFound,
		},
		{
			name:       "conflict with different wording stays generic",
			statusCode: http.StatusConflict,
			message:    "volume already exists: my-data",
			target:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apiErrorFromStatus(tt.statusCode, tt.message)
			for _, sentinel := range []error{ErrVolumeNotFound, ErrSandboxNotFound, ErrVolumeInUse} {
				want := sentinel == tt.target
				if got := errors.Is(err, sentinel); got != want {
					t.Fatalf("errors.Is(%v, %v)=%v, want %v", err, sentinel, got, want)
				}
			}
		})
	}
}
