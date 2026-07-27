# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
snapshot_resume.py — checkpoint a long-running Go job, then resume from it.

The scenario: a job processes steps 1..10 and persists its progress so it can
pick up where a previous run stopped. Halfway through, the run crashes and
leaves a corrupted record behind. Instead of re-running from zero, we roll the
whole sandbox back to a checkpoint taken at step 5 — memory and filesystem
alike — and finish the job from there.

Steps:
  1. Boot a sandbox and build the job binary
  2. Run steps 1..5, then `create_snapshot()` — this is the checkpoint
  3. Run on toward step 10 but crash at step 8, corrupting the result log
  4. Show the dirty state (progress 7, a CORRUPTED line in the log)
  5. `rollback(checkpoint)` — back to exactly step 5, corruption gone
  6. Resume to step 10 and verify the log is clean
"""

from cubesandbox import Sandbox

from env import TEMPLATE_ID, check

PROJECT_DIR = "/workspace/job"
STATE_DIR = "/workspace/state"

GO_MOD = """module cubejob

go 1.24
"""

JOB_GO = """// A resumable long-running job: it processes steps 1..N and persists its
// progress after each one, so a later run continues instead of restarting.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const stateDir = "/workspace/state"

var (
	progressFile = filepath.Join(stateDir, "progress.txt")
	resultFile   = filepath.Join(stateDir, "results.log")
)

func loadProgress() int {
	raw, err := os.ReadFile(progressFile)
	if err != nil {
		return 0
	}
	done, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return done
}

func appendResult(line string) error {
	f, err := os.OpenFile(resultFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func main() {
	until := flag.Int("until", 10, "process steps up to and including this one")
	failAt := flag.Int("fail-at", 0, "simulate a crash at this step (0 = never)")
	flag.Parse()

	if err := os.MkdirAll(stateDir, 0o777); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}

	done := loadProgress()
	fmt.Printf("resuming from step %d\\n", done)

	for step := done + 1; step <= *until; step++ {
		// Pretend each step is expensive — that is what makes re-running
		// from zero unattractive and a checkpoint worth taking.
		time.Sleep(50 * time.Millisecond)

		if *failAt != 0 && step == *failAt {
			// Crash *after* dirtying the log, so the leftover state is
			// genuinely inconsistent and has to be discarded.
			_ = appendResult(fmt.Sprintf("step %d CORRUPTED", step))
			fmt.Fprintf(os.Stderr, "fatal: step %d crashed\\n", step)
			os.Exit(1)
		}

		if err := appendResult(fmt.Sprintf("step %d ok", step)); err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(progressFile, []byte(strconv.Itoa(step)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
		fmt.Printf("step %d done\\n", step)
	}

	fmt.Printf("job finished at step %d\\n", loadProgress())
}
"""


def progress(sb) -> str:
    """Current progress counter as seen from outside the sandbox."""
    return sb.files.read(f"{STATE_DIR}/progress.txt").strip()


def results(sb) -> str:
    return sb.files.read(f"{STATE_DIR}/results.log").strip()


checkpoint_id = None

with Sandbox.create(template=TEMPLATE_ID) as sb:
    print(f"sandbox: {sb.sandbox_id}")

    # Step 1: build the job binary.
    check(sb.commands.run(f"mkdir -p {PROJECT_DIR} {STATE_DIR}"), "mkdir")
    sb.files.write(f"{PROJECT_DIR}/go.mod", GO_MOD)
    sb.files.write(f"{PROJECT_DIR}/job.go", JOB_GO)
    check(sb.commands.run("go build -o /workspace/job-bin .", cwd=PROJECT_DIR), "go build")
    print("build ok")

    # Step 2: run the first half, then checkpoint.
    r = check(sb.commands.run("/workspace/job-bin --until 5"), "job --until 5")
    print(r.stdout.strip())

    checkpoint = sb.create_snapshot()
    checkpoint_id = checkpoint.snapshot_id
    print(f"checkpoint at step {progress(sb)}: {checkpoint_id}")

    # Step 3: keep going, but crash at step 8. A non-zero exit is expected
    # here, so this one deliberately does not go through check().
    r = sb.commands.run("/workspace/job-bin --until 10 --fail-at 8")
    print(f"crashed as designed (exit_code={r.exit_code}): {r.stderr.strip()}")

    # Step 4: the sandbox is now in a dirty state.
    dirty_progress, dirty_results = progress(sb), results(sb)
    print(f"dirty progress: {dirty_progress}")
    assert dirty_progress == "7", f"expected progress 7, got {dirty_progress!r}"
    assert "CORRUPTED" in dirty_results, "expected a corrupted record in the log"

    # Step 5: roll back to the checkpoint. Same sandbox ID, restored state.
    sb.rollback(checkpoint_id)
    restored_progress, restored_results = progress(sb), results(sb)
    print(f"rolled back — progress: {restored_progress}")
    assert restored_progress == "5", f"expected progress 5, got {restored_progress!r}"
    assert "CORRUPTED" not in restored_results, "corruption survived the rollback"

    # Step 6: resume from the checkpoint and finish cleanly.
    r = check(sb.commands.run("/workspace/job-bin --until 10"), "job --until 10")
    print(r.stdout.strip())

    final_progress, final_results = progress(sb), results(sb)
    assert final_progress == "10", f"expected progress 10, got {final_progress!r}"
    assert "CORRUPTED" not in final_results, "corruption survived the rollback"
    assert final_results.count("ok") == 10, f"expected 10 ok records:\n{final_results}"
    print("final log:")
    print(final_results)

Sandbox.delete_snapshot(checkpoint_id)
print(f"snapshot deleted: {checkpoint_id}")
print("OK")
