// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
)

// leasePlugin is a builtin volume plugin that "mounts" a directory.
type leasePlugin struct {
	name       string
	hostPath   func(req *volume.AttachRequest) string
	attachErr  error
	attached   atomic.Int64
	detached   atomic.Int64
	lastAttach atomic.Value // *volume.AttachRequest
	lastDetach atomic.Value // *volume.DetachRequest
}

func (p *leasePlugin) Name() string                                    { return p.name }
func (p *leasePlugin) PluginType() volume.PluginType                   { return volume.PluginTypeBuiltin }
func (p *leasePlugin) Init(context.Context, volume.PluginConfig) error { return nil }
func (p *leasePlugin) Close() error                                    { return nil }

func (p *leasePlugin) Attach(_ context.Context, req *volume.AttachRequest) (*volume.AttachResult, error) {
	if p.attachErr != nil {
		return nil, p.attachErr
	}
	p.attached.Add(1)
	cp := *req
	p.lastAttach.Store(&cp)
	path := p.hostPath(req)
	_ = os.MkdirAll(path, 0o755)
	return &volume.AttachResult{VolumeID: req.VolumeID, HostPath: path, Metadata: map[string]string{"k": "v"}}, nil
}

func (p *leasePlugin) Detach(_ context.Context, req *volume.DetachRequest) error {
	p.detached.Add(1)
	cp := *req
	p.lastDetach.Store(&cp)
	return nil
}

var (
	leaseGood = &leasePlugin{name: "lease-good", hostPath: func(r *volume.AttachRequest) string {
		return filepath.Join(r.VolumeBaseDir, "lease-good-"+r.VolumeID)
	}}
	leaseEscape = &leasePlugin{name: "lease-escape", hostPath: func(r *volume.AttachRequest) string {
		return filepath.Join(r.VolumeBaseDir, "..", "elsewhere")
	}}
	leaseBroken = &leasePlugin{name: "lease-broken", attachErr: errors.New("mount failed")}

	registerLeasePlugins sync.Once
)

func TestManagerLeaser(t *testing.T) {
	// The global Manager has no unregister, so register once per process.
	registerLeasePlugins.Do(func() {
		for _, p := range []*leasePlugin{leaseGood, leaseEscape, leaseBroken} {
			volume.Register(p)
		}
	})
	good, escape, broken := leaseGood, leaseEscape, leaseBroken
	base := t.TempDir()

	l := &ManagerLeaser{Mgr: volume.Global(), Namespace: "default", BaseDir: base}
	ctx := context.Background()

	if _, _, err := l.Acquire(ctx, "vol-1", "no-such-driver", ""); !errors.Is(err, ErrDriverUnavailable) {
		t.Fatalf("unknown driver: %v", err)
	}
	if _, _, err := l.Acquire(ctx, "vol-1", "", ""); !errors.Is(err, ErrDriverUnavailable) {
		t.Fatalf("empty driver: %v", err)
	}

	attachedBefore, detachedBefore := good.attached.Load(), good.detached.Load()
	host, release, err := l.Acquire(ctx, "vol-1", "lease-good", "volumes/vol-1/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(host, base) {
		t.Fatalf("host path %q not under %q", host, base)
	}
	req := good.lastAttach.Load().(*volume.AttachRequest)
	if req.PrivateData != "volumes/vol-1/" || req.Namespace != "default" || req.VolumeBaseDir != base ||
		!strings.HasPrefix(req.SandboxID, "volcontent-") {
		t.Fatalf("attach request: %+v", req)
	}
	if good.detached.Load() != detachedBefore {
		t.Fatal("detached before release")
	}
	release()
	release() // idempotent
	if good.attached.Load()-attachedBefore != 1 || good.detached.Load()-detachedBefore != 1 {
		t.Fatalf("attach/detach counts: %d/%d", good.attached.Load()-attachedBefore, good.detached.Load()-detachedBefore)
	}
	det := good.lastDetach.Load().(*volume.DetachRequest)
	if det.SandboxID != req.SandboxID || det.Metadata["k"] != "v" {
		t.Fatalf("detach must reuse the lease holder and metadata: %+v", det)
	}

	escapeDetached := escape.detached.Load()
	if _, _, err := l.Acquire(ctx, "vol-2", "lease-escape", ""); err == nil {
		t.Fatal("plugin host path outside the base dir was accepted")
	}
	if escape.detached.Load()-escapeDetached != 1 {
		t.Fatal("rejected attach must be detached so the reference is not leaked")
	}
	if _, _, err := l.Acquire(ctx, "vol-3", "lease-broken", ""); err == nil || broken.detached.Load() != 0 {
		t.Fatalf("failed attach: err=%v detaches=%d", err, broken.detached.Load())
	}
}
