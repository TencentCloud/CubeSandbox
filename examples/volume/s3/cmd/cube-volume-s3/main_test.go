// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
)

type fakeCreateStore struct {
	exists      bool
	existsErr   error
	hasData     bool
	prefixErr   error
	createErr   error
	events      []string
	createdPath string
}

func (s *fakeCreateStore) BucketExists(context.Context) (bool, error) {
	s.events = append(s.events, "bucket")
	return s.exists, s.existsErr
}

func (s *fakeCreateStore) PrefixHasData(_ context.Context, prefix string) (bool, error) {
	s.events = append(s.events, "prefix")
	s.createdPath = prefix
	return s.hasData, s.prefixErr
}

func (s *fakeCreateStore) CreatePrefix(_ context.Context, prefix string) error {
	s.events = append(s.events, "create")
	s.createdPath = prefix
	return s.createErr
}

func TestDynamicCreatePreconditions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exists    bool
		hasData   bool
		setupErr  error
		prefixErr error
		createErr error
		want      []string
		wantError string
	}{
		{name: "missing bucket", want: []string{"bucket"}, wantError: "does not exist"},
		{name: "bucket lookup fails", setupErr: errors.New("lookup failed"), want: []string{"bucket"}, wantError: "lookup failed"},
		{name: "prefix has existing objects", exists: true, hasData: true, want: []string{"bucket", "prefix"}, wantError: "already contains objects"},
		{name: "prefix lookup fails", exists: true, prefixErr: errors.New("list failed"), want: []string{"bucket", "prefix"}, wantError: "list failed"},
		{name: "marker create fails without cleanup", exists: true, createErr: errors.New("put failed"), want: []string{"bucket", "prefix", "create"}, wantError: "put failed"},
		{name: "empty prefix", exists: true, want: []string{"bucket", "prefix", "create"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCreateStore{
				exists: tc.exists, existsErr: tc.setupErr, hasData: tc.hasData,
				prefixErr: tc.prefixErr, createErr: tc.createErr,
			}
			err := createDynamicPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatalf("createDynamicPrefix: %v", err)
			}
			if !reflect.DeepEqual(store.events, tc.want) {
				t.Errorf("operations = %v, want %v", store.events, tc.want)
			}
			if len(store.events) > 1 && store.createdPath != "workspaces/user-a/" {
				t.Errorf("prefix = %q, want the requested normalized prefix", store.createdPath)
			}
		})
	}
}

// A resolved state that exceeds the API limit must fail before any S3 request.
// The input fits because encoding escapes each '<' as six ASCII bytes.
func TestCreateRejectsOversizeStateBeforeTouchingBucket(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	conf := filepath.Join(dir, "volume-s3.conf")
	credentialDir := filepath.Join(dir, "credentials")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("ACCESS_KEY_ID=ak\nSECRET_ACCESS_KEY=sk\nBUCKET=legacy-bucket\nENDPOINT="+srv.URL+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "user.conf"), []byte("ACCESS_KEY_ID=ref-ak\nSECRET_ACCESS_KEY=ref-sk\nENDPOINT="+srv.URL+"\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("CUBE_S3_CONFIG", conf)
	t.Setenv("CUBE_S3_CREDENTIALS_DIR", credentialDir)

	raw := `{"bucket":"user-bucket","prefix":"` + strings.Repeat("<", 300) + `/","cred_ref":"user"}`
	if len(raw) > binding.MaxPrivateDataLen {
		t.Fatalf("test descriptor is %d bytes, must fit the input limit", len(raw))
	}
	err := doCreate(context.Background(), &options{volumeID: "vol-1", privateData: raw})
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("doCreate error = %v, want final private_data size rejection", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("S3 requests = %d, want no bucket or prefix access", requests.Load())
	}
}

func TestMountDirFromMetadata(t *testing.T) {
	if got := mountDirFromMetadata(`{"mount_dir":"/data/x/s3-v1"}`); got != "/data/x/s3-v1" {
		t.Errorf("mountDirFromMetadata = %q", got)
	}
	for _, raw := range []string{"", "not json", `{}`} {
		if got := mountDirFromMetadata(raw); got != "" {
			t.Errorf("mountDirFromMetadata(%q) = %q, want empty", raw, got)
		}
	}
}

func TestPasswdFileFromMetadata(t *testing.T) {
	if got := passwdFileFromMetadata(`{"passwd_file":"/etc/cube/.passwd-s3fs-dynamic-v1"}`); got != "/etc/cube/.passwd-s3fs-dynamic-v1" {
		t.Errorf("passwdFileFromMetadata = %q", got)
	}
	for _, raw := range []string{"", "not json", `{}`} {
		if got := passwdFileFromMetadata(raw); got != "" {
			t.Errorf("passwdFileFromMetadata(%q) = %q, want empty", raw, got)
		}
	}
}

// Exercise the shipped entry point so its JSON envelope and exit status are
// checked together rather than inferred from doCreate's returned error.
func TestBinaryCreateErrorKinds(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cube-volume-s3")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}

	missingConfig := filepath.Join(t.TempDir(), "missing.conf")
	for _, tc := range []struct {
		name      string
		op        string
		raw       string
		wantKind  string
		wantError string
	}{
		{"bucket only", "create", `{"bucket":"user-bucket"}`, "invalid_argument", "cred_ref is required"},
		{"prefix only", "create", `{"prefix":"workspaces/user-a/"}`, "invalid_argument", "cred_ref is required"},
		{"bucket and prefix", "create", `{"bucket":"user-bucket","prefix":"workspaces/user-a/"}`, "invalid_argument", "cred_ref is required"},
		{"empty reference", "create", `{"bucket":"user-bucket","cred_ref":""}`, "invalid_argument", "cred_ref is required"},
		{"null reference", "create", `{"bucket":"user-bucket","cred_ref":null}`, "invalid_argument", "cred_ref is required"},
		{"invalid reference", "create", `{"bucket":"user-bucket","cred_ref":"../x"}`, "invalid_argument", "cred_ref has an invalid format"},
		{"invalid reference only", "create", `{"cred_ref":"../x"}`, "invalid_argument", "cred_ref has an invalid format"},
		{"long reference", "create", `{"bucket":"user-bucket","cred_ref":"` + strings.Repeat("a", 65) + `"}`, "invalid_argument", "cred_ref has an invalid format"},
		{"whitespace reference", "create", `{"bucket":"user-bucket","cred_ref":" "}`, "invalid_argument", "cred_ref has an invalid format"},
		{"illegal character reference", "create", `{"bucket":"user-bucket","cred_ref":"a/b"}`, "invalid_argument", "cred_ref has an invalid format"},
		{"malformed JSON", "create", `{"bucket":`, "", "JSON object"},
		{"non-string reference", "create", `{"bucket":"user-bucket","cred_ref":123}`, "", "JSON object"},
		{"object reference", "create", `{"bucket":"user-bucket","cred_ref":{}}`, "", "JSON object"},
		{"configuration failure", "create", `{"bucket":"user-bucket","cred_ref":"user"}`, "", missingConfig},
		{"64 character reference reaches configuration", "create", `{"bucket":"user-bucket","cred_ref":"` + strings.Repeat("a", 64) + `"}`, "", missingConfig},
		{"legacy configuration failure", "create", "", "", missingConfig},
		{"destroy state failure", "destroy", `{"v":1,"bucket":"user-bucket","prefix":"workspaces/user-a/"}`, "", "requires cred_ref"},
		{"attach state failure", "attach", `{"v":1,"bucket":"user-bucket","prefix":"workspaces/user-a/"}`, "", "requires cred_ref"},
		{"destroy invalid state reference", "destroy", `{"v":1,"bucket":"user-bucket","prefix":"workspaces/user-a/","cred_ref":"../x"}`, "", "must not contain .."},
		{"attach invalid state reference", "attach", `{"v":1,"bucket":"user-bucket","prefix":"workspaces/user-a/","cred_ref":"../x"}`, "", "must not contain .."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--op", tc.op, "--volume-id", "vol-1", "--private-data", tc.raw)
			cmd.Env = append(os.Environ(), "CUBE_S3_CONFIG="+missingConfig)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("exit = %v, want status 1; stderr=%s", err, stderr.String())
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("stdout is not one JSON object: %v; stdout=%q", err, stdout.String())
			}
			var message string
			if err := json.Unmarshal(response["error"], &message); err != nil || message == "" {
				t.Fatalf("error field = %s, decode error = %v", response["error"], err)
			}
			if tc.wantKind != "" && message != tc.wantError {
				t.Errorf("error = %q, want public message %q", message, tc.wantError)
			} else if !strings.Contains(message, tc.wantError) {
				t.Errorf("error = %q, want it to contain %q", message, tc.wantError)
			}
			if tc.wantKind != "" {
				for _, private := range []string{"create:", "../x", "user-bucket", missingConfig} {
					if strings.Contains(message, private) {
						t.Errorf("public error contains internal/input detail %q: %q", private, message)
					}
				}
			}
			kind, present := response["error_kind"]
			if tc.wantKind == "" {
				if present {
					t.Errorf("generic failure includes error_kind=%s", kind)
				}
				return
			}
			var gotKind string
			if err := json.Unmarshal(kind, &gotKind); err != nil || gotKind != tc.wantKind {
				t.Errorf("error_kind=%s, decode error=%v, want %q", kind, err, tc.wantKind)
			}
		})
	}
}

func TestCreateCredRefPrecedesConfiguration(t *testing.T) {
	t.Setenv("CUBE_S3_CONFIG", filepath.Join(t.TempDir(), "missing.conf"))
	for _, tc := range []struct {
		name string
		raw  string
		want error
	}{
		{"missing", `{"bucket":"user-bucket"}`, binding.ErrMissingCredRef},
		{"invalid", `{"bucket":"user-bucket","cred_ref":"../x"}`, binding.ErrInvalidCredRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := doCreate(t.Context(), &options{volumeID: "vol-1", privateData: tc.raw})
			if !errors.Is(err, tc.want) {
				t.Fatalf("doCreate error = %v, want wrapped %v before config loading", err, tc.want)
			}
		})
	}
}
