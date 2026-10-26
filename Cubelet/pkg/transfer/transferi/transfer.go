// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package transferi

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ExternalMounter interface {
	Mount(context.Context, string, string) (string, error)
	Unmount(context.Context, string) error
}

type ExternalRootfs interface {
	PrepareContent(context.Context, content.Store) (imagespec.Descriptor, error)
}
