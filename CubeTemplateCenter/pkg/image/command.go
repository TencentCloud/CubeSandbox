// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var executableLookPath = exec.LookPath

// progressEmitInterval throttles how often parsed progress is forwarded to the
// caller's ProgressFunc, so a chatty subprocess (skopeo refreshes its bars many
// times per second) does not translate into excessive downstream work.
const progressEmitInterval = 200 * time.Millisecond

// maxCaptureBytes bounds how much subprocess output is retained for error
// diagnostics. A subprocess streaming carriage-return progress redraws (or an
// adversarial registry) could otherwise emit unbounded output; we keep only the
// most recent bytes, which is where the actionable failure message lives.
const maxCaptureBytes = 256 * 1024

// boundedBuffer retains at most maxCaptureBytes of the most recent lines so the
// captured diagnostics cannot grow without bound.
type boundedBuffer struct {
	max   int
	lines []string
	size  int
}

func (b *boundedBuffer) add(line string) {
	if b.max <= 0 {
		return
	}
	if len(line)+1 > b.max {
		keep := b.max - 1
		if keep < 0 {
			keep = 0
		}
		line = line[len(line)-keep:]
	}
	b.lines = append(b.lines, line)
	b.size += len(line) + 1
	for b.size > b.max && len(b.lines) > 0 {
		b.size -= len(b.lines[0]) + 1
		b.lines = b.lines[1:]
	}
}

func (b *boundedBuffer) String() string {
	if len(b.lines) == 0 {
		return ""
	}
	return strings.Join(b.lines, "\n") + "\n"
}

func dockerLogin(ctx context.Context, configDir, imageRef, username, password string) error {
	registry := registryHostFromImageRef(imageRef)
	cmd := exec.CommandContext(ctx, "docker", "--config", configDir, "login", "-u", username, "--password-stdin", "--", registry)
	cmd.Stdin = strings.NewReader(password)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func dockerRun(ctx context.Context, configDir string, args ...string) error {
	_, err := dockerOutput(ctx, configDir, args...)
	return err
}

func dockerOutput(ctx context.Context, configDir string, args ...string) ([]byte, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if configDir != "" {
		cmdArgs = append(cmdArgs, "--config", configDir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func skopeoOutput(ctx context.Context, authFile string, args ...string) ([]byte, error) {
	// skopeo expects global-ish flags such as --authfile to follow the
	// subcommand (e.g. `skopeo inspect --authfile X docker://...`).
	cmdArgs := make([]string, 0, len(args)+2)
	if authFile != "" && len(args) > 0 {
		cmdArgs = append(cmdArgs, args[0], "--authfile", authFile)
		cmdArgs = append(cmdArgs, args[1:]...)
	} else {
		cmdArgs = append(cmdArgs, args...)
	}
	cmd := exec.CommandContext(ctx, "skopeo", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// dockerPull runs `docker [--config dir] pull -- imageRef`. When onProgress is
// nil it preserves the original buffered behaviour (dockerRun). When non-nil it
// streams stdout/stderr through a docker layer-status parser so callers can
// observe per-layer pull progress.
func dockerPull(ctx context.Context, configDir, imageRef string, onProgress ProgressFunc) error {
	if onProgress == nil {
		return dockerRun(ctx, configDir, "pull", "--", imageRef)
	}
	args := make([]string, 0, 4)
	if configDir != "" {
		args = append(args, "--config", configDir)
	}
	args = append(args, "pull", "--", imageRef)
	_, err := streamCommand(ctx, "", "docker", newDockerParser(), onProgress, args...)
	return err
}

// skopeoCopy runs `skopeo copy ...`. When onProgress is nil it preserves the
// original buffered behaviour (runCommand). When non-nil it streams output
// through a skopeo per-blob byte parser. totalHint seeds the total size so the
// percentage is stable before every blob has started transferring.
func skopeoCopy(ctx context.Context, args []string, onProgress ProgressFunc, totalHint int64) error {
	if onProgress == nil {
		return runCommand(ctx, "", "skopeo", args...)
	}
	_, err := streamCommand(ctx, "", "skopeo", newSkopeoParser(totalHint), onProgress, args...)
	return err
}

// streamCommand executes name+args, feeding every output line (split on both
// \n and \r so carriage-return progress redraws are observed) to parser and
// forwarding throttled progress snapshots to onProgress. The full combined
// output is captured and returned, and on failure is embedded in the error,
// matching the diagnostics callers get from CombinedOutput.
func streamCommand(ctx context.Context, dir, name string, parser progressParser, onProgress ProgressFunc, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var (
		mu          sync.Mutex
		buf         = &boundedBuffer{max: maxCaptureBytes}
		lastEmit    time.Time
		latest      PullProgress
		haveLatest  bool
		latestDirty bool
	)
	recordProgressLocked := func(p PullProgress) (PullProgress, bool) {
		if onProgress == nil {
			return PullProgress{}, false
		}
		latest = p
		haveLatest = true
		now := time.Now()
		if now.Sub(lastEmit) < progressEmitInterval {
			latestDirty = true
			return PullProgress{}, false
		}
		lastEmit = now
		latestDirty = false
		return p, true
	}
	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		sc.Split(scanLinesCR)
		for sc.Scan() {
			line := sc.Text()
			var (
				emitProgress PullProgress
				shouldEmit   bool
			)
			mu.Lock()
			buf.add(line)
			if parser != nil {
				if p, ok := parser.feed(line); ok {
					emitProgress, shouldEmit = recordProgressLocked(p)
				}
			}
			mu.Unlock()
			if shouldEmit {
				onProgress(emitProgress)
			}
		}
		// Always drain whatever remains (e.g. a single line longer than the
		// scanner's max token size aborts Scan with ErrTooLong). If we returned
		// without draining, the subprocess could block writing to a full pipe
		// and cmd.Wait would hang forever.
		_, _ = io.Copy(io.Discard, r)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scan(stdout) }()
	go func() { defer wg.Done(); scan(stderr) }()
	wg.Wait()
	waitErr := cmd.Wait()

	// Flush the final snapshot so consumers always observe the terminal
	// progress state even if the last update was throttled.
	var (
		emitProgress PullProgress
		shouldEmit   bool
	)
	mu.Lock()
	if onProgress != nil && haveLatest && latestDirty {
		emitProgress = latest
		shouldEmit = true
	}
	output := []byte(buf.String())
	mu.Unlock()
	if shouldEmit {
		onProgress(emitProgress)
	}

	if waitErr != nil {
		return output, fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// scanLinesCR is a bufio.SplitFunc that breaks input on either a line feed or a
// carriage return, so progress bars that redraw with \r are surfaced as
// discrete lines.
func scanLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		advance := i + 1
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			advance++
		}
		return advance, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
