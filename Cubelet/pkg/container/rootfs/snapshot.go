// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package rootfs

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

const (
	fsDir = "fs"
)

func SnapshotRefFs(ctx context.Context, sn snapshots.Snapshotter, snapshotKey string) ([]string, error) {
	var lowerDirs []string
	tempKey := fmt.Sprintf("view-%s", uuid.NewString())
	mount, err := sn.View(ctx, tempKey, snapshotKey)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			mount, err = sn.Mounts(ctx, tempKey)
			if err != nil {
				return nil, fmt.Errorf("failed to get mounts from snapshot %s: %w", tempKey, err)
			}
		} else {
			return nil, fmt.Errorf("failed to gen mounts from snapshot %s: %w", snapshotKey, err)
		}
	}
	defer func() {

		if err := sn.Remove(ctx, tempKey); err != nil {
			log.G(ctx).Errorf("failed to remove snapshot %s: %v", tempKey, err)
		}
	}()

	for _, m := range mount {
		if m.Type == "overlay" {
			for _, opt := range m.Options {
				if strings.HasPrefix(opt, "lowerdir=") {
					opt = strings.TrimPrefix(opt, "lowerdir=")
					lowerDirs = append(lowerDirs, strings.Split(opt, ":")...)
					break
				}
			}
			break
		} else if m.Type == "bind" {
			lowerDirs = append(lowerDirs, m.Source)
		}
	}

	return lowerDirs, nil
}
