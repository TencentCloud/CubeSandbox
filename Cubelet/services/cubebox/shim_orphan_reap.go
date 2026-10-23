// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// orphanShimDeleteTimeout bounds one fallback "<shim> delete" invocation.
const orphanShimDeleteTimeout = 10 * time.Second

// orphanShimProcRoot is the process table scanned for orphaned shims. It is a
// variable so tests can point it at a fake tree.
var orphanShimProcRoot = "/proc"

// runShimDelete executes a prepared "<shim> delete" command. It is a variable
// so tests can observe the invocation without running a real shim.
var runShimDelete = func(cmd *exec.Cmd) error { return cmd.Run() }

// killOrphanShim is the last resort when the shim delete did not take the
// shim down. It is a variable so tests can observe it.
var killOrphanShim = func(id utils.ProcessIdentity) error {
	if id.Status() != utils.LivenessAlive {
		return nil
	}
	return syscall.Kill(id.Pid, syscall.SIGKILL)
}

// orphanShim is a shim server that outlived a failed task create.
type orphanShim struct {
	ident     utils.ProcessIdentity
	exe       string
	namespace string
	address   string // containerd address the shim was started with
	socket    string // shim ttrpc socket, "unix://..."
}

// reapOrphanShims runs the shim's own "delete" for shim servers of id that
// survived a failed NewTask.
//
// When task create fails, containerd's rollback removes the bundle before the
// dead-shim cleanup forks "<shim> -bundle <bundle> delete" with the bundle as
// its working directory. The child then fails in chdir(2), surfacing as
// "fork/exec <shim>: no such file or directory": the shim keeps running and
// its sandbox resources are never released.
//
// The shim delete only needs "shim.pid" and "address" in its working
// directory, so a throwaway bundle carrying those two files is enough to run
// the regular cleanup implemented in the shim. The shim is found by its
// command line because the original bundle is gone.
func reapOrphanShims(ctx context.Context, id string) {
	if id == "" {
		return
	}
	for _, s := range findOrphanShims(orphanShimProcRoot, id) {
		if err := deleteOrphanShim(ctx, id, s); err != nil {
			log.G(ctx).Warnf("shim delete for orphan shim pid %d of %s: %v", s.ident.Pid, id, err)
		}
		if err := waitOrphanShimGone(s.ident); err != nil {
			log.G(ctx).Warnf("orphan shim pid %d of %s not gone after delete, killing: %v", s.ident.Pid, id, err)
			if err := killOrphanShim(s.ident); err != nil {
				log.G(ctx).Warnf("kill orphan shim pid %d of %s: %v", s.ident.Pid, id, err)
				continue
			}
			if err := waitOrphanShimGone(s.ident); err != nil {
				log.G(ctx).Warnf("orphan shim pid %d of %s not gone: %v", s.ident.Pid, id, err)
				continue
			}
		}
		log.G(ctx).Warnf("reaped orphan shim pid %d of %s after failed task create", s.ident.Pid, id)
	}
}

func waitOrphanShimGone(ident utils.ProcessIdentity) error {
	wctx, cancel := context.WithTimeout(context.Background(), runtimeReapWait)
	defer cancel()
	return utils.WaitIdentityGone(wctx, ident)
}

// deleteOrphanShim invokes "<shim> delete" from a temporary bundle that holds
// the shim pid and socket address, as containerd would from the real bundle.
func deleteOrphanShim(ctx context.Context, id string, s orphanShim) error {
	// The delete signals whatever pid the bundle names; never hand it a pid
	// that no longer belongs to this shim.
	if s.ident.Status() != utils.LivenessAlive {
		return nil
	}
	dir, err := os.MkdirTemp("", "cube-shim-delete-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "shim.pid"), []byte(strconv.Itoa(s.ident.Pid)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "address"), []byte(s.socket), 0o600); err != nil {
		return err
	}

	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orphanShimDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(dctx, s.exe,
		"-namespace", s.namespace,
		"-address", s.address,
		"-id", id,
		"-bundle", dir,
		"delete")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TTRPC_ADDRESS="+s.address+".ttrpc")
	var stderr bytes.Buffer
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &stderr
	if err := runShimDelete(cmd); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// findOrphanShims returns long-running shim servers started for id. Each pid
// is pinned to its incarnation before its cmdline is read, so later actions
// cannot hit a recycled pid. Short-lived "start"/"delete" invocations are
// skipped: they exit on their own and are driven by containerd.
func findOrphanShims(procRoot, id string) []orphanShim {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var found []orphanShim
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		ident, err := utils.ReadProcessIdentityAt(procRoot, pid)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		s, ok := parseShimServer(strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), id)
		if !ok {
			continue
		}
		// Prefer the running image: the path in argv may have been replaced
		// by an upgrade since the shim started.
		if exe, err := os.Readlink(filepath.Join(procRoot, e.Name(), "exe")); err == nil &&
			filepath.IsAbs(exe) && !strings.HasSuffix(exe, " (deleted)") {
			s.exe = exe
		}
		s.ident = ident
		found = append(found, s)
	}
	return found
}

// parseShimServer reports whether argv is a shim server for id and extracts
// what is needed to run its delete.
func parseShimServer(argv []string, id string) (orphanShim, bool) {
	var s orphanShim
	if len(argv) < 3 || !strings.HasPrefix(filepath.Base(argv[0]), "containerd-shim") {
		return s, false
	}
	switch argv[len(argv)-1] {
	case "start", "delete":
		return s, false
	}
	s.exe = argv[0]
	matched := false
	for i := 1; i+1 < len(argv); i++ {
		switch argv[i] {
		case "-id":
			matched = argv[i+1] == id
		case "-namespace":
			s.namespace = argv[i+1]
		case "-address":
			s.address = argv[i+1]
		case "-socket":
			s.socket = argv[i+1]
		}
	}
	if !matched || s.namespace == "" || s.address == "" || !strings.HasPrefix(s.socket, "unix://") {
		return orphanShim{}, false
	}
	return s, true
}
