// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

func TestResolvedVolumeDefaultsAndReference(t *testing.T) {
	for _, reference := range []bool{false, true} {
		name := "legacy defaults"
		if reference {
			name = "dynamic reference"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Bucket: "default-bucket", Endpoint: "https://global.example.com", Region: "global-region",
				AccessKeyID: "global-key", SecretAccessKey: "global-secret", Addressing: config.AddressingPath,
				S3FSExtraOpts: []string{"-ouse_path_request_style", "-odbglevel=info"},
				PasswdFile:    "/run/legacy-passwd", PasswdDir: "/run/dynamic", CredentialsDir: t.TempDir()}
			state := binding.Binding{Prefix: "volumes/vol-1/"}
			if reference {
				state = binding.Binding{Bucket: "user-bucket", Prefix: "workspaces/user/", CredRef: "user", Dynamic: true}
				body := "ACCESS_KEY_ID=reference-key\nSECRET_ACCESS_KEY=reference-secret\nENDPOINT=https://reference.example.com\nREGION=reference-region\nADDRESSING_STYLE=virtual\n"
				if err := os.WriteFile(filepath.Join(cfg.CredentialsDir, "user.conf"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			volume, err := resolveVolume(cfg, state)
			if err != nil {
				t.Fatal(err)
			}
			spec := volume.mountSpec(cfg, "vol-1")
			if spec.Bucket != volume.state.Bucket || spec.Subdir != strings.TrimSuffix(state.Prefix, "/") ||
				spec.Endpoint != volume.credentials.Endpoint || spec.Region != volume.credentials.Region || spec.Addressing != volume.credentials.Addressing {
				t.Fatal("mount adapter diverged from resolved storage binding")
			}
			if reference {
				if spec.Bucket != "user-bucket" || spec.Endpoint != "https://reference.example.com" || spec.Region != "reference-region" ||
					spec.Addressing != config.AddressingVirtual || strings.Contains(strings.Join(spec.ExtraOpts, " "), "use_path_request_style") ||
					spec.PasswdFile != "/run/dynamic/.passwd-s3fs-dynamic-vol-1" || spec.AccessKeyID != "reference-key" || spec.SecretAccessKey != "reference-secret" {
					t.Fatal("dynamic mapping fell back to deployment defaults")
				}
				encoded, err := binding.EncodeState(volume.state)
				if err != nil || strings.Contains(encoded, "reference-key") || strings.Contains(encoded, "reference-secret") {
					t.Fatal("host credentials reached state B")
				}
			} else if spec.Bucket != cfg.Bucket || spec.PasswdFile != cfg.PasswdFile || spec.SecretAccessKey != cfg.SecretAccessKey || state.Bucket != "" {
				t.Fatal("legacy fallback or input immutability changed")
			}
		})
	}
}

func TestResolvedRoleDoesNotFetchCredentials(t *testing.T) {
	cfg := &config.Config{Bucket: "role-bucket", Endpoint: "https://s3.amazonaws.com", Region: "us-east-1", Credentials: config.CredentialsInstanceRole}
	volume, err := resolveVolume(cfg, binding.Binding{Prefix: "volumes/vol-1/"})
	if err != nil {
		t.Fatal(err)
	}
	spec := volume.mountSpec(cfg, "vol-1")
	if !spec.InstanceRole || spec.PasswdFile != "" || spec.SecretAccessKey != "" {
		t.Fatal("role mapping unexpectedly materialized static credentials")
	}
}

func TestResolveDynamicRejectsMissingCredRefBeforeGlobalFallback(t *testing.T) {
	cfg := &config.Config{
		Bucket: "global-bucket", Endpoint: "https://s3.example.com", Region: "us-east-1",
		AccessKeyID: "global-ak", SecretAccessKey: "global-sk",
	}
	if _, err := resolveVolume(cfg, binding.Binding{Bucket: "caller-bucket", Prefix: "tenant/", Dynamic: true}); err == nil || !strings.Contains(err.Error(), "requires cred_ref") {
		t.Fatalf("resolveVolume error = %v, want dynamic cred_ref rejection", err)
	}
}
