// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

// Liveness is deliberately three-valued. A bool cannot express "we could not
// find out", and treating that case as "gone" is what lets a still-running
// shim look dead to the destroy path.
type Liveness int

const (
	// LivenessUnknown means /proc could not be read. Callers must treat this
	// as "may still be holding resources" and refuse to release anything.
	LivenessUnknown Liveness = iota
	// LivenessGone means the pid is absent, a zombie, or belongs to a
	// different incarnation than the one recorded.
	LivenessGone
	// LivenessAlive means the recorded incarnation is still running.
	LivenessAlive
)

func (l Liveness) String() string {
	switch l {
	case LivenessGone:
		return "gone"
	case LivenessAlive:
		return "alive"
	default:
		return "unknown"
	}
}

// ProcessIdentity pins a pid to one incarnation. A bare pid is not an
// identity: Linux recycles pid numbers, so kill(pid, 0) succeeding only means
// *some* process holds that number today. StartTime (/proc/<pid>/stat field
// 22, in clock ticks since boot) never repeats for a reused pid, so a match
// means the very process we recorded rather than a stranger that inherited
// the number.
type ProcessIdentity struct {
	Pid       int
	StartTime uint64
}

// ReadProcessIdentity captures the identity of a currently running pid.
func ReadProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 1 {
		return ProcessIdentity{}, fmt.Errorf("invalid pid %d", pid)
	}
	st, err := readProcStat(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{Pid: pid, StartTime: st.startTime}, nil
}

// Status reports whether the recorded incarnation is still running.
//
// Liveness comes from /proc rather than kill(pid, 0) on purpose: kill fails
// with EPERM for a process we may not signal, which is indistinguishable from
// ESRCH at the call site and would report a running process as gone. Reading
// /proc/<pid>/stat needs no such permission.
//
// A zero StartTime means the caller never captured one (e.g. a record written
// by an older version). We cannot prove identity in that case, so a live pid
// is reported as LivenessUnknown rather than LivenessAlive — the caller has to
// decide, and the safe decision is to refuse.
func (id ProcessIdentity) Status() Liveness {
	if id.Pid <= 1 {
		return LivenessGone
	}
	st, err := readProcStat(id.Pid)
	if err != nil {
		if os.IsNotExist(err) {
			return LivenessGone
		}
		return LivenessUnknown
	}
	if st.state == 'Z' {
		// A zombie holds no fds: it cannot keep a tap or a volume open, even
		// though the parent has not reaped it yet.
		return LivenessGone
	}
	if id.StartTime == 0 {
		return LivenessUnknown
	}
	if st.startTime != id.StartTime {
		// The number was recycled; our process is long gone.
		return LivenessGone
	}
	return LivenessAlive
}

// WaitIdentityGone blocks until the recorded incarnation has exited or ctx is
// done. An unknown state is an error, never a success: the caller asked us to
// confirm the process is gone, and we cannot.
func WaitIdentityGone(ctx context.Context, id ProcessIdentity) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		switch id.Status() {
		case LivenessGone:
			return nil
		case LivenessUnknown:
			return fmt.Errorf("process %d liveness unknown: refusing to assume it exited", id.Pid)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("process %d still running: %w", id.Pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// ProcessComm returns the command name of a pid, or "" if it cannot be read.
// For operator-facing diagnostics only — never base a decision on it, since
// a recycled pid answers just as readily as the process you meant.
func ProcessComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type procStat struct {
	state     byte   // field 3
	startTime uint64 // field 22, clock ticks since boot
}

// readProcStat parses the fields of /proc/<pid>/stat that we care about.
//
// Fields 1 and 2 (pid and comm) cannot be split on whitespace: comm is an
// arbitrary command name wrapped in parentheses and may contain both spaces
// and parentheses. Everything after the last ')' is regular, and the first
// token there is field 3, so field 22 sits at index 19.
func readProcStat(pid int) (procStat, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStat{}, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return procStat{}, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(b[i+2:]))
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return procStat{}, fmt.Errorf("malformed /proc/%d/stat: %d fields after comm", pid, len(fields))
	}
	startTime, err := strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("parse start time of pid %d: %w", pid, err)
	}
	return procStat{state: fields[0][0], startTime: startTime}, nil
}

func ProcessExists(ctx context.Context, pid int) bool {
	if pid <= 0 {
		return false
	}
	ctxTmp, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	for {
		select {
		case <-ctxTmp.Done():
			log.G(ctx).Warnf("process[%d] check timeout,still exist,%v", pid, ctxTmp.Err())
			return true
		default:

			if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
				log.G(ctx).Debugf("process[%d] not exist,%v", pid, err)
				return false
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}
