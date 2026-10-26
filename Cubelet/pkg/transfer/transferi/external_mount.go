// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package transferi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

var lockMap = new(sync.Map)

func MountExternalSnapshot(ctx context.Context, sn snapshots.Snapshotter,
	chain []digest.Digest,
	annotations map[string]string) error {
	if len(chain) == 0 {
		return fmt.Errorf("empty chain")
	}
	var (
		err     error
		parent  = identity.ChainID(chain[:len(chain)-1])
		chainID = identity.ChainID(chain).String()
		start   = time.Now()
	)

	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "mount_external_snapshot_count", time.Since(start), time.Millisecond)
	}()
	lock, _ := lockMap.LoadOrStore(chainID, new(sync.Mutex))
	lock.(*sync.Mutex).Lock()
	defer func() {
		lock.(*sync.Mutex).Unlock()
		lockMap.Delete(chainID)
	}()

	if _, err := sn.Stat(ctx, chainID); err == nil {

		return nil
	}

	snapshotLabels := snapshots.FilterInheritedLabels(annotations)
	if snapshotLabels == nil {
		snapshotLabels = make(map[string]string)
	}
	snapshotLabels[constants.AnnotationSnapshotRef] = chainID

	var (
		key  string
		opts = []snapshots.Opt{snapshots.WithLabels(snapshotLabels)}
	)

	mountFromExternal := func(ctx context.Context, key, parent string, opts []snapshots.Opt) error {

		key = fmt.Sprintf(snapshots.UnpackKeyFormat, uniquePart(), chainID)
		_, err = sn.Prepare(ctx, key, parent, opts...)
		if err != nil {
			if errdefs.IsAlreadyExists(err) {

				return nil
			} else {
				return fmt.Errorf("failed to prepare extraction snapshot %q: %w", key, err)
			}
		}

		abort := func(ctx context.Context) {
			if err := sn.Remove(ctx, key); err != nil {
				log.G(ctx).WithError(err).Errorf("failed to cleanup %q", key)
			}
		}

		if err := sn.Commit(ctx, chainID, key, opts...); err != nil {
			abort(ctx)
			return fmt.Errorf("failed to commit snapshot %s: %w", chainID, err)
		}
		log.G(ctx).Infof("mount external snapshot %s from %s success", chainID, annotations[constants.AnnotationSnapshotterExternalPath])

		return nil
	}
	for try := 1; try <= 3; try++ {
		err = mountFromExternal(ctx, key, parent.String(), opts)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("unable to prepare %s external extraction snapshot after 3 times: %w", parent.String(), err)
	}

	return nil
}

func uniquePart() string {
	t := time.Now()
	var b [3]byte

	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.Nanosecond(), base64.URLEncoding.EncodeToString(b[:]))
}
