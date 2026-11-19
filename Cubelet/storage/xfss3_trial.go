// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

// A checkpoint is ready only after a node other than its origin has restored
// it. The verifier downloads the committed session in full into a private
// trial directory (which is also the complete readback: every chunk is
// checked against the manifest on the way in) and hands the files to
// `cube-runtime verify-restore`. That command runs in a network namespace of
// its own, created at exec, so the restored guest has no route to anything,
// and it checks the guest agent the way a production resume does. Nothing it
// touches is shared with production sandboxes: its VM id, run directory,
// vsock socket, disks and memory are all private copies.

const (
	xfss3TrialDirName      = "checkpoint-trial"
	xfss3TrialAttemptFile  = "attempt.json"
	xfss3TrialTimeout      = 3 * time.Minute
	xfss3TrialOutputLimit  = 1 << 20
	xfss3VMRunDir          = "/run/vc/vm"
	xfss3DefaultRuntimeBin = "/usr/local/services/cubetoolbox/cube-shim/bin/cube-runtime"
	xfss3TrialResultPrefix = "VERIFY_RESULT "
)

var (
	// One trial at a time per node: each maps a full guest memory image.
	xfss3TrialSlot = make(chan struct{}, 1)
	xfss3SpecDirRE = regexp.MustCompile(`^(\d+)C(\d+)M$`)
	// xfss3RunTrial is a test hook.
	xfss3RunTrial = runXFSS3TrialProcess
)

// XFSS3TrialResult is what cube-runtime verify-restore reports.
type XFSS3TrialResult struct {
	OK               bool     `json:"ok"`
	Stage            string   `json:"stage"`
	Error            string   `json:"error"`
	RestoreMS        int64    `json:"restore_ms"`
	VsockReadyMS     int64    `json:"vsock_ready_ms"`
	AgentMS          int64    `json:"agent_ms"`
	AgentAttempts    int      `json:"agent_attempts"`
	NetnsInterfaces  []string `json:"netns_interfaces"`
	DownloadMS       int64    `json:"download_ms"`
	TrialID          string   `json:"trial_id"`
	ProcessReleased  bool     `json:"process_released"`
	CleanupCompleted bool     `json:"cleanup_completed"`
}

// errXFSS3TrialDownload marks a trial that could not fetch the checkpoint:
// the store, not this node's ability to restore it.
var errXFSS3TrialDownload = errors.New("trial download failed")

// trialVerdict says what a failed trial means for the checkpoint.
type trialVerdict int

const (
	// trialRetry: this node or this moment could not run the trial; the
	// checkpoint stays pending and another attempt follows.
	trialRetry trialVerdict = iota
	// trialDefect: the checkpoint itself cannot be restored anywhere.
	trialDefect
)

func classifyTrial(r *XFSS3TrialResult) trialVerdict {
	switch r.Stage {
	case "config":
		// Unmapped writable disks, writable pmem, ivshmem or passthrough
		// devices: the package does not carry what a restore needs.
		return trialDefect
	case "restore":
		// The data was verified and the failure is neither a host
		// incompatibility nor resource exhaustion (those are reported as
		// compat and infra). It may still be this node: an image, a kernel
		// setting, a VMM build. One node cannot tell, so Master fails the
		// checkpoint only once two different nodes failed to restore it.
		return trialRetry
	default:
		// isolation, compat, deps, infra, agent: this node or this moment
		// could not run the trial. That says nothing about the checkpoint,
		// so it stays pending and is retried, never failed.
		return trialRetry
	}
}

// trialRetryPrefix marks a pending status whose trial could not run on
// this node. Master moves such a checkpoint to another verifier when the
// stage is about the node (compat, deps, isolation).
func trialRetryPrefix(stage string) string {
	if stage == "" {
		stage = "unknown"
	}
	return "retry(" + stage + "): "
}

const (
	xfss3TrialBackoffMin = 30 * time.Second
	xfss3TrialBackoffMax = 10 * time.Minute
)

// xfss3TrialBackoff spaces out retries of a trial that could not run: each
// one downloads the whole checkpoint again.
func xfss3TrialBackoff(failures int) time.Duration {
	d := xfss3TrialBackoffMin
	for i := 1; i < failures && d < xfss3TrialBackoffMax; i++ {
		d *= 2
	}
	return min(d, xfss3TrialBackoffMax)
}

type xfss3TrialAttempt struct {
	TrialID    string `json:"trial_id"`
	SnapshotID string `json:"snapshot_id"`
	Session    string `json:"session"`
	StartedAt  string `json:"started_at"`
	PID        int    `json:"pid,omitempty"`
}

func xfss3TrialRoot() string { return filepath.Join(XFSObjectsDir(), xfss3TrialDirName) }

func xfss3RuntimeBin() string {
	if p := strings.TrimSpace(os.Getenv("CUBE_XFSS3_RUNTIME")); p != "" {
		return p
	}
	return xfss3DefaultRuntimeBin
}

// writableDiskPaths lists the writable disks the snapshot recorded. The
// package carries exactly one, its rootfs.
func writableDiskPaths(configJSON []byte) ([]string, error) {
	var cfg struct {
		Disks []struct {
			Path     string `json:"path"`
			Readonly bool   `json:"readonly"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("decode snapshot vm config: %w", err)
	}
	var out []string
	for _, d := range cfg.Disks {
		if !d.Readonly {
			out = append(out, d.Path)
		}
	}
	return out, nil
}

// trialXFSS3 downloads m into a private directory and restores it there in
// isolation. A nil error means the trial passed. The returned verdict tells
// a failure apart: a defect fails the checkpoint, anything else keeps it
// pending.
func trialXFSS3(ctx context.Context, client *checkpoint.Client, m *checkpoint.Manifest, entry *SnapshotCatalogEntry,
	progress *checkpoint.Progress) (*XFSS3TrialResult, trialVerdict, error) {
	select {
	case xfss3TrialSlot <- struct{}{}:
		defer func() { <-xfss3TrialSlot }()
	case <-ctx.Done():
		return nil, trialRetry, ctx.Err()
	}
	spec := xfss3SpecDirRE.FindStringSubmatch(strings.TrimSpace(entry.SpecDir))
	if spec == nil {
		return nil, trialDefect, fmt.Errorf("%w: spec dir %q has no cpu/memory", checkpoint.ErrCorrupt, entry.SpecDir)
	}
	trialID := "ckpt-trial-" + strings.ReplaceAll(newXFSS3Session(), "t", "")
	dir := filepath.Join(xfss3TrialRoot(), trialID)
	home := filepath.Join(dir, "home")
	meta := filepath.Join(home, SnapshotMetadataDir)
	vols := filepath.Join(dir, "vol")
	share := filepath.Join(dir, "share")
	res := &XFSS3TrialResult{TrialID: trialID}
	cleaned := false
	defer func() {
		// Every failure path still removes the trial; a passed trial has
		// already done so and may only pass if that worked.
		if !cleaned {
			res.CleanupCompleted = removeXFSS3TrialFn(trialID) == nil
		}
	}()
	for _, d := range []string{meta, vols, share} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, trialRetry, err
		}
	}
	// Recorded before anything runs, so a Cubelet restart can find and
	// reap whatever this attempt leaves behind.
	attempt := &xfss3TrialAttempt{TrialID: trialID, SnapshotID: m.SnapshotID, Session: m.Session,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeXFSS3TrialAttempt(dir, attempt); err != nil {
		return res, trialRetry, err
	}

	start := time.Now()
	err := checkpoint.Materialize(ctx, client, m, xfss3ReadWorkers(client), progress,
		func(f *checkpoint.File) (string, error) {
			if f.Role == checkpoint.RoleVolume {
				return filepath.Join(vols, f.Name), nil
			}
			p := filepath.Join(meta, filepath.FromSlash(f.Name))
			return p, os.MkdirAll(filepath.Dir(p), 0o700)
		})
	res.DownloadMS = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, checkpoint.ErrCorrupt) {
			return res, trialDefect, err
		}
		return res, trialRetry, fmt.Errorf("%w: %v", errXFSS3TrialDownload, err)
	}
	if err := EnsureShimSpecDirLink(home, entry.SpecDir); err != nil {
		return res, trialRetry, err
	}
	cfgRaw, err := os.ReadFile(filepath.Join(meta, "snapshot", "config.json")) // NOCC:Path Traversal()
	if err != nil {
		return res, trialDefect, fmt.Errorf("%w: read snapshot vm config: %v", checkpoint.ErrCorrupt, err)
	}
	disks, err := writableDiskPaths(cfgRaw)
	if err != nil {
		return res, trialDefect, fmt.Errorf("%w: %v", checkpoint.ErrCorrupt, err)
	}
	if len(disks) != 1 {
		return res, trialDefect, fmt.Errorf("package carries one rootfs but the snapshot has %d writable disks", len(disks))
	}
	args := []string{
		"verify-restore",
		"--base", home,
		"--vm-id", trialID,
		"--cpu", spec[1],
		"--memory", spec[2],
		"--memory-vol", "file://" + filepath.Join(vols, entry.MemoryVol),
		"--disk", disks[0] + "=" + filepath.Join(vols, entry.RootfsVol),
		"--share-dir", share,
	}
	out, released, runErr := xfss3RunTrial(ctx, dir, attempt, args)
	res.ProcessReleased = released
	if parsed := parseXFSS3TrialOutput(out); parsed != nil {
		parsed.DownloadMS, parsed.TrialID, parsed.ProcessReleased = res.DownloadMS, res.TrialID, res.ProcessReleased
		*res = *parsed
	}
	switch {
	case !released:
		// Never report a verdict while the trial guest may still run.
		return res, trialRetry, fmt.Errorf("trial %s process was not confirmed gone: %v", trialID, runErr)
	case res.OK && runErr == nil:
		cleaned = true
		if err := finishPassedTrial(res, trialID); err != nil {
			return res, trialRetry, err
		}
		return res, trialRetry, nil
	case res.Stage == "":
		return res, trialRetry, fmt.Errorf("trial %s produced no result: %v; output: %s", trialID, runErr, tail(out, 600))
	default:
		return res, classifyTrial(res), fmt.Errorf("trial restore failed at %s: %s", res.Stage, res.Error)
	}
}

// removeXFSS3TrialFn is a test hook.
var removeXFSS3TrialFn = removeXFSS3Trial

// finishPassedTrial removes a passed trial's files and run directory. The
// trial only counts once nothing of it is left: a full guest image and its
// run directory must not outlive a ready checkpoint.
func finishPassedTrial(res *XFSS3TrialResult, trialID string) error {
	if err := removeXFSS3TrialFn(trialID); err != nil {
		res.CleanupCompleted = false
		return fmt.Errorf("trial %s passed but its files were not removed: %w", trialID, err)
	}
	res.CleanupCompleted = true
	return nil
}

// xfss3TrialPassed is the gate before evidence and ready: the guest
// answered, its process is confirmed gone and its files are removed.
func xfss3TrialPassed(res *XFSS3TrialResult) error {
	switch {
	case res == nil || !res.OK:
		return errors.New("trial restore did not pass")
	case !res.ProcessReleased:
		return errors.New("trial process was not confirmed gone")
	case !res.CleanupCompleted:
		return errors.New("trial files were not removed")
	}
	return nil
}

func parseXFSS3TrialOutput(out []byte) *XFSS3TrialResult {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), xfss3TrialOutputLimit)
	var res *XFSS3TrialResult
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, xfss3TrialResultPrefix); i >= 0 {
			var r XFSS3TrialResult
			if json.Unmarshal([]byte(line[i+len(xfss3TrialResultPrefix):]), &r) == nil {
				res = &r
			}
		}
	}
	return res
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

func writeXFSS3TrialAttempt(dir string, a *xfss3TrialAttempt) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return writeFileDurable(filepath.Join(dir, xfss3TrialAttemptFile), raw, 0o600)
}

// limitedBuffer keeps the last limit bytes a process writes, including the
// VERIFY_RESULT line printed after the runtime logs.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit > 0 {
		if len(p) >= b.limit {
			b.buf.Reset()
			pTail := p[len(p)-b.limit:]
			b.buf.Write(pTail)
		} else {
			if drop := b.buf.Len() + len(p) - b.limit; drop > 0 {
				b.buf.Next(drop)
			}
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// processGone reports whether pid no longer runs the trial trialID.
func processGone(pid int, trialID string) bool {
	if pid <= 0 {
		return true
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return true
	}
	return !bytes.Contains(cmdline, []byte(trialID))
}

// removeXFSS3Trial deletes a trial's files and run directory. It refuses
// while the trial process still runs.
func removeXFSS3Trial(trialID string) error {
	if !strings.HasPrefix(trialID, "ckpt-trial-") || strings.ContainsAny(trialID, `/\.`) {
		return fmt.Errorf("refusing to remove trial %q", trialID)
	}
	dir := filepath.Join(xfss3TrialRoot(), trialID)
	if raw, err := os.ReadFile(filepath.Join(dir, xfss3TrialAttemptFile)); err == nil { // NOCC:Path Traversal()
		var a xfss3TrialAttempt
		if json.Unmarshal(raw, &a) == nil && !processGone(a.PID, trialID) {
			return fmt.Errorf("trial %s process %d still runs", trialID, a.PID)
		}
	}
	errRun := os.RemoveAll(filepath.Join(xfss3VMRunDir, trialID))
	errDir := os.RemoveAll(dir)
	return errors.Join(errRun, errDir)
}

// CleanupXFSS3Trials reaps what trials of a previous Cubelet left behind:
// a still-running trial process is killed, then its files go.
func CleanupXFSS3Trials(ctx context.Context) {
	entries, err := os.ReadDir(xfss3TrialRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !strings.HasPrefix(id, "ckpt-trial-") {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(xfss3TrialRoot(), id, xfss3TrialAttemptFile)) // NOCC:Path Traversal()
		var a xfss3TrialAttempt
		_ = json.Unmarshal(raw, &a)
		if !processGone(a.PID, id) {
			_ = syscall.Kill(a.PID, syscall.SIGKILL)
			for i := 0; i < 50 && !processGone(a.PID, id); i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if err := removeXFSS3Trial(id); err != nil {
			log.G(ctx).Warnf("xfs-s3: leftover trial %s not removed: %v", id, err)
			continue
		}
		log.G(ctx).Infof("xfs-s3: removed leftover trial %s of %s", id, a.SnapshotID)
	}
}
