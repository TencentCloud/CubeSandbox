// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

const (
	envdVersionExecIDPrefix = "cubesandbox-internal-probe-"
	// envdVersionExecTimeout caps the in-guest `envd --version` probe so a hung
	// or unresponsive guest can never stall snapshot/commit.
	envdVersionExecTimeout = 5 * time.Second
	// envdVersionOutputLimit bounds the captured stdout/stderr to defend against
	// an image that floods the probe with output.
	envdVersionOutputLimit = 4 << 10 // 4 KiB
)

// envdSemverRe extracts a semantic version (major.minor.patch) from arbitrary
// `envd --version` output.
var envdSemverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// boundedBuffer is a concurrency-safe io.Writer that retains at most limit bytes
// and silently discards the rest.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remain := b.limit - b.buf.Len(); remain > 0 {
		if len(p) > remain {
			b.buf.Write(p[:remain])
		} else {
			b.buf.Write(p)
		}
	}
	// Always report a full write so the IO copy loop never blocks/errors once
	// the cap is reached.
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// collectEnvdVersion runs `envd --version` inside the running guest of sandboxID
// via containerd task.Exec and returns the parsed semantic version.
//
// It is strictly best-effort: any failure, timeout, non-zero exit, or malformed
// output yields "" (the caller falls back to a default) and a warning log; it
// never returns an error and never interrupts the snapshot/commit main flow.
//
// Security: the command always executes inside the microVM guest (task.Exec),
// never on the host, so an untrusted custom-image binary stays confined to the
// sandbox.
func (s *service) collectEnvdVersion(ctx context.Context, sandboxID string) (version string) {
	logger := log.G(ctx).WithField("sandboxID", sandboxID)

	// Self-contained panic guard: this runs inside the AppSnapshot/CommitSandbox
	// success path, so a panic here must NOT bubble up and fail an already-good
	// snapshot. Swallow it and degrade to the empty/fallback version.
	defer func() {
		if r := recover(); r != nil {
			logger.Warnf("collect envd version: recovered from panic: %v", r)
			version = ""
		}
	}()

	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		logger.Warnf("collect envd version: get cubebox failed: %v", err)
		return ""
	}
	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	execCtx, cancel := context.WithTimeout(namespaces.WithNamespace(ctx, ns), envdVersionExecTimeout)
	defer cancel()

	container, err := s.cubeboxMgr.client.LoadContainer(execCtx, sandboxID)
	if err != nil {
		logger.Warnf("collect envd version: load container failed: %v", err)
		return ""
	}
	task, err := container.Task(execCtx, nil)
	if err != nil {
		logger.Warnf("collect envd version: get task failed: %v", err)
		return ""
	}
	spec, err := container.Spec(execCtx)
	if err != nil {
		logger.Warnf("collect envd version: get container spec failed: %v", err)
		return ""
	}
	if spec.Process == nil {
		logger.Warnf("collect envd version: container spec has no process")
		return ""
	}
	pspecCopy := *spec.Process
	pspecCopy.Env = append([]string{}, spec.Process.Env...)
	pspec := &pspecCopy
	pspec.Terminal = true
	pspec.Args = []string{"envd", "--version"}

	out := &boundedBuffer{limit: envdVersionOutputLimit}
	execID := envdVersionExecIDPrefix + uuid.New().String()
	process, err := task.Exec(execCtx, execID, pspec, cio.NewCreator(cio.WithStreams(nil, out, out), cio.WithTerminal))
	if err != nil {
		logger.Warnf("collect envd version: exec failed: %v", err)
		return ""
	}
	defer func() {
		deleteCtx := namespaces.WithNamespace(context.Background(), ns)
		if _, derr := process.Delete(deleteCtx); derr != nil {
			logger.Warnf("collect envd version: delete exec process failed: %v", derr)
		}
	}()

	statusCh, err := process.Wait(execCtx)
	if err != nil {
		logger.Warnf("collect envd version: wait failed: %v", err)
		return ""
	}
	if err := process.Start(execCtx); err != nil {
		logger.Warnf("collect envd version: start failed: %v", err)
		return ""
	}

	select {
	case <-execCtx.Done():
		// Kill the exec process and wait for it to reap so the deferred Delete
		// does not race a still-running process and leak shim resources.
		_ = process.Kill(context.Background(), syscall.SIGKILL)
		select {
		case <-statusCh:
		case <-time.After(envdVersionExecTimeout):
			logger.Warnf("collect envd version: exec did not reap after kill")
		}
		logger.Warnf("collect envd version: timed out after %s", envdVersionExecTimeout)
		return ""
	case status := <-statusCh:
		if code, _, serr := status.Result(); serr != nil || code != 0 {
			logger.Warnf("collect envd version: non-zero exit (code=%d err=%v)", code, serr)
			return ""
		}
	}

	version = envdSemverRe.FindString(out.String())
	if version == "" {
		logger.Warnf("collect envd version: no semver in output")
		return ""
	}
	logger.Infof("collect envd version: %s", version)
	return version
}
