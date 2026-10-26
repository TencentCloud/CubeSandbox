// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"errors"
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

// orphanShimDeleteTimeout bounds one fallback "<shim> delete" invocation. The
// reap runs under the sandbox lock, so keep this short: the delete only signals
// a pid and removes a few directories.
const orphanShimDeleteTimeout = 5 * time.Second

// shimBinaryPrefix is the basename prefix every containerd shim binary has.
const shimBinaryPrefix = "containerd-shim"

// orphanShimProcRoot is the process table scanned for orphaned shims. It is a
// variable so tests can point it at a fake tree.
var orphanShimProcRoot = "/proc"

// orphanShimGoneWait bounds each wait for a shim to exit, after the delete and
// after the SIGKILL. It is a variable so tests do not have to sleep through it.
var orphanShimGoneWait = runtimeReapWait

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

// errNotShimOfID marks a process that is not a shim server of the requested
// id at all, as opposed to one that matched the id but was rejected.
var errNotShimOfID = errors.New("not a shim server of this id")

// orphanShim is a shim server that outlived a failed task create.
type orphanShim struct {
	ident     utils.ProcessIdentity
	exe       string // verified binary to run the delete with; "" if none
	argv0     string
	namespace string
	address   string // containerd address the shim was started with
	socket    string // shim ttrpc socket, always "unix://..."
}

// reapOrphanShims runs the shim's own "delete" for shim servers of id that
// survived a failed NewTask, and returns how many of them could not be
// confirmed gone afterwards.
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
//
// A non-zero result means a shim may still hold the sandbox's resources; the
// caller must not record the sandbox as having no shim.
func reapOrphanShims(ctx context.Context, id string) int {
	if id == "" {
		return 0
	}
	survivors := 0
	for _, s := range findOrphanShims(ctx, orphanShimProcRoot, id) {
		if !reapOrphanShim(ctx, id, s) {
			survivors++
		}
	}
	return survivors
}

// reapOrphanShim takes one orphan shim down and reports whether it is gone.
func reapOrphanShim(ctx context.Context, id string, s orphanShim) bool {
	pid := s.ident.Pid
	if s.exe == "" {
		log.G(ctx).Warnf("orphan shim pid %d of %s: running image cannot be executed, "+
			"skipping shim delete; its sandbox resources will not be cleaned", pid, id)
	} else if err := deleteOrphanShim(ctx, id, s); err != nil {
		log.G(ctx).Warnf("shim delete for orphan shim pid %d of %s: %v", pid, id, err)
	}
	if err := waitOrphanShimGone(s.ident); err != nil {
		log.G(ctx).Warnf("orphan shim pid %d of %s not gone after delete, killing: %v", pid, id, err)
		if err := killOrphanShim(s.ident); err != nil {
			log.G(ctx).Errorf("kill orphan shim pid %d of %s: %v", pid, id, err)
			return false
		}
		if err := waitOrphanShimGone(s.ident); err != nil {
			log.G(ctx).Errorf("orphan shim pid %d of %s not gone: %v", pid, id, err)
			return false
		}
	}
	log.G(ctx).Infof("reaped orphan shim pid %d of %s after failed task create", pid, id)
	return true
}

func waitOrphanShimGone(ident utils.ProcessIdentity) error {
	wctx, cancel := context.WithTimeout(context.Background(), orphanShimGoneWait)
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
	cmd.Env = shimDeleteEnv(s)
	var stderr bytes.Buffer
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &stderr
	if err := runShimDelete(cmd); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// shimDeleteEnv is the environment of the delete invocation: the variables
// containerd's shim.Command adds, PATH, and the two the shim reads for its
// logging. Nothing else from Cubelet is passed on; the delete path reads no
// other variable. TTRPC_ADDRESS is not used by the delete itself, but the shim
// bootstrap refuses to start without it.
func shimDeleteEnv(s orphanShim) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"TTRPC_ADDRESS=" + s.address + ".ttrpc",
		"GRPC_ADDRESS=" + s.address,
		"NAMESPACE=" + s.namespace,
	}
	for _, k := range []string{"RUST_LOG", "FUNCTION_TYPE"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// findOrphanShims returns long-running shim servers started for id. Each pid
// is pinned to its incarnation before its cmdline is read, so later actions
// cannot hit a recycled pid. Short-lived "start"/"delete" invocations are
// skipped: they exit on their own and are driven by containerd.
//
// A process is only accepted when it runs as Cubelet's own uid (containerd
// spawns shims as itself) and its running image, not just its argv[0], is a
// containerd shim. The image is what the delete executes, and argv is under
// the control of whoever started the process.
func findOrphanShims(ctx context.Context, procRoot, id string) []orphanShim {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		log.G(ctx).Warnf("scan %s for orphan shims of %s: %v", procRoot, id, err)
		return nil
	}
	self := os.Getpid()
	euid := uint32(os.Geteuid())
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
		s, err := parseShimServer(strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), id)
		if err == nil {
			err = verifyShimProcess(filepath.Join(procRoot, e.Name()), euid, &s)
		}
		if err != nil {
			if !errors.Is(err, errNotShimOfID) {
				log.G(ctx).Debugf("orphan shim candidate pid %d of %s rejected: %v", pid, id, err)
			}
			continue
		}
		s.ident = ident
		found = append(found, s)
	}
	return found
}

// verifyShimProcess checks the owner and the running image of a candidate.
// An image that is a shim but can no longer be executed (replaced by an
// upgrade) leaves s.exe empty: the process is still ours to kill, but there is
// nothing safe to run its delete with.
func verifyShimProcess(dir string, euid uint32, s *orphanShim) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read owner of %s", dir)
	}
	if st.Uid != euid {
		return fmt.Errorf("owned by uid %d, not %d", st.Uid, euid)
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return fmt.Errorf("read running image: %w", err)
	}
	exe, deleted := strings.CutSuffix(exe, " (deleted)")
	if !filepath.IsAbs(exe) || !strings.HasPrefix(filepath.Base(exe), shimBinaryPrefix) {
		return fmt.Errorf("running image %q is not a containerd shim", exe)
	}
	s.exe = ""
	if !deleted {
		s.exe = exe
		return nil
	}
	// The image was replaced since the shim started. The binary now at the
	// argv[0] path is the upgraded shim, which speaks the same delete
	// contract; accept it only when it is a root-owned shim that no one else
	// can have written to.
	if trustedShimBinary(s.argv0) {
		s.exe = s.argv0
	}
	return nil
}

// trustedShimBinary reports whether path is an absolute, root-owned regular
// file named like a shim that neither group nor others can write.
func trustedShimBinary(path string) bool {
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), shimBinaryPrefix) {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}

// parseShimServer reports whether argv is a shim server for id and extracts
// what is needed to run its delete. It returns errNotShimOfID for anything
// that is not a shim server of id, and a descriptive error for a shim server
// of id whose arguments cannot drive a delete.
func parseShimServer(argv []string, id string) (orphanShim, error) {
	var s orphanShim
	if len(argv) < 3 || !strings.HasPrefix(filepath.Base(argv[0]), shimBinaryPrefix) {
		return s, errNotShimOfID
	}
	switch argv[len(argv)-1] {
	case "start", "delete":
		return s, errNotShimOfID
	}
	s.argv0 = argv[0]
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
	if !matched {
		return orphanShim{}, errNotShimOfID
	}
	// The shim's "address" file holds a unix:// URL; containerd's shim CLI
	// may pass the socket either that way or as a bare path.
	if filepath.IsAbs(s.socket) {
		s.socket = "unix://" + s.socket
	}
	switch {
	case s.namespace == "":
		return orphanShim{}, errors.New("missing -namespace")
	case s.address == "":
		return orphanShim{}, errors.New("missing -address")
	case !strings.HasPrefix(s.socket, "unix://") || len(s.socket) == len("unix://"):
		return orphanShim{}, fmt.Errorf("unsupported -socket %q", s.socket)
	}
	return s, nil
}
