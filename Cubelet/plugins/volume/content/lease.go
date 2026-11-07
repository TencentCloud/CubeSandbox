// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
)

// ErrDriverUnavailable means this node has no plugin for the volume's driver.
var ErrDriverUnavailable = errors.New("volume driver is not available on this node")

// Leaser gives a request exclusive-lifetime access to a volume's host
// directory. The directory must stay mounted until release is called.
type Leaser interface {
	Acquire(ctx context.Context, volumeID, driver, privateData string) (hostPath string, release func(), err error)
}

// pluginManager is the subset of volume.Manager used for leases.
type pluginManager interface {
	Has(driver string) bool
	Attach(ctx context.Context, req *volume.AttachRequest) (*volume.AttachResult, error)
	Detach(ctx context.Context, req *volume.DetachRequest) error
}

// ManagerLeaser leases volumes through the same plugin Attach/Detach path and
// node ref-count used by sandboxes, but holds the reference for the duration
// of one content request under a synthetic holder ID. Cubelet's startup
// ref-count recovery drops holders that are not live sandboxes, so references
// left behind by a crash are reclaimed on restart.
type ManagerLeaser struct {
	Mgr       pluginManager
	Namespace string
	BaseDir   string
}

const detachTimeout = 30 * time.Second

func (l *ManagerLeaser) Acquire(ctx context.Context, volumeID, driver, privateData string) (string, func(), error) {
	if driver == "" || !l.Mgr.Has(driver) {
		return "", nil, ErrDriverUnavailable
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", nil, err
	}
	holder := refcount.TransientHolderPrefix + hex.EncodeToString(suffix[:])

	res, err := l.Mgr.Attach(ctx, &volume.AttachRequest{
		SandboxID: holder, Namespace: l.Namespace, VolumeID: volumeID, Driver: driver,
		VolumeBaseDir: l.BaseDir, PrivateData: privateData,
	})
	if err != nil {
		return "", nil, fmt.Errorf("attach volume %q: %w", volumeID, err)
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			// Detach must complete even if the client has gone away.
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachTimeout)
			defer cancel()
			_ = l.Mgr.Detach(dctx, &volume.DetachRequest{
				SandboxID: holder, Namespace: l.Namespace, VolumeID: volumeID,
				Driver: driver, Metadata: res.Metadata,
			})
		})
	}
	if err := hostPathUnderBase(res.HostPath, l.BaseDir); err != nil {
		release()
		return "", nil, err
	}
	return res.HostPath, release, nil
}

// hostPathUnderBase mirrors the check Cubelet applies for sandbox attaches:
// a plugin must return an absolute directory strictly inside the base dir.
func hostPathUnderBase(hostPath, baseDir string) error {
	if hostPath == "" || !filepath.IsAbs(hostPath) {
		return fmt.Errorf("plugin returned invalid host path")
	}
	rel, err := filepath.Rel(filepath.Clean(baseDir), filepath.Clean(hostPath))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("plugin host path is outside the volume base directory")
	}
	return nil
}
