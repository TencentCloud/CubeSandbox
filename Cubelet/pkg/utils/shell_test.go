// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestExec(t *testing.T) {
	out, err, execErr := Exec("echo 'hello world'", time.Second)
	assert.NoError(t, execErr)
	assert.Equal(t, "hello world\n", out)
	assert.Empty(t, err)
}

func TestExecV(t *testing.T) {
	out, err, execErr := ExecV([]string{"bash", "-c", "echo 'hello world'"}, time.Second)
	assert.NoError(t, execErr)
	assert.Equal(t, "hello world\n", out)
	assert.Empty(t, err)

	_, _, execErr = ExecV(nil, time.Second)
	assert.Error(t, execErr)
	assert.Contains(t, execErr.Error(), "cmd not found")

	_, _, execErr = ExecV([]string{"bash", "-c", "sleep 2"}, 100*time.Millisecond)
	assert.Error(t, execErr)

	assert.Contains(t, execErr.Error(), "signal: killed")
}

func TestExecVCtx(t *testing.T) {

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out, err, execErr := ExecVCtx(ctx, []string{"bash", "-c", "echo 'hello world'"})
	assert.NoError(t, execErr)
	assert.Equal(t, "hello world\n", out)
	assert.Empty(t, err)

	_, _, execErr = ExecVCtx(ctx, nil)
	assert.Error(t, execErr)
	assert.Contains(t, execErr.Error(), "cmd not found")

	_, _, execErr = ExecVCtx(ctx, []string{"ls"})
	assert.NoError(t, execErr)

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer timeoutCancel()

	_, _, execErr = ExecVCtx(timeoutCtx, []string{"bash", "-c", "sleep 2"})
	assert.Error(t, execErr)

	assert.Contains(t, execErr.Error(), "signal: killed")

	cancelledCtx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc()

	_, _, execErr = ExecVCtx(cancelledCtx, []string{"bash", "-c", "echo 'test'"})
	assert.Error(t, execErr)
	assert.Contains(t, execErr.Error(), "context canceled")
}

func TestExecBin(t *testing.T) {

	out, err, execErr := ExecBin("echo", []string{"hello", "world"}, time.Second)
	assert.NoError(t, execErr)
	assert.Equal(t, "hello world\n", out)
	assert.Empty(t, err)

	out, err, execErr = ExecBin("pwd", nil, time.Second)
	assert.NoError(t, execErr)
	assert.NotEmpty(t, out)
	assert.Empty(t, err)

	out, err, execErr = ExecBin("pwd", []string{}, time.Second)
	assert.NoError(t, execErr)
	assert.NotEmpty(t, out)
	assert.Empty(t, err)

	_, _, execErr = ExecBin("nonexistentcommand", []string{}, time.Second)
	assert.Error(t, execErr)

	_, _, execErr = ExecBin("sleep", []string{"2"}, 100*time.Millisecond)
	assert.Error(t, execErr)

	assert.Contains(t, execErr.Error(), "signal: killed")

	_, stderrOut, execErr := ExecBin("ls", []string{"/nonexistent/path"}, time.Second)
	assert.Error(t, execErr)
	assert.NotEmpty(t, stderrOut)
}

func TestExecBinCtx(t *testing.T) {

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out, err, execErr := ExecBinCtx(ctx, "echo", []string{"hello", "world"})
	assert.NoError(t, execErr)
	assert.Equal(t, "hello world\n", out)
	assert.Empty(t, err)

	out, err, execErr = ExecBinCtx(ctx, "pwd", nil)
	assert.NoError(t, execErr)
	assert.NotEmpty(t, out)
	assert.Empty(t, err)

	out, err, execErr = ExecBinCtx(ctx, "pwd", []string{})
	assert.NoError(t, execErr)
	assert.NotEmpty(t, out)
	assert.Empty(t, err)

	_, _, execErr = ExecBinCtx(ctx, "nonexistentcommand", []string{})
	assert.Error(t, execErr)

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer timeoutCancel()

	_, _, execErr = ExecBinCtx(timeoutCtx, "sleep", []string{"2"})
	assert.Error(t, execErr)

	assert.Contains(t, execErr.Error(), "signal: killed")

	cancelledCtx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc()

	_, _, execErr = ExecBinCtx(cancelledCtx, "echo", []string{"test"})
	assert.Error(t, execErr)
	assert.Contains(t, execErr.Error(), "context canceled")

	_, stderr, execErr := ExecBinCtx(ctx, "ls", []string{"/nonexistent/path"})
	assert.Error(t, execErr)
	assert.NotEmpty(t, stderr)
}

func TestValidateExecArgs(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		args    []string
		wantErr bool
		errSub  string
	}{
		{name: "ok-simple", cmd: "ls", args: []string{"-l", "/tmp"}, wantErr: false},
		{name: "ok-pathlike", cmd: "/usr/bin/ls", args: nil, wantErr: false},
		{name: "empty-cmd", cmd: "", args: nil, wantErr: true, errSub: "empty"},
		{name: "cmd-leading-dash", cmd: "-rf", args: nil, wantErr: true, errSub: "'-'"},
		{name: "cmd-with-semicolon", cmd: "ls;rm", args: nil, wantErr: true, errSub: "forbidden"},
		{name: "cmd-with-pipe", cmd: "ls|cat", args: nil, wantErr: true, errSub: "forbidden"},
		{name: "cmd-with-backtick", cmd: "l`s", args: nil, wantErr: true, errSub: "forbidden"},
		{name: "cmd-with-dollar", cmd: "ls$IFS", args: nil, wantErr: true, errSub: "forbidden"},
		{name: "arg-with-nul", cmd: "ls", args: []string{"a\x00b"}, wantErr: true, errSub: "NUL"},
		{name: "arg-with-bell", cmd: "ls", args: []string{"a\x07b"}, wantErr: true, errSub: "control"},
		{name: "arg-with-newline-ok", cmd: "ls", args: []string{"a\nb"}, wantErr: false},
		{name: "arg-with-tab-ok", cmd: "ls", args: []string{"a\tb"}, wantErr: false},
		{name: "arg-with-pipe-ok", cmd: "ls", args: []string{"a|b"}, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExecArgs(tc.cmd, tc.args)
			if tc.wantErr {
				assert.Error(t, err)
				if tc.errSub != "" {
					assert.Contains(t, err.Error(), tc.errSub)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestExecBinRejectsInjection(t *testing.T) {
	stdout, stderr, err := ExecBin("ls;rm -rf /", nil, time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr, "stderr must be empty when no command was executed")

	stdout, stderr, err = ExecBin("ls", []string{"a\x00b"}, time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NUL")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)

	stdout, stderr, err = ExecBin("-rf", nil, time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'-'")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
}

func TestExecRejectsNul(t *testing.T) {
	stdout, stderr, err := Exec("echo a\x00b", time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NUL")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr, "stderr must be empty when no command was executed")
}

func TestExecVCtxRejectsInjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	stdout, stderr, err := ExecVCtx(ctx, []string{"ls;rm", "-rf"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr, "stderr must be empty when no command was executed")

	stdout, stderr, err = ExecVCtx(ctx, []string{"ls", "arg\x00bad"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NUL")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
}
