// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package diag

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

var reclaimCommand = &cli.Command{
	Name:      "reclaim",
	Usage:     "kill a process that is still holding sandbox resources, after verifying its identity",
	ArgsUsage: " ",
	Description: `Last resort for a quarantined sandbox: kill the process that is still
holding its tap so the resources go back to the pool.

--start-time is required and is not a formality. A pid on its own does not
identify a process — Linux reuses pid numbers, and by the time an operator
reads an alert and runs this command, the number may belong to something
else entirely. The start time pins it to one incarnation, so this command
either kills the process you meant or refuses.

Take both values from 'cubecli diag tap-holders'.

This is a SIGKILL. The shim gets no chance to flush or unwind, which is
accepted here because the alternative is a permanently reduced pool.`,
	Flags: []cli.Flag{
		&cli.IntFlag{
			Name:     "pid",
			Usage:    "pid of the holder",
			Required: true,
		},
		&cli.Uint64Flag{
			Name:     "start-time",
			Usage:    "start time of that pid, as shown by 'diag tap-holders'",
			Required: true,
		},
		&cli.BoolFlag{
			Name:  "yes",
			Usage: "skip the confirmation prompt",
		},
		&cli.DurationFlag{
			Name:  "wait",
			Value: 10 * time.Second,
			Usage: "how long to wait for the process to disappear",
		},
	},
	Action: func(c *cli.Context) error {
		id := utils.ProcessIdentity{Pid: c.Int("pid"), StartTime: c.Uint64("start-time")}
		out := c.App.Writer

		switch st := id.Status(); st {
		case utils.LivenessGone:
			// Either it exited on its own or the number now belongs to someone
			// else. Both mean there is nothing here to kill.
			fmt.Fprintf(out, "pid %d (start time %d) is already gone; nothing to do\n", id.Pid, id.StartTime)
			return nil
		case utils.LivenessUnknown:
			return fmt.Errorf("cannot confirm the identity of pid %d: %s. "+
				"Refusing to kill a process we cannot identify", id.Pid, st)
		}

		comm := utils.ProcessComm(id.Pid)
		fmt.Fprintf(out, "pid %d (%s), start time %d\n", id.Pid, comm, id.StartTime)

		if !c.Bool("yes") {
			ok, err := confirm(c, fmt.Sprintf("SIGKILL pid %d (%s)?", id.Pid, comm))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(out, "aborted")
				return nil
			}
		}

		// Re-verify immediately before signalling. The gap between the check
		// above and here is small but real, and this is the one operation
		// where being wrong means killing an unrelated process.
		if st := id.Status(); st != utils.LivenessAlive {
			return fmt.Errorf("pid %d changed state to %s before the signal was sent; nothing was killed", id.Pid, st)
		}
		if err := syscall.Kill(id.Pid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("kill pid %d: %w", id.Pid, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), c.Duration("wait"))
		defer cancel()
		if err := utils.WaitIdentityGone(ctx, id); err != nil {
			return fmt.Errorf("pid %d did not exit after SIGKILL: %w", id.Pid, err)
		}
		fmt.Fprintf(out, "pid %d is gone; its resources should return to the pool on the next cleanup\n", id.Pid)
		return nil
	},
}

func confirm(c *cli.Context, prompt string) (bool, error) {
	fmt.Fprintf(c.App.Writer, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(c.App.Reader).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
