// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package service

import (
	"context"
	"fmt"

	"github.com/containerd/typeurl/v2"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/transfer/transferi"
	"golang.org/x/sync/semaphore"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/unpack"
	"github.com/containerd/containerd/v2/pkg/imageverifier"
	"github.com/containerd/errdefs"
)

type localTransferService struct {
	content content.Store
	images  images.Store

	limiterU *semaphore.Weighted

	limiterD *semaphore.Weighted
	config   TransferConfig
}

func NewTransferService(cs content.Store, is images.Store, tc TransferConfig) transfer.Transferrer {
	ts := &localTransferService{
		content: cs,
		images:  is,
		config:  tc,
	}
	if tc.MaxConcurrentUploadedLayers > 0 {
		ts.limiterU = semaphore.NewWeighted(int64(tc.MaxConcurrentUploadedLayers))
	}
	if tc.MaxConcurrentDownloads > 0 {
		ts.limiterD = semaphore.NewWeighted(int64(tc.MaxConcurrentDownloads))
	}
	return ts
}

func (ts *localTransferService) Transfer(ctx context.Context, src interface{}, dest interface{}, opts ...transfer.Opt) error {
	topts := &transfer.Config{}
	for _, opt := range opts {
		opt(topts)
	}

	switch s := src.(type) {
	case transferi.ExternalRootfs:
		switch d := dest.(type) {
		case transfer.ImageStorer:
			return ts.prepareCfs(ctx, s, d, topts)
		}
	}
	return fmt.Errorf("unable to transfer from %s to %s: %w", name(src), name(dest), errdefs.ErrNotImplemented)
}

func name(t interface{}) string {
	switch s := t.(type) {
	case fmt.Stringer:
		return s.String()
	case typeurl.Any:
		return s.GetTypeUrl()
	default:
		return fmt.Sprintf("%T", t)
	}
}

type TransferConfig struct {
	MaxConcurrentDownloads int

	MaxConcurrentUploadedLayers int

	BaseHandlers []images.Handler

	UnpackPlatforms []unpack.Platform

	Verifiers map[string]imageverifier.ImageVerifier

	RegistryConfigPath string
}
