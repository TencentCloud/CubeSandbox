// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3api"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3fsmnt"
)

// Only state may be persisted as B. Host credentials remain separate and
// unexported; client and mount adapters consume the same resolved values.
type resolvedVolume struct {
	state       binding.Binding
	credentials *config.Credentials
}

func resolveVolume(cfg *config.Config, state binding.Binding) (resolvedVolume, error) {
	if state.Dynamic && state.CredRef == "" {
		return resolvedVolume{}, fmt.Errorf("dynamic private_data requires cred_ref; global credentials are reserved for legacy volumes")
	}
	if state.Bucket == "" {
		state.Bucket = cfg.Bucket
		if state.Bucket == "" {
			return resolvedVolume{}, fmt.Errorf("private_data names no bucket and BUCKET is not configured")
		}
	}
	creds, err := cfg.ResolveCredentials(state.CredRef)
	if err != nil {
		return resolvedVolume{}, err
	}
	return resolvedVolume{state: state, credentials: creds}, nil
}

func (v resolvedVolume) client(ctx context.Context) (*s3api.Client, error) {
	c := v.credentials
	return s3api.NewFor(ctx, s3api.Options{
		Bucket: v.state.Bucket, Endpoint: c.Endpoint, Region: c.Region, Addressing: c.Addressing,
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, InstanceRole: c.InstanceRole,
	})
}

func (v resolvedVolume) mountSpec(cfg *config.Config, volumeID string) s3fsmnt.Spec {
	c := v.credentials
	spec := s3fsmnt.Spec{
		Bucket: v.state.Bucket, Subdir: strings.TrimSuffix(v.state.Prefix, "/"),
		Endpoint: c.Endpoint, Region: c.Region, Addressing: c.Addressing,
		Dynamic: v.state.Dynamic, InstanceRole: c.InstanceRole, AccessKeyID: c.AccessKeyID, ExtraOpts: c.ExtraOpts,
	}
	if !c.InstanceRole {
		if !v.state.Dynamic && cfg.PasswdFile != "" {
			spec.PasswdFile = cfg.PasswdFile
		} else {
			spec.PasswdFile = cfg.PasswdFileForVolume(volumeID)
		}
		spec.SecretAccessKey = c.SecretAccessKey
	}
	return spec
}
