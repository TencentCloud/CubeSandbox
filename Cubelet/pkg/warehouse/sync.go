// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"golang.org/x/sync/singleflight"
)

const (
	inventoryPutTimeout = 15 * time.Second
	jobAckTimeout       = 15 * time.Second
)

// Fetcher downloads a missing inventory version from CubeOps.
type Fetcher struct {
	client    *Client
	base      string
	flight    singleflight.Group
	reportMu  sync.Mutex
	lastAck   []InventoryItem
	lastAckOK bool
}

func NewFetcher(client *Client, baseDir string) *Fetcher {
	if baseDir == "" {
		baseDir = templatetypes.DefaultVersionedBaseDir
	}
	return &Fetcher{client: client, base: baseDir}
}

func (f *Fetcher) Fetch(ctx context.Context, name, version string) error {
	if f == nil || f.client == nil {
		return fmt.Errorf("%w: cubeops_addr is not configured", ErrNotFound)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	name = strings.TrimSpace(name)
	version = templatetypes.InventoryVersionKey(version)
	if name == "" || version == "" {
		return fmt.Errorf("component or version is empty")
	}
	ch := f.flight.DoChan(name+"/"+version, func() (interface{}, error) {
		timeout := f.client.downloadTimeout()
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		return nil, f.fetchOnce(flightCtx, name, version)
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		select {
		case r := <-ch:
			return r.Err
		default:
			return ctx.Err()
		}
	}
}

func (f *Fetcher) fetchOnce(ctx context.Context, name, version string) error {
	if !destDirExists(f.base, name, version) {
		ref, err := f.client.ResolveBlob(ctx, name, version)
		if err != nil {
			return err
		}
		body, err := f.client.OpenBlob(ctx, ref)
		if err != nil {
			return err
		}
		defer body.Close()
		if err := InstallBlob(ctx, f.base, name, version, body, ref.Checksum, ref.SizeBytes); err != nil {
			return err
		}
	}
	f.reportInventoryBestEffort(ctx)
	return nil
}

func (f *Fetcher) reportInventoryBestEffort(ctx context.Context) {
	if err := f.syncInventory(ctx); err != nil {
		log.G(ctx).WithField("mod", "warehouse").Warnf("report inventory failed: %v", err)
	}
}

// ScanAndReport walks local inventory and replaces the CubeOps snapshot for this node.
func (f *Fetcher) ScanAndReport(ctx context.Context) {
	if f == nil || f.client == nil {
		return
	}
	f.reportInventoryBestEffort(ctx)
}

func (f *Fetcher) syncInventory(ctx context.Context) error {
	if f == nil || f.client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	f.reportMu.Lock()
	defer f.reportMu.Unlock()
	items := collectValidDirs(f.base)
	if f.lastAckOK && sameInventorySet(items, f.lastAck) {
		return nil
	}
	putCtx, cancel := context.WithTimeout(ctx, inventoryPutTimeout)
	defer cancel()
	if err := f.client.PutInventory(putCtx, items); err != nil {
		return err
	}
	f.lastAck = cloneInventory(items)
	f.lastAckOK = true
	return nil
}

func collectValidDirs(baseDir string) []InventoryItem {
	var items []InventoryItem
	for _, name := range []string{
		templatetypes.CubeComponentCubeShim,
		templatetypes.CubeComponentCubeKernel,
		templatetypes.CubeComponentCubeImage,
		templatetypes.CubeComponentCubeAgent,
	} {
		compDir := filepath.Join(baseDir, name)
		entries, err := os.ReadDir(compDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "" || e.Name()[0] == '.' {
				continue
			}
			ver := templatetypes.InventoryVersionKey(e.Name())
			if ver == "" {
				continue
			}
			if err := validateTree(filepath.Join(compDir, e.Name()), name); err != nil {
				continue
			}
			items = append(items, InventoryItem{Component: name, Version: ver})
		}
	}
	return items
}

func sameInventorySet(a, b []InventoryItem) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, it := range a {
		seen[it.Component+"\x00"+it.Version] = struct{}{}
	}
	if len(seen) != len(a) {
		return false
	}
	for _, it := range b {
		if _, ok := seen[it.Component+"\x00"+it.Version]; !ok {
			return false
		}
	}
	return true
}

func cloneInventory(items []InventoryItem) []InventoryItem {
	if items == nil {
		return nil
	}
	out := make([]InventoryItem, len(items))
	copy(out, items)
	return out
}

// RunPreinstallLoop polls CubeOps for pending jobs and installs them.
func RunPreinstallLoop(stopCh <-chan struct{}, client *Client, fetcher *Fetcher, interval time.Duration) {
	if client == nil || fetcher == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		scanCtx, scanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		fetcher.ScanAndReport(scanCtx)
		scanCancel()
		pollOnce(client, fetcher)
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}
	}
}

func pollOnce(client *Client, fetcher *Fetcher) {
	listCtx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
	jobs, err := client.ListJobs(listCtx)
	listCancel()
	if err != nil {
		log.G(context.Background()).WithField("mod", "warehouse").Warnf("list preinstall jobs: %v", err)
		return
	}
	for _, job := range jobs {
		jobCtx, jobCancel := context.WithTimeout(context.Background(), preinstallJobBudget(client))
		ackJob(client, job.ID, "running", "")
		if err := fetcher.Fetch(jobCtx, job.Component, job.Version); err != nil {
			ackJob(client, job.ID, "failed", err.Error())
		} else {
			ackJob(client, job.ID, "succeeded", "")
		}
		jobCancel()
	}
}

func preinstallJobBudget(client *Client) time.Duration {
	timeout := 10 * time.Minute
	if client != nil {
		timeout = client.downloadTimeout()
	}
	return timeout + 2*time.Minute
}

func ackJob(client *Client, id, status, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), jobAckTimeout)
	defer cancel()
	if ackErr := client.AckJob(ctx, id, status, errMsg); ackErr != nil {
		log.G(ctx).WithField("mod", "warehouse").Warnf("ack job %s %s: %v", id, status, ackErr)
	}
}

func NodeArch() string {
	return runtime.GOARCH
}

func NodeID() string {
	id, err := utils.GetInstanceID()
	if err != nil {
		return ""
	}
	return id
}
