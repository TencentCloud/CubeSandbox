// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const DefaultTimeout = time.Second * 3

// shellMetaChars lists characters interpreted by a shell. A command name
// (argv[0]) sourced from external input must never contain any of them:
// even though exec.Command itself does not spawn a shell, the same string
// may later be concatenated by a caller or passed to a shell-based entry
// point such as Exec.
const shellMetaChars = ";|&`$<>()\\*?\"'\r\n"

// validateExecArg ensures a single command argument is safe to pass to
// os/exec:
//   - Reject NUL bytes (os/exec would panic; surface a friendlier error).
//   - Reject ASCII control characters other than \t/\r/\n to prevent
//     terminal-escape injection and similar nuisance payloads.
func validateExecArg(arg string) error {
	for i, r := range arg {
		if r == 0 {
			return fmt.Errorf("argument contains NUL byte at index %d", i)
		}
		if r < 0x20 && r != '\t' && r != '\r' && r != '\n' {
			return fmt.Errorf("argument contains control character %#U at index %d", r, i)
		}
		if r == unicode.ReplacementChar {
			return fmt.Errorf("argument contains invalid utf-8 at index %d", i)
		}
	}
	return nil
}

// validateExecName validates the command name (argv[0]). The command name
// is expected to be a fixed executable path or binary name; any external
// input reaching this position implies a potential command-injection
// vector, so we reject shell metacharacters and a leading '-' (which could
// otherwise be misinterpreted as an option flag).
func validateExecName(name string) error {
	if name == "" {
		return fmt.Errorf("command name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("command name %q must not start with '-'", name)
	}
	if i := strings.IndexAny(name, shellMetaChars); i >= 0 {
		return fmt.Errorf("command name %q contains forbidden character %q", name, string(name[i]))
	}
	return validateExecArg(name)
}

// validateExecArgs is the last line of defense for the Exec*/ExecV*/ExecBin*
// family of sinks. Even when upstream callers already sanitized the inputs,
// re-validating here keeps the sanitizer on the data-flow path for static
// analysis tools and prevents any future caller from accidentally passing
// arguments that contain NUL or control characters.
func validateExecArgs(name string, args []string) error {
	if err := validateExecName(name); err != nil {
		return err
	}
	for i, a := range args {
		if err := validateExecArg(a); err != nil {
			return fmt.Errorf("argv[%d]: %w", i+1, err)
		}
	}
	return nil
}

// Exec runs the given string through /usr/bin/bash -lc, so the argument
// goes through full shell parsing. It is intended only for trusted internal
// callers (never concatenate external input into arg). The validation
// below still rejects NUL and control characters to catch obvious
// injection attempts and accidental misuse.
func Exec(arg string, timeout time.Duration) (string, string, error) {
	if err := validateExecArg(arg); err != nil {
		return "", "", err
	}
	var stderrBuffer bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/bash", "-lc", arg)
	cmd.Stderr = &stderrBuffer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.Output()
	return string(output), stderrBuffer.String(), err
}

func ExecV(argv []string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return ExecVCtx(ctx, argv)
}

func PipeExecV(argv []string, timeout time.Duration) (string, string, error) {
	return "", "", nil
}

func ExecVCtx(ctx context.Context, argv []string) (string, string, error) {
	if len(argv) == 0 {
		return "", "cmd not found", fmt.Errorf("cmd not found")
	}
	if err := validateExecArgs(argv[0], argv[1:]); err != nil {
		return "", "", err
	}
	var stderrBuffer bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = &stderrBuffer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.Output()
	return string(output), stderrBuffer.String(), err
}

func ExecBin(name string, args []string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return ExecBinCtx(ctx, name, args)
}

func ExecBinCtx(ctx context.Context, name string, args []string) (string, string, error) {
	if err := validateExecArgs(name, args); err != nil {
		return "", "", err
	}
	var stderrBuffer bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderrBuffer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.Output()
	return string(output), stderrBuffer.String(), err
}
