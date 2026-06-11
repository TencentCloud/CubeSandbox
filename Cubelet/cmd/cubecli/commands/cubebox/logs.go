// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/urfave/cli/v2"
)

const (
	// cubeletStateDir is the containerd state directory inside cubelet's mount namespace.
	cubeletStateDir = "/data/cubelet/state/io.containerd.runtime.v2.task/default"

	// templateLogDir is the directory where template build logs are written by the shim.
	templateLogDir = "/data/log/template"

	// defaultTailLines is the number of lines returned when no flags are specified.
	defaultTailLines = 100

	// envLogsMode is set to "1" when the process has re-exec'd into the
	// cubelet mount namespace and should run the actual log-reading logic.
	envLogsMode = "CUBECLI_LOGS_MODE"
)

// LogsCommand prints container stdout (or stderr) for a given sandbox ID.
//
// LogsCommand prints container stdout (or stderr) for a given sandbox or template ID.
//
// Sandbox log files live at:
//
//	<cubeletStateDir>/<sandboxID>/stdout|stderr  (inside cubelet mount namespace)
//
// Template log files live at:
//
//	<templateLogDir>/<templateID>_<attempt>/stdout|stderr  (host filesystem, no ns needed)
//
// For sandbox logs the process re-execs itself with CUBEMNT=1 so the C
// constructor in pkg/cubemnt/nsenter.c enters the mount namespace while still
// single-threaded.  Template logs are on the host filesystem and need no
// namespace entry.
var LogsCommand = &cli.Command{
	Name:  "logs",
	Usage: "show container stdout/stderr log for a sandbox or template",
	ArgsUsage: "<id>\n\n" +
		"Examples:\n" +
		"  cubecli logs <sandbox-id>             # last 100 lines of sandbox stdout\n" +
		"  cubecli logs --tpl <template-id>      # last 100 lines of latest template build\n" +
		"  cubecli logs --tpl --attempt 2 <id>  # specific build attempt\n" +
		"  cubecli logs --stderr <id>            # stderr\n" +
		"  cubecli logs --all <id>               # full log\n" +
		"  cubecli logs -t 50 <id>              # last 50 lines\n" +
		"  cubecli logs -H 20 <id>              # first 20 lines",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "tpl",
			Usage: "treat the id as a template ID; reads from /data/log/template/ without entering a namespace",
		},
		&cli.IntFlag{
			Name:  "attempt",
			Usage: "template build attempt number (default: latest)",
			Value: -1,
		},
		&cli.BoolFlag{
			Name:    "stderr",
			Aliases: []string{"e"},
			Usage:   "read stderr instead of stdout",
		},
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "print all lines (overrides --tail / --head)",
		},
		&cli.IntFlag{
			Name:    "tail",
			Aliases: []string{"t"},
			Usage:   "print the last N lines (default 100 when neither --all nor --head is set)",
			Value:   0,
		},
		&cli.IntFlag{
			Name:    "head",
			Aliases: []string{"H"},
			Usage:   "print the first N lines",
			Value:   0,
		},
	},
	Action: func(cliCtx *cli.Context) error {
		if cliCtx.NArg() < 1 {
			return fmt.Errorf("id is required")
		}
		id := cliCtx.Args().First()
		if err := pathutil.ValidateSafeID(id); err != nil {
			return fmt.Errorf("invalid id: %w", err)
		}

		stream := "stdout"
		if cliCtx.Bool("stderr") {
			stream = "stderr"
		}
		all := cliCtx.Bool("all")
		tailN := cliCtx.Int("tail")
		headN := cliCtx.Int("head")
		if !all && tailN == 0 && headN == 0 {
			tailN = defaultTailLines
		}

		// Template log: read directly from host filesystem, no namespace needed.
		if cliCtx.Bool("tpl") {
			return readTemplateLog(id, stream, all, tailN, headN, cliCtx.Int("attempt"))
		}

		// Already inside the namespace: do the real work.
		if os.Getenv(envLogsMode) == "1" {
			return readLog(id, stream, all, tailN, headN)
		}

		// Re-exec with CUBEMNT=1 so the C constructor enters the cubelet mount
		// namespace before Go runtime starts (single-threaded at that point).
		// Pass os.Args[1:] directly so flag parsing is handled by the CLI
		// framework in the child, avoiding fragile manual flag reconstruction.
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot determine executable path: %w", err)
		}

		cmd := exec.Command(self, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "CUBEMNT=1", envLogsMode+"=1")

		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			return err
		}
		return nil
	},
}

// readLog opens the log file for sandboxID/stream and prints lines according
// to the requested mode. Must be called after entering the cubelet mount namespace.
func readLog(sandboxID, stream string, all bool, tailN, headN int) error {
	logPath := filepath.Join(cubeletStateDir, sandboxID, stream)

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s\n(sandbox may not exist or log forwarding may not be enabled)", logPath)
		}
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer f.Close()

	switch {
	case all:
		_, err = io.Copy(os.Stdout, f)
		return err
	case headN > 0:
		return printHead(f, headN)
	default:
		return printTail(f, tailN)
	}
}

// printHead prints the first n lines from r.
func printHead(r io.Reader, n int) error {
	if n <= 0 {
		return nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for i := 0; i < n && scanner.Scan(); i++ {
		fmt.Println(scanner.Text())
	}
	return scanner.Err()
}

// printTail prints the last n lines from r using a circular buffer.
func printTail(r io.Reader, n int) error {
	if n <= 0 {
		return nil
	}
	buf := make([]string, n)
	count := 0

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for scanner.Scan() {
		buf[count%n] = scanner.Text()
		count++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}

	start := 0
	size := count
	if count > n {
		start = count % n
		size = n
	}
	for i := 0; i < size; i++ {
		fmt.Println(buf[(start+i)%n])
	}
	return nil
}

// readTemplateLog reads log files from /data/log/template/<templateID>_<attempt>/.
// If attempt is -1 (default), the latest attempt (highest number) is used.
// Template logs are on the host filesystem; no namespace entry is needed.
func readTemplateLog(templateID, stream string, all bool, tailN, headN, attempt int) error {
	var dir string
	if attempt >= 0 {
		dir = filepath.Join(templateLogDir, fmt.Sprintf("%s_%d", templateID, attempt))
	} else {
		// Find the latest attempt by listing all <templateID>_N subdirectories.
		entries, err := os.ReadDir(templateLogDir)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("template log directory not found: %s", templateLogDir)
			}
			return fmt.Errorf("read template log dir: %w", err)
		}
		prefix := templateID + "_"
		var matches []string
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
				matches = append(matches, e.Name())
			}
		}
		if len(matches) == 0 {
			return fmt.Errorf("no log directories found for template %s under %s", templateID, templateLogDir)
		}
		// Sort lexicographically; since attempt numbers are appended as integers
		// after the last underscore, lexicographic order matches numeric order
		// for reasonable attempt counts (< 10).
		sort.Strings(matches)
		dir = filepath.Join(templateLogDir, matches[len(matches)-1])
	}

	logPath := filepath.Join(dir, stream)
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s", logPath)
		}
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer f.Close()

	switch {
	case all:
		_, err = io.Copy(os.Stdout, f)
		return err
	case headN > 0:
		return printHead(f, headN)
	default:
		return printTail(f, tailN)
	}
}
