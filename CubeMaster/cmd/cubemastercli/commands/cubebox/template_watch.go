// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	xterm "github.com/charmbracelet/x/term"
	commands "github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/urfave/cli"
)

// defaultWatchInterval is used when a command does not define an --interval flag
// or it is left at zero.
const defaultWatchInterval = 2 * time.Second

// detachRequested reports whether the user asked to submit-and-exit instead of
// the new default of waiting for completion. --no-wait is an alias for
// --detach.
func detachRequested(c *cli.Context) bool {
	return c.Bool("detach") || c.Bool("no-wait")
}

// shouldUseTUI reports whether the rich bubbletea UI should be used. It is only
// enabled on an interactive terminal and never when machine-readable output
// (--json) or detached submission was requested, so pipes, CI and JSON
// consumers keep the original line-oriented behaviour.
func shouldUseTUI(c *cli.Context) bool {
	if c.Bool("json") || detachRequested(c) {
		return false
	}
	// Require both stdout (for the rendered frame) and stdin (so q/esc/ctrl+c
	// key handling works) to be terminals; otherwise fall back to plain output.
	return xterm.IsTerminal(os.Stdout.Fd()) && xterm.IsTerminal(os.Stdin.Fd())
}

func watchInterval(c *cli.Context) time.Duration {
	if d := c.Duration("interval"); d > 0 {
		return d
	}
	return defaultWatchInterval
}

// runImageJobWatch follows a create-from-image / redo job to completion. It uses
// the TUI when appropriate and otherwise falls back to plain line output.
func runImageJobWatch(c *cli.Context, jobID string) error {
	if !shouldUseTUI(c) {
		return runImageJobWatchPlain(c, jobID)
	}
	model, err := tea.NewProgram(newImageJobTUI(c, jobID)).Run()
	if err != nil {
		// If the TUI cannot start (e.g. terminal quirk), degrade gracefully.
		return runImageJobWatchPlain(c, jobID)
	}
	return finishImageJobWatch(c, jobID, model, runImageJobWatchPlain)
}

func finishImageJobWatch(c *cli.Context, jobID string, model tea.Model, fallback func(*cli.Context, string) error) error {
	final, ok := model.(imageJobTUI)
	if !ok {
		log.Printf("template watch TUI returned unexpected model type %T\n", model)
		return fallback(c, jobID)
	}
	if final.canceled {
		log.Printf("stopped watching; job still running, resume with: template watch --job-id %s\n", jobID)
		return nil
	}
	if final.job != nil {
		printTemplateImageJobCompletionSummary(final.job)
	}
	return final.fatal
}

func runImageJobWatchPlain(c *cli.Context, jobID string) error {
	interval := watchInterval(c)
	var lastPrinted string
	for {
		rsp, err := fetchTemplateImageJob(c, jobID)
		if err != nil {
			return err
		}
		if rsp.Job == nil {
			printTemplateImageJobWatchLine(nil)
			printTemplateImageJobCompletionSummary(nil)
			return errors.New("empty job")
		}
		current := formatTemplateImageJobWatchLine(rsp.Job)
		if current != lastPrinted {
			printTemplateImageJobWatchLine(rsp.Job)
			lastPrinted = current
		}
		if rsp.Job.Status == "READY" || rsp.Job.Status == "FAILED" {
			printTemplateImageJobCompletionSummary(rsp.Job)
			if c.Bool("json") {
				commands.PrintAsJSON(rsp)
			}
			if rsp.Job.Status == "FAILED" {
				return errors.New(imageJobFailureMessage(rsp.Job))
			}
			return nil
		}
		time.Sleep(interval)
	}
}

// runBuildWatch follows a sandbox commit build to completion.
func runBuildWatch(c *cli.Context, buildID string) error {
	if !shouldUseTUI(c) {
		return runBuildWatchPlain(c, buildID)
	}
	model, err := tea.NewProgram(newBuildJobTUI(c, buildID)).Run()
	if err != nil {
		return runBuildWatchPlain(c, buildID)
	}
	return finishBuildWatch(c, buildID, model, runBuildWatchPlain)
}

func finishBuildWatch(c *cli.Context, buildID string, model tea.Model, fallback func(*cli.Context, string) error) error {
	final, ok := model.(buildJobTUI)
	if !ok {
		log.Printf("template build-watch TUI returned unexpected model type %T\n", model)
		return fallback(c, buildID)
	}
	if final.canceled {
		log.Printf("stopped watching; build still running, resume with: template build-watch --build-id %s\n", buildID)
		return nil
	}
	if final.rsp != nil {
		printTemplateBuildStatus(final.rsp)
	}
	return final.fatal
}

func runBuildWatchPlain(c *cli.Context, buildID string) error {
	interval := watchInterval(c)
	var lastPrinted string
	for {
		rsp, err := fetchTemplateBuildStatus(c, buildID)
		if err != nil {
			return err
		}
		current := fmt.Sprintf("%s/%d/%s", rsp.Status, rsp.Progress, rsp.Message)
		if current != lastPrinted {
			printTemplateBuildStatus(rsp)
			lastPrinted = current
		}
		if rsp.Status == "ready" || rsp.Status == "error" {
			if c.Bool("json") {
				commands.PrintAsJSON(rsp)
			}
			if rsp.Status == "error" {
				msg := rsp.Message
				if strings.TrimSpace(msg) == "" {
					msg = "sandbox commit build failed"
				}
				return errors.New(msg)
			}
			return nil
		}
		time.Sleep(interval)
	}
}
