// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

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
