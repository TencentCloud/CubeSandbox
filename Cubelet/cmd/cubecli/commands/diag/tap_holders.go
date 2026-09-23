// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const tunDevice = "/dev/net/tun"

// tunHolder is one open /dev/net/tun fd and the tap device it is attached to.
type tunHolder struct {
	Pid       int
	Comm      string
	StartTime uint64
	Fd        string
	// Iface is the tap device this fd is currently attached to, straight from
	// the kernel. An fd that has been opened but not yet attached has none.
	Iface string
}

var tapHoldersCommand = &cli.Command{
	Name:  "tap-holders",
	Usage: "list processes holding a /dev/net/tun fd, and which tap each one is attached to",
	Description: `Answers "who is still holding this tap" by asking the kernel rather than
inferring it from cubelet's records, which is the point: a leaked tap is
exactly the case where the records are missing or wrong.

The tun driver writes the currently attached device name into the fd's
fdinfo, so the mapping is authoritative. cubelet's own pool fds show up here
too, which is expected — a tap held by cubelet is not leaked.

The start time column identifies a process incarnation, and is what
'cubecli diag reclaim' requires so it cannot act on a recycled pid.`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "iface",
			Usage: "only show holders of this tap device",
		},
		&cli.StringFlag{
			Name:   "proc-root",
			Value:  "/proc",
			Hidden: true,
		},
	},
	Action: func(c *cli.Context) error {
		holders, err := scanTunHolders(c.String("proc-root"))
		if err != nil {
			return err
		}
		if iface := c.String("iface"); iface != "" {
			filtered := holders[:0]
			for _, h := range holders {
				if h.Iface == iface {
					filtered = append(filtered, h)
				}
			}
			holders = filtered
		}
		if len(holders) == 0 {
			fmt.Fprintln(c.App.Writer, "no process is holding a tun fd")
			return nil
		}

		w := tabwriter.NewWriter(c.App.Writer, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "PID\tCOMM\tSTART_TIME\tFD\tTAP")
		for _, h := range holders {
			iface := h.Iface
			if iface == "" {
				iface = "-"
			}
			fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", h.Pid, h.Comm, h.StartTime, h.Fd, iface)
		}
		return w.Flush()
	},
}

// scanTunHolders walks procRoot looking for open /dev/net/tun fds.
//
// Processes that vanish mid-scan, and fds we may not read, are skipped rather
// than failing the scan: a partial answer is useful here, and the alternative
// is a command that fails whenever the host is busy.
func scanTunHolders(procRoot string) ([]tunHolder, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}

	var holders []tunHolder
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a process directory
		}
		fdDir := filepath.Join(procRoot, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || target != tunDevice {
				continue
			}
			var startTime uint64
			if id, err := utils.ReadProcessIdentity(pid); err == nil {
				startTime = id.StartTime
			}
			holders = append(holders, tunHolder{
				Pid:       pid,
				Comm:      utils.ProcessComm(pid),
				StartTime: startTime,
				Fd:        fd.Name(),
				Iface:     readTunIface(filepath.Join(procRoot, entry.Name(), "fdinfo", fd.Name())),
			})
		}
	}
	sort.Slice(holders, func(i, j int) bool {
		if holders[i].Pid != holders[j].Pid {
			return holders[i].Pid < holders[j].Pid
		}
		return holders[i].Fd < holders[j].Fd
	})
	return holders, nil
}

// readTunIface returns the tap device an fd is attached to, from the "iff:"
// line the tun driver adds to fdinfo.
func readTunIface(fdinfoPath string) string {
	b, err := os.ReadFile(fdinfoPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "iff:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
