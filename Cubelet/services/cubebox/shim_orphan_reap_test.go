// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const (
	testShimBin    = "/opt/bin/containerd-shim-cube-rs"
	testShimSocket = "unix:///run/containerd/s/abc"
)

// writeFakeProc creates /proc/<pid> with a stat, a cmdline and an "exe" link
// to exe. The directory is owned by the test's own uid, which is what the
// reaper requires.
func writeFakeProc(t *testing.T, root string, pid int, exe string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"),
		[]byte(strconv.Itoa(pid)+" (shim) S "+strings.Repeat("0 ", 18)+"4242\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"),
		[]byte(strings.Join(argv, "\x00")+"\x00"), 0o644))
	if exe != "" {
		require.NoError(t, os.Symlink(exe, filepath.Join(dir, "exe")))
	}
}

func shimServerArgv(id string) []string {
	return []string{testShimBin, "-namespace", "default", "-id", id,
		"-address", "/data/cubelet/cubelet.sock", "-socket", testShimSocket}
}

// withFakeShimEnv points the reaper at a fake /proc and records its actions.
// The fake shim "exits" when its /proc entry is removed.
func withFakeShimEnv(t *testing.T, root string, deleteKills bool) (deletes *[]*exec.Cmd, kills *[]int) {
	t.Helper()
	prevRoot, prevRun, prevKill, prevWait := orphanShimProcRoot, runShimDelete, killOrphanShim, orphanShimGoneWait
	t.Cleanup(func() {
		orphanShimProcRoot, runShimDelete, killOrphanShim, orphanShimGoneWait = prevRoot, prevRun, prevKill, prevWait
	})
	orphanShimProcRoot = root
	orphanShimGoneWait = 50 * time.Millisecond
	deletes, kills = &[]*exec.Cmd{}, &[]int{}
	runShimDelete = func(cmd *exec.Cmd) error {
		*deletes = append(*deletes, cmd)
		pid, err := os.ReadFile(filepath.Join(cmd.Dir, "shim.pid"))
		require.NoError(t, err)
		addr, err := os.ReadFile(filepath.Join(cmd.Dir, "address"))
		require.NoError(t, err)
		assert.Equal(t, testShimSocket, string(addr))
		if deleteKills {
			return os.RemoveAll(filepath.Join(root, string(pid)))
		}
		return errors.New("delete failed")
	}
	killOrphanShim = func(id utils.ProcessIdentity) error {
		*kills = append(*kills, id.Pid)
		assert.Equal(t, uint64(4242), id.StartTime, "kill must target the pinned incarnation")
		return os.RemoveAll(filepath.Join(root, strconv.Itoa(id.Pid)))
	}
	return deletes, kills
}

func TestParseShimServer(t *testing.T) {
	const id = "sb1"
	cases := []struct {
		name    string
		argv    []string
		ok      bool
		foreign bool // rejected as not a shim of id at all
	}{
		{"server", shimServerArgv(id), true, false},
		{"bare socket path", []string{testShimBin, "-namespace", "default", "-id", id,
			"-address", "/a.sock", "-socket", "/run/containerd/s/abc"}, true, false},
		{"other id", shimServerArgv("sb2"), false, true},
		{"id prefix", shimServerArgv(id + "x"), false, true},
		{"no socket", []string{testShimBin, "-namespace", "default", "-id", id, "-address", "/a.sock"}, false, false},
		{"relative socket", []string{testShimBin, "-namespace", "default", "-id", id,
			"-address", "/a.sock", "-socket", "s/abc"}, false, false},
		{"no namespace", []string{testShimBin, "-id", id, "-address", "/a.sock", "-socket", testShimSocket}, false, false},
		{"delete cmd", append(shimServerArgv(id), "delete"), false, true},
		{"start cmd", append(shimServerArgv(id), "start"), false, true},
		{"not a shim", []string{"/usr/bin/sleep", "-id", id, "100"}, false, true},
		{"empty", nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseShimServer(tc.argv, id)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, testShimSocket, s.socket)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.foreign, errors.Is(err, errNotShimOfID))
		})
	}

	s, err := parseShimServer(shimServerArgv(id), id)
	require.NoError(t, err)
	assert.Equal(t, testShimBin, s.argv0)
	assert.Empty(t, s.exe, "argv[0] is never trusted as the binary to run")
	assert.Equal(t, "default", s.namespace)
	assert.Equal(t, "/data/cubelet/cubelet.sock", s.address)
}

func TestReapOrphanShimsRunsShimDelete(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, testShimBin, shimServerArgv("sb1")...)
	writeFakeProc(t, root, 100002, testShimBin, shimServerArgv("sb2")...)
	writeFakeProc(t, root, 100003, testShimBin, append(shimServerArgv("sb1"), "delete")...)
	deletes, kills := withFakeShimEnv(t, root, true)
	t.Setenv("CUBELET_SECRET_FOR_TEST", "must-not-leak")

	assert.Zero(t, reapOrphanShims(context.Background(), "sb1"))

	require.Len(t, *deletes, 1)
	cmd := (*deletes)[0]
	assert.Equal(t, testShimBin, cmd.Path)
	assert.Equal(t, []string{testShimBin, "-namespace", "default", "-address", "/data/cubelet/cubelet.sock",
		"-id", "sb1", "-bundle", cmd.Dir, "delete"}, cmd.Args)
	assert.Contains(t, cmd.Env, "TTRPC_ADDRESS=/data/cubelet/cubelet.sock.ttrpc")
	for _, kv := range cmd.Env {
		assert.NotContains(t, kv, "CUBELET_SECRET_FOR_TEST", "Cubelet's environment must not reach the shim")
	}
	assert.Empty(t, *kills, "a successful shim delete needs no kill")
	_, err := os.Stat(cmd.Dir)
	assert.True(t, os.IsNotExist(err), "temporary bundle must be removed")
	_, err = os.Stat(filepath.Join(root, "100002"))
	assert.NoError(t, err, "a shim of another sandbox must be left alone")
}

// A process may claim to be a shim in its argv; what decides is the image it
// runs, because that is what the delete would execute as root.
func TestReapOrphanShimsIgnoresSpoofedArgv(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, "/tmp/payload", shimServerArgv("sb1")...)
	writeFakeProc(t, root, 100002, "", shimServerArgv("sb1")...) // exe unreadable
	deletes, kills := withFakeShimEnv(t, root, true)

	assert.Zero(t, reapOrphanShims(context.Background(), "sb1"))

	assert.Empty(t, *deletes, "nothing may be executed for a process that is not running a shim")
	assert.Empty(t, *kills, "a process that is not a shim must not be signalled either")
}

// When the shim binary was replaced after the shim started, the running image
// shows up as "(deleted)". There is nothing safe to run its delete with unless
// argv[0] is a trusted shim binary, but the process is still ours to kill.
func TestReapOrphanShimsKillsWhenImageReplaced(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, testShimBin+" (deleted)", shimServerArgv("sb1")...)
	deletes, kills := withFakeShimEnv(t, root, true)

	assert.Zero(t, reapOrphanShims(context.Background(), "sb1"))

	assert.Empty(t, *deletes, "argv[0] is not a root-owned shim binary here, so it must not be run")
	assert.Equal(t, []int{100001}, *kills)
}

func TestReapOrphanShimsKillsWhenDeleteFails(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, testShimBin, shimServerArgv("sb1")...)
	deletes, kills := withFakeShimEnv(t, root, false)

	assert.Zero(t, reapOrphanShims(context.Background(), "sb1"))

	assert.Len(t, *deletes, 1)
	assert.Equal(t, []int{100001}, *kills)
}

func TestReapOrphanShimsReportsSurvivors(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, testShimBin, shimServerArgv("sb1")...)
	withFakeShimEnv(t, root, false)
	killOrphanShim = func(utils.ProcessIdentity) error { return errors.New("EPERM") }

	assert.Equal(t, 1, reapOrphanShims(context.Background(), "sb1"))
}

func TestReapOrphanShimsNoMatch(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, testShimBin, shimServerArgv("sb2")...)
	deletes, kills := withFakeShimEnv(t, root, true)

	assert.Zero(t, reapOrphanShims(context.Background(), "sb1"))
	assert.Zero(t, reapOrphanShims(context.Background(), ""))

	assert.Empty(t, *deletes)
	assert.Empty(t, *kills)
}

func TestTrustedShimBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "containerd-shim-cube-rs")
	require.NoError(t, os.WriteFile(bin, []byte("x"), 0o755))
	other := filepath.Join(dir, "payload")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o755))
	writable := filepath.Join(dir, "containerd-shim-w")
	require.NoError(t, os.WriteFile(writable, []byte("x"), 0o755))
	require.NoError(t, os.Chmod(writable, 0o777))

	assert.Equal(t, os.Geteuid() == 0, trustedShimBinary(bin), "root-owned shim binary")
	assert.False(t, trustedShimBinary(other), "not named like a shim")
	assert.False(t, trustedShimBinary(writable), "world-writable")
	assert.False(t, trustedShimBinary("containerd-shim-cube-rs"), "relative path")
	assert.False(t, trustedShimBinary(filepath.Join(dir, "containerd-shim-missing")))
}
