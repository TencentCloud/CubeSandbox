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

// LogsCommand prints container stdout (or stderr) for a given sandbox or template ID.
//
// Sandbox log files live at:
//
//	<cubeletStateDir>/<sandboxID>/stdout|stderr  (inside cubelet mount namespace)
//
// Template log files live at:
//
//	<templateLogDir>/<templateID>_0/stdout|stderr  (host filesystem, no ns needed)
//
// The "_0" suffix is the container index within the sandbox (currently always 0).
// For sandbox logs the process re-execs itself with CUBEMNT=1 so the C
// constructor in pkg/cubemnt/nsenter.c enters the mount namespace while still
// single-threaded.  Template logs are on the host filesystem and need no
// namespace entry.
var LogsCommand = &cli.Command{
	Name:  "logs",
	Usage: "show container stdout/stderr log for a sandbox or template",
	ArgsUsage: "<id>\n\n" +
		"Examples:\n" +
		"  cubecli logs <sandbox-id>        # last 100 lines of sandbox stdout\n" +
		"  cubecli logs --tpl <template-id> # last 100 lines of template build (container _0)\n" +
		"  cubecli logs --stderr <id>       # stderr\n" +
		"  cubecli logs --all <id>          # full log\n" +
		"  cubecli logs -t 50 <id>          # last 50 lines\n" +
		"  cubecli logs -H 20 <id>          # first 20 lines",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "tpl",
			Usage: "treat the id as a template ID; reads from /data/log/template/<id>_0/ without entering a namespace",
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
			return readTemplateLog(id, stream, all, tailN, headN)
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

// readTemplateLog reads log files from /data/log/template/<templateID>_0/.
// The "_0" suffix is the container index within the sandbox (always 0 for
// single-container templates).
// Template logs are on the host filesystem; no namespace entry is needed.
func readTemplateLog(templateID, stream string, all bool, tailN, headN int) error {
	dir := filepath.Join(templateLogDir, templateID+"_0")

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
