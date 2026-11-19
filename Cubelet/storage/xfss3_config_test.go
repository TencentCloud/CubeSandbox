// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

func TestXFSS3ConfigPathOrder(t *testing.T) {
	t.Setenv("CUBE_XFSS3_CONFIG", "")
	t.Setenv("RCOW_S3_CFG", "")
	if got := xfss3ConfigPath(); got != checkpoint.DefaultConfigPath {
		t.Fatalf("default: %s", got)
	}
	t.Setenv("RCOW_S3_CFG", "/etc/rcow/s3.cfg")
	if got := xfss3ConfigPath(); got != "/etc/rcow/s3.cfg" {
		t.Fatalf("one-click override ignored: %s", got)
	}
	t.Setenv("CUBE_XFSS3_CONFIG", "/etc/xfss3.cfg")
	if got := xfss3ConfigPath(); got != "/etc/xfss3.cfg" {
		t.Fatalf("explicit setting must win: %s", got)
	}
}

func TestXFSS3ReadyReportsAnUnusableStore(t *testing.T) {
	prevNew, prevVal, prevProbe := xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional
	t.Cleanup(func() {
		xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional = prevNew, prevVal, prevProbe
		xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = false, nil, false
	})
	xfss3ClientVal = nil
	xfss3ProbeConditional = func(context.Context, *checkpoint.Client) error { return errors.New("connection refused") }
	xfss3NewClient = func() (*checkpoint.Client, error) { return nil, errors.New("open /data/cubelet/s3.cfg: no such file") }
	if err := XFSS3Ready(); err == nil {
		t.Fatal("a node without a store must not accept an xfs-s3 pause")
	}
	xfss3NewClient = func() (*checkpoint.Client, error) {
		return checkpoint.NewClient(checkpoint.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	}
	if err := XFSS3Ready(); err != nil {
		t.Fatalf("usable store rejected: %v", err)
	}
}

func TestXFSS3PauseRefusedOnceTheStoreIsSeenToOverwrite(t *testing.T) {
	prevNew, prevVal, prevProbe := xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional
	t.Cleanup(func() {
		xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional = prevNew, prevVal, prevProbe
		xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = false, nil, false
	})
	xfss3ClientVal = nil
	xfss3NewClient = func() (*checkpoint.Client, error) {
		return checkpoint.NewClient(checkpoint.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	}
	c, err := xfss3Client()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	answer := errors.New("dial tcp 127.0.0.1:9: connection refused")
	xfss3ProbeConditional = func(context.Context, *checkpoint.Client) error {
		calls++
		return answer
	}

	// A store that cannot be asked holds up neither pause nor a later probe;
	// the pause asks it again itself.
	if err := xfss3CheckConditionalWrites(context.Background(), c); err == nil {
		t.Fatal("an unanswered probe is not a yes")
	}
	if err := XFSS3Ready(); err != nil {
		t.Fatalf("pause refused while the store cannot be asked: %v", err)
	}

	answer = checkpoint.ErrConditionalWriteUnsupported
	if err := xfss3CheckConditionalWrites(context.Background(), c); !errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
		t.Fatalf("got %v", err)
	}
	if err := XFSS3Ready(); !errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
		t.Fatalf("pause accepted on a store that overwrites commits: %v", err)
	}
	_ = xfss3CheckConditionalWrites(context.Background(), c)
	if calls != 3 {
		t.Fatalf("the store's answer is kept: probed %d times", calls)
	}
}

func TestXFSS3ReadyAsksTheStoreBeforeTheProbeAnswered(t *testing.T) {
	prevNew, prevVal, prevProbe := xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional
	t.Cleanup(func() {
		xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional = prevNew, prevVal, prevProbe
		xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = false, nil, false
	})
	xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = false, nil, false
	xfss3ClientVal = nil
	xfss3NewClient = func() (*checkpoint.Client, error) {
		return checkpoint.NewClient(checkpoint.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	}
	calls := 0
	xfss3ProbeConditional = func(context.Context, *checkpoint.Client) error {
		calls++
		return checkpoint.ErrConditionalWriteUnsupported
	}
	// A Pause that comes before the startup probe has answered asks the
	// store itself and is refused before the freeze, not after it.
	if err := XFSS3Ready(); !errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
		t.Fatalf("pause accepted before the store was asked: %v", err)
	}
	if err := XFSS3Ready(); !errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) || calls != 1 {
		t.Fatalf("the answer is kept: %v after %d probes", err, calls)
	}
}

func TestXFSS3OptionalConditionalWritesDegradeInsteadOfRefusing(t *testing.T) {
	prevNew, prevVal, prevProbe := xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional
	t.Cleanup(func() {
		xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional = prevNew, prevVal, prevProbe
		xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = false, nil, false
	})
	xfss3ClientVal = nil
	xfss3NewClient = func() (*checkpoint.Client, error) {
		return checkpoint.NewClient(checkpoint.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "a", SecretKey: "s",
			ConditionalWrites: checkpoint.ConditionalOptional})
	}
	xfss3ProbeConditional = func(context.Context, *checkpoint.Client) error { return checkpoint.ErrConditionalWriteUnsupported }
	c, err := xfss3Client()
	if err != nil {
		t.Fatal(err)
	}
	if err := xfss3CheckConditionalWrites(context.Background(), c); err != nil {
		t.Fatalf("an accepted store without If-None-Match must not fail the export: %v", err)
	}
	if err := XFSS3Ready(); err != nil {
		t.Fatalf("pause refused on an accepted store: %v", err)
	}
	if c.ConditionalWritesInUse() || !xfss3CommitsDegraded() {
		t.Fatal("commits must switch to the degraded path")
	}
}

func TestXFSS3TransferWorkersComeFromTheConfigUnlessOverridden(t *testing.T) {
	newClient := func(export, read int) *checkpoint.Client {
		c, err := checkpoint.NewClient(checkpoint.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "a", SecretKey: "s",
			ExportWorkers: export, ReadWorkers: read})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Setenv("CUBE_XFSS3_EXPORT_WORKERS", "")
	t.Setenv("CUBE_XFSS3_READ_WORKERS", "")
	if e, r := xfss3ExportWorkers(newClient(0, 0)), xfss3ReadWorkers(newClient(0, 0)); e != 32 || r != 32 {
		t.Fatalf("defaults %d/%d, want 32/32", e, r)
	}
	if e, r := xfss3ExportWorkers(newClient(8, 12)), xfss3ReadWorkers(newClient(8, 12)); e != 8 || r != 12 {
		t.Fatalf("configured %d/%d, want 8/12", e, r)
	}
	// The environment wins over the configuration, on this node only.
	t.Setenv("CUBE_XFSS3_EXPORT_WORKERS", "20")
	t.Setenv("CUBE_XFSS3_READ_WORKERS", "64")
	if e, r := xfss3ExportWorkers(newClient(8, 12)), xfss3ReadWorkers(newClient(8, 12)); e != 20 || r != 64 {
		t.Fatalf("overridden %d/%d, want 20/64", e, r)
	}
	// An override out of range is ignored.
	for _, bad := range []string{"0", "65", "many", "-3"} {
		t.Setenv("CUBE_XFSS3_EXPORT_WORKERS", bad)
		if n := xfss3ExportWorkers(newClient(8, 12)); n != 8 {
			t.Errorf("CUBE_XFSS3_EXPORT_WORKERS=%s gave %d, want the configured 8", bad, n)
		}
	}
}

func TestXFSS3ReadyHandlesRejectedConditionalHeaders(t *testing.T) {
	prevNew, prevVal, prevProbe := xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional
	prevKnown, prevErr, prevDegraded := xfss3CondKnown, xfss3CondErr, xfss3CondDegraded
	t.Cleanup(func() {
		xfss3NewClient, xfss3ClientVal, xfss3ProbeConditional = prevNew, prevVal, prevProbe
		xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = prevKnown, prevErr, prevDegraded
	})
	for _, code := range []int{400, 403, 405, 501} {
		for _, mode := range []string{checkpoint.ConditionalRequired, checkpoint.ConditionalOptional} {
			t.Run(fmt.Sprintf("http-%d/%s", code, mode), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("If-None-Match") != "" {
						w.WriteHeader(code)
					}
				}))
				defer srv.Close()
				xfss3ClientVal = nil
				xfss3CondKnown, xfss3CondErr, xfss3CondDegraded = false, nil, false
				xfss3NewClient = func() (*checkpoint.Client, error) {
					return checkpoint.NewClient(checkpoint.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", PathStyle: true, ConditionalWrites: mode})
				}
				xfss3ProbeConditional = func(ctx context.Context, c *checkpoint.Client) error { return c.ProbeConditionalWrite(ctx) }
				err := XFSS3Ready()
				if mode == checkpoint.ConditionalRequired {
					if !errors.Is(err, checkpoint.ErrConditionalWriteUnsupported) {
						t.Fatalf("pause accepted: %v", err)
					}
				} else {
					if err != nil {
						t.Fatalf("optional probe failed: %v", err)
					}
					if !xfss3CommitsDegraded() || xfss3ClientVal.ConditionalWritesInUse() {
						t.Fatal("optional mode did not degrade")
					}
				}
			})
		}
	}
}
