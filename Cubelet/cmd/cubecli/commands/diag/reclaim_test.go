// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package diag

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func runReclaim(stdin string, args ...string) (string, error) {
	var out bytes.Buffer
	app := &cli.App{
		Commands:  []*cli.Command{reclaimCommand},
		Reader:    strings.NewReader(stdin),
		Writer:    &out,
		ErrWriter: &out,
	}
	err := app.Run(append([]string{"cubecli", "reclaim"}, args...))
	return out.String(), err
}

func startReclaimSleeper(t *testing.T) utils.ProcessIdentity {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id, err := utils.ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)
	return id
}

func TestReclaimRequiresPidAndStartTime(t *testing.T) {
	_, err := runReclaim("", "--start-time", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pid")

	_, err = runReclaim("", "--pid", "100")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start-time")
}

func TestReclaimReportsAnAbsentPidAsGone(t *testing.T) {
	out, err := runReclaim("", "--pid", "1000000000", "--start-time", "42")
	require.NoError(t, err)
	assert.Contains(t, out, "already gone")
}

func TestReclaimRefusesALivePidWithNoStartTime(t *testing.T) {
	live := startReclaimSleeper(t)

	_, err := runReclaim("", "--pid", fmt.Sprint(live.Pid), "--start-time", "0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Refusing to kill")
	assert.Equal(t, utils.LivenessAlive, live.Status(), "a pid we cannot identify must not be signalled")
}

func TestReclaimDoesNotSignalARecycledPid(t *testing.T) {
	live := startReclaimSleeper(t)

	out, err := runReclaim("",
		"--pid", fmt.Sprint(live.Pid),
		"--start-time", fmt.Sprint(live.StartTime+1),
		"--yes")
	require.NoError(t, err)
	assert.Contains(t, out, "already gone")
	assert.Equal(t, utils.LivenessAlive, live.Status())
}

func TestReclaimAbortsWhenTheOperatorDeclines(t *testing.T) {
	live := startReclaimSleeper(t)

	out, err := runReclaim("n\n",
		"--pid", fmt.Sprint(live.Pid),
		"--start-time", fmt.Sprint(live.StartTime))
	require.NoError(t, err)
	assert.Contains(t, out, "aborted")
	assert.Equal(t, utils.LivenessAlive, live.Status())
}

func TestReclaimRefusesWhenConfirmationCannotBeRead(t *testing.T) {
	live := startReclaimSleeper(t)

	_, err := runReclaim("",
		"--pid", fmt.Sprint(live.Pid),
		"--start-time", fmt.Sprint(live.StartTime))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read confirmation")
	assert.Equal(t, utils.LivenessAlive, live.Status())
}

func TestReclaimKillsTheVerifiedProcess(t *testing.T) {
	live := startReclaimSleeper(t)

	out, err := runReclaim("",
		"--yes",
		"--pid", fmt.Sprint(live.Pid),
		"--start-time", fmt.Sprint(live.StartTime),
		"--wait", "2s")
	require.NoError(t, err)
	assert.Contains(t, out, "is gone")
	assert.Equal(t, utils.LivenessGone, live.Status())
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name    string
		stdin   string
		wantOK  bool
		wantErr bool
	}{
		{"y confirms", "y\n", true, false},
		{"yes confirms", "yes\n", true, false},
		{"uppercase is accepted", "Y\n", true, false},
		{"anything else aborts", "n\n", false, false},
		{"an empty line aborts", "\n", false, false},
		{"eof is an error", "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotOK bool
			var gotErr error
			app := &cli.App{
				Reader: strings.NewReader(tc.stdin),
				Writer: &bytes.Buffer{},
				Action: func(c *cli.Context) error {
					gotOK, gotErr = confirm(c, "SIGKILL pid 1?")
					return nil
				},
			}
			require.NoError(t, app.Run([]string{"cubecli"}))
			assert.Equal(t, tc.wantOK, gotOK)
			if tc.wantErr {
				require.Error(t, gotErr)
				return
			}
			require.NoError(t, gotErr)
		})
	}
}
