// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/container"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

var ListCommand = &cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "Warning: `cuebcli ls` is deprecated, please use `cubecli cubebox ls` instead",
	ArgsUsage: "[flags] [<filter>, ...]\n" +
		"io.kubernetes.cri.container-type [container|sandbox]\n" +
		"io.kubernetes.cri.sandbox-id xx",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "print only the container id",
		},
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "Show all containers (default shows just running)",
		},
		&cli.IntFlag{
			Name:    "last",
			Aliases: []string{"n"},
			Usage:   "Show n last created containers (includes all states)",
		},
		&cli.BoolFlag{
			Name:    "latest",
			Aliases: []string{"l"},
			Usage:   "Show the latest created container (includes all states)",
		},
		&cli.BoolFlag{
			Name:  "no-trunc",
			Usage: "Don't truncate output",
		},
		&cli.StringFlag{
			Name:    "sandbox",
			Aliases: []string{"s"},
			Usage:   "filter sandbox",
		},
		&cli.BoolFlag{
			Name:  "log-res",
			Usage: "print and log the container(exception sandbox) resources to logfile,should set quiet to true",
		},
		&cli.BoolFlag{
			Name:    "wide",
			Aliases: []string{"w"},
			Usage:   "display more detailed info, like portmapping",
		},
		&cli.BoolFlag{
			Name:  "raw",
			Usage: "display raw info",
		},
	},
	Action: func(context *cli.Context) error {
		var (
			args = context.Args().Slice()
		)
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := cubebox.NewCubeboxMgrClient(conn)

		req := &cubebox.ListCubeSandboxRequest{}
		if context.IsSet("sandbox") {
			id := context.String("sandbox")
			req.Id = &id
		}
		req.Filter = &cubebox.CubeSandboxFilter{}
		if !context.Bool("all") {
			state := &cubebox.ContainerStateValue{
				State: cubebox.ContainerState_CONTAINER_RUNNING,
			}
			req.Filter.State = state
		}
		if len(args) == 2 {
			req.Filter.LabelSelector = map[string]string{
				args[0]: args[1],
			}
		}

		resp, err := client.List(ctx, req)
		if err != nil {
			return err
		}

		sandboxes := resp.Items

		sort.Slice(sandboxes, func(i, j int) bool {
			return sandboxes[i].CreatedAt > sandboxes[j].CreatedAt
		})

		quiet := context.Bool("quiet")
		trunc := !context.Bool("no-trunc")
		all := context.Bool("all")
		latest := context.Bool("latest")
		lastN := context.Int("last")
		logRes := context.Bool("log-res")
		if lastN == 0 && latest {
			lastN = 1
		}
		if !all && lastN > 0 {
			if lastN < len(sandboxes) {
				sandboxes = sandboxes[:lastN]
			}
		}

		if logRes {
			initCubeLog(context, "Cubecli", context.String("logpath"))
		}

		if quiet {
			reqID := uuid.New().String()
			stubCost := time.Second
			for _, sb := range sandboxes {
				fmt.Printf("%s\n", sb.GetId())
				if logRes {
					for _, c := range sb.Containers {
						if c.GetType() == "sandbox" {
							continue
						}
						id := c.GetId()
						if trunc && len(id) > 12 {
							id = id[:12]
						}
						res := c.GetResources()
						action := fmt.Sprintf("%s%s", res.Cpu, res.Mem)
						reportCls(stubCost.Microseconds(), "Cubecli", action, id, reqID)
					}
				}
			}
			return nil
		}

		userNamespace := context.String("namespace")
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		tabHeader := "NS\tCONTAINER\tCUBEBOX\tTYPE\tSTATUS\tIMAGE\tCREATED"
		if context.Bool("wide") {
			tabHeader += "\tPauseAt\tPORTMAPPING\tINSTANCE ID"
		}
		if context.Bool("raw") {
			tabHeader += "\tRAW"
		}
		fmt.Fprintln(w, tabHeader)
		for _, sb := range sandboxes {
			if !all && userNamespace != sb.Namespace {
				continue
			}
			sandboxID := sb.GetId()
			if trunc && len(sandboxID) > 12 {
				sandboxID = sandboxID[:12]
			}

			for _, c := range sb.Containers {
				id := c.GetId()
				if trunc && len(id) > 12 {
					id = id[:12]
				}
				imageName := c.GetImage()
				if imageName == "" {
					imageName = "-"
				}
				row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
					sb.Namespace,
					id,
					sandboxID,
					c.Type,
					ContainerStatus(c),
					imageName,
					formatTime(c.CreatedAt),
				)
				if context.Bool("wide") {
					row += fmt.Sprintf("\t%s\t%s", formatTime(c.PausedAt), displayPortMapping(sb.PortMappings))
				}
				if context.Bool("raw") {
					row += fmt.Sprintf("\t%s", utils.InterfaceToString(c))
				}
				if _, err := fmt.Fprintln(w, row); err != nil {
					return err
				}
			}
		}
		return w.Flush()
	},
}

func displayPortMapping(m []*cubebox.PortMapping) string {
	sort.SliceStable(m, func(i, j int) bool {
		return m[i].ContainerPort < m[j].ContainerPort
	})
	var s []string
	for _, p := range m {
		s = append(s, fmt.Sprintf("%d->%d", p.ContainerPort, p.HostPort))
	}
	return strings.Join(s, ",")
}

func ContainerStatus(c *cubebox.Container) string {
	switch c.GetState() {
	case cubebox.ContainerState_CONTAINER_EXITED:
		return fmt.Sprintf("Exited %s", container.TimeSinceInHuman(time.Unix(0, c.GetFinishedAt())))
	case cubebox.ContainerState_CONTAINER_RUNNING:
		return "Up"
	case cubebox.ContainerState_CONTAINER_CREATED:
		return "Created"
	case cubebox.ContainerState_CONTAINER_PAUSED:
		return "Paused"
	case cubebox.ContainerState_CONTAINER_PAUSING:
		return "Pausing"
	default:
		return c.GetState().String()
	}
}

func formatTime(created int64) string {
	if created == 0 {
		return "-"
	}
	createdAt := time.Unix(0, created).Round(time.Second).Local()
	return createdAt.Format("2006-01-02 15:04:05")
}

var ListSandboxCommand = &cli.Command{
	Name:    "sandboxes",
	Aliases: []string{"s"},
	Usage:   "list cubebox sandboxs",
	ArgsUsage: "[flags] [<filter>, ...]\n" +
		"io.kubernetes.cri.container-type [container|sandbox]\n" +
		"io.kubernetes.cri.sandbox-id xx",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "print only the container id",
		},
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "Show all containers (default shows just running)",
		},
		&cli.IntFlag{
			Name:    "last",
			Aliases: []string{"n"},
			Usage:   "Show n last created containers (includes all states)",
		},
		&cli.BoolFlag{
			Name:    "latest",
			Aliases: []string{"l"},
			Usage:   "Show the latest created container (includes all states)",
		},
		&cli.BoolFlag{
			Name:  "no-trunc",
			Usage: "Don't truncate output",
		},
		&cli.BoolFlag{
			Name:  "wide",
			Usage: "display more detailed info",
		},
	},
	Action: func(context *cli.Context) error {
		var (
			args = context.Args().Slice()
		)
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := cubebox.NewCubeboxMgrClient(conn)

		req := &cubebox.ListCubeSandboxRequest{}
		req.Filter = &cubebox.CubeSandboxFilter{}
		if !context.Bool("all") {
			state := &cubebox.ContainerStateValue{
				State: cubebox.ContainerState_CONTAINER_RUNNING,
			}
			req.Filter.State = state
		}
		if len(args) == 2 {
			req.Filter.LabelSelector = map[string]string{
				args[0]: args[1],
			}
		}

		resp, err := client.List(ctx, req)
		if err != nil {
			return err
		}

		sandboxes := resp.Items

		sort.Slice(sandboxes, func(i, j int) bool {
			return sandboxes[i].CreatedAt > sandboxes[j].CreatedAt
		})

		trunc := !context.Bool("no-trunc")
		all := context.Bool("all")
		latest := context.Bool("latest")
		lastN := context.Int("last")
		if lastN == 0 && latest {
			lastN = 1
		}
		if !all && lastN > 0 {
			if lastN < len(sandboxes) {
				sandboxes = sandboxes[:lastN]
			}
		}

		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		tabHeader := "SANDBOX\tCONTAINER\tIMAGE\tSTATUS\tCREATED\tTYPE"
		if context.Bool("wide") {
			tabHeader += "\tPORTMAPPING"
		}
		fmt.Fprintln(w, tabHeader)
		for _, sb := range sandboxes {
			sandboxID := sb.GetId()
			if trunc && len(sandboxID) > 12 {
				sandboxID = sandboxID[:12]
			}

			for _, c := range sb.Containers {
				imageName := c.GetImage()
				if imageName == "" {
					imageName = "-"
				}
				id := c.GetId()
				if trunc && len(id) > 12 {
					id = id[:12]
				}
				row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
					sandboxID,
					id,
					imageName,
					ContainerStatus(c),
					formatTime(c.CreatedAt),
					c.Type,
				)
				if context.Bool("wide") {
					row += fmt.Sprintf("\t%s", displayPortMapping(sb.PortMappings))
				}
				if _, err := fmt.Fprintln(w, row); err != nil {
					return err
				}
			}
		}
		return w.Flush()
	},
}
