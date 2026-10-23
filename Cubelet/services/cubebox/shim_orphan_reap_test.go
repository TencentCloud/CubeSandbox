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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const (
	testShimBin    = "/opt/bin/containerd-shim-cube-rs"
	testShimSocket = "unix:///run/containerd/s/abc"
)

func writeFakeProc(t *testing.T, root string, pid int, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"),
		[]byte(strconv.Itoa(pid)+" (shim) S "+strings.Repeat("0 ", 18)+"4242\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"),
		[]byte(strings.Join(argv, "\x00")+"\x00"), 0o644))
}

func shimServerArgv(id string) []string {
	return []string{testShimBin, "-namespace", "default", "-id", id,
		"-address", "/data/cubelet/cubelet.sock", "-socket", testShimSocket}
}

// withFakeShimEnv points the reaper at a fake /proc and records its actions.
// The fake shim "exits" when its /proc entry is removed.
func withFakeShimEnv(t *testing.T, root string, deleteKills bool) (deletes *[]*exec.Cmd, kills *[]int) {
	t.Helper()
	prevRoot, prevRun, prevKill := orphanShimProcRoot, runShimDelete, killOrphanShim
	t.Cleanup(func() { orphanShimProcRoot, runShimDelete, killOrphanShim = prevRoot, prevRun, prevKill })
	orphanShimProcRoot = root
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
		name string
		argv []string
		want bool
	}{
		{"server", shimServerArgv(id), true},
		{"other id", shimServerArgv("sb2"), false},
		{"id prefix", shimServerArgv(id + "x"), false},
		{"no socket", []string{testShimBin, "-namespace", "default", "-id", id, "-address", "/a.sock"}, false},
		{"delete cmd", append(shimServerArgv(id), "delete"), false},
		{"start cmd", append(shimServerArgv(id), "start"), false},
		{"not a shim", []string{"/usr/bin/sleep", "-id", id, "100"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := parseShimServer(tc.argv, id)
			assert.Equal(t, tc.want, ok)
		})
	}

	s, ok := parseShimServer(shimServerArgv(id), id)
	require.True(t, ok)
	assert.Equal(t, testShimBin, s.exe)
	assert.Equal(t, "default", s.namespace)
	assert.Equal(t, "/data/cubelet/cubelet.sock", s.address)
	assert.Equal(t, testShimSocket, s.socket)
}

func TestReapOrphanShimsRunsShimDelete(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, shimServerArgv("sb1")...)
	writeFakeProc(t, root, 100002, shimServerArgv("sb2")...)
	writeFakeProc(t, root, 100003, append(shimServerArgv("sb1"), "delete")...)
	deletes, kills := withFakeShimEnv(t, root, true)

	reapOrphanShims(context.Background(), "sb1")

	require.Len(t, *deletes, 1)
	cmd := (*deletes)[0]
	assert.Equal(t, testShimBin, cmd.Path)
	assert.Equal(t, []string{testShimBin, "-namespace", "default", "-address", "/data/cubelet/cubelet.sock",
		"-id", "sb1", "-bundle", cmd.Dir, "delete"}, cmd.Args)
	assert.Contains(t, cmd.Env, "TTRPC_ADDRESS=/data/cubelet/cubelet.sock.ttrpc")
	assert.Empty(t, *kills, "a successful shim delete needs no kill")
	_, err := os.Stat(cmd.Dir)
	assert.True(t, os.IsNotExist(err), "temporary bundle must be removed")
	_, err = os.Stat(filepath.Join(root, "100002"))
	assert.NoError(t, err, "a shim of another sandbox must be left alone")
}

func TestReapOrphanShimsKillsWhenDeleteFails(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, shimServerArgv("sb1")...)
	deletes, kills := withFakeShimEnv(t, root, false)

	reapOrphanShims(context.Background(), "sb1")

	assert.Len(t, *deletes, 1)
	assert.Equal(t, []int{100001}, *kills)
}

func TestReapOrphanShimsNoMatch(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100001, shimServerArgv("sb2")...)
	deletes, kills := withFakeShimEnv(t, root, true)

	reapOrphanShims(context.Background(), "sb1")
	reapOrphanShims(context.Background(), "")

	assert.Empty(t, *deletes)
	assert.Empty(t, *kills)
}
