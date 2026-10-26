// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	"context"
	gocontext "context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/typeurl/v2"
	"github.com/docker/go-units"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/urfave/cli/v2"
)

var ListCommand = &cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "list containers",
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
	},
	Action: func(context *cli.Context) error {
		var (
			args    = context.Args()
			quiet   = context.Bool("quiet")
			filters []string
		)
		cntdClient, err := containerd.New(context.String("address"),
			containerd.WithDefaultPlatform(platforms.Default()),
		)
		if err != nil {
			return err
		}
		cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))
		cntCtx, cntCancel := gocontext.WithTimeout(cntCtx, context.Duration("timeout"))
		defer cntCancel()

		if args.Len() == 1 {
			filters = []string{
				fmt.Sprintf("id~=^%s.*$", regexp.QuoteMeta(args.Get(0))),
			}
		}

		if args.Len() == 2 {
			filters = append(filters, fmt.Sprintf("labels.%q==%s", args.Get(0), args.Get(1)))
		}

		if context.IsSet("sandbox") {
			filters = append(filters, fmt.Sprintf("labels.\"io.kubernetes.cri.sandbox-id\"==%s",
				context.String("sandbox")))
		}

		containers, err := cntdClient.Containers(cntCtx, filters...)
		if err != nil {
			return err
		}
		trunc := !context.Bool("no-trunc")
		all := context.Bool("all")
		latest := context.Bool("latest")
		lastN := context.Int("last")
		withoutSandbox := context.Bool("without-sandbox-id")
		if lastN == -1 && latest {
			lastN = 1
		}
		if !all && lastN > 0 {
			all = true
			sort.Slice(containers, func(i, j int) bool {
				infoI, _ := containers[i].Info(cntCtx, containerd.WithoutRefreshedMetadata)
				infoJ, _ := containers[j].Info(cntCtx, containerd.WithoutRefreshedMetadata)
				return infoI.CreatedAt.After(infoJ.CreatedAt)
			})
			if lastN < len(containers) {
				containers = containers[:lastN]
			}
		}

		if quiet {
			for _, c := range containers {
				fmt.Printf("%s\n", c.ID())
			}
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		title := "CONTAINER"
		if !withoutSandbox {
			title += "\tSANDBOX"
		}
		title += "\tTYPE\tIMAGE\tSTATUS\tCREATED"

		fmt.Fprintln(w, title)
		for _, c := range containers {
			info, err := c.Info(cntCtx, containerd.WithoutRefreshedMetadata)
			if err != nil {
				if errdefs.IsNotFound(err) {
					log.L.WithError(err).Error("container not found")
					continue
				}
				return err
			}

			imageName := info.Image
			if imageName == "" {
				imageName = "-"
			}
			sandboxID := info.SandboxID
			if trunc && len(sandboxID) > 12 {
				sandboxID = sandboxID[:12]
			}
			id := c.ID()
			if trunc && len(id) > 12 {
				id = id[:12]
			}
			cStatus := ContainerStatus(cntCtx, c)
			ctype := getContainerType(info.Labels)
			if _, err := fmt.Fprintf(w, "%s", id); err != nil {
				return err
			}
			if !withoutSandbox {
				if _, err := fmt.Fprintf(w, "\t%s", sandboxID); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "\t%s\t%s\t%s\t%s",
				ctype,
				imageName,
				cStatus,
				info.CreatedAt.Round(time.Second).Local().String(),
			); err != nil {
				return err
			}

			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return err
			}
		}
		return w.Flush()
	},
}

func getContainerType(containerLabels map[string]string) string {
	if t, ok := containerLabels[constants.ContainerType]; ok {
		return t
	}
	return "unknown"
}

func ContainerStatus(ctx gocontext.Context, c containerd.Container) string {

	ctx, cancel := gocontext.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	task, err := c.Task(ctx, nil)
	if err != nil {

		if errdefs.IsNotFound(err) {
			return strings.Title(string(containerd.Created))
		}
		return strings.Title(string(containerd.Unknown))
	}

	status, err := task.Status(ctx)
	if err != nil {
		return strings.Title(string(containerd.Unknown))
	}
	switch s := status.Status; s {
	case containerd.Stopped:
		return fmt.Sprintf("Exited (%v) %s", status.ExitStatus, TimeSinceInHuman(status.ExitTime))
	case containerd.Running:
		return "Up"
	default:
		return strings.Title(string(s))
	}
}

func TimeSinceInHuman(since time.Time) string {
	return fmt.Sprintf("%s ago", units.HumanDuration(time.Since(since)))
}

var InfoCommand = &cli.Command{
	Name:      "info",
	Usage:     "get info about a container",
	ArgsUsage: "CONTAINER",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "spec",
			Usage: "only display the spec",
		},
	},
	Action: func(context *cli.Context) error {
		id := context.Args().First()
		if id == "" {
			return fmt.Errorf("container id must be provided: %w", errdefs.ErrInvalidArgument)
		}
		cntdClient, err := containerd.New(context.String("address"),
			containerd.WithDefaultPlatform(platforms.Default()),
		)
		if err != nil {
			return err
		}
		cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))
		cntCtx, cntCancel := gocontext.WithTimeout(cntCtx, context.Duration("timeout"))
		defer cntCancel()
		filters := []string{
			fmt.Sprintf("id~=^%s.*$", regexp.QuoteMeta(id)),
		}

		cntrs, err := cntdClient.Containers(cntCtx, filters...)
		if err != nil {
			return err
		}
		for _, c := range cntrs {
			info, err := c.Info(cntCtx, containerd.WithoutRefreshedMetadata)
			if err != nil {
				if errdefs.IsNotFound(err) {
					log.L.WithError(err).Error("container not found")
					continue
				}
				return err
			}
			if context.Bool("spec") {
				v, err := typeurl.UnmarshalAny(info.Spec)
				if err != nil {
					fmt.Printf("failed to unmarshal container spec with url [%s]: %s", info.Spec.GetTypeUrl(), string(info.Spec.GetValue()))
					return err
				}
				commands.PrintAsJSON(v)
				return nil
			}

			if info.Spec != nil && info.Spec.GetValue() != nil {
				v, err := typeurl.UnmarshalAny(info.Spec)
				if err != nil {
					return fmt.Errorf("failed to unmarshal container spec with url %s: %w", info.Spec.GetTypeUrl(), err)
				}
				commands.PrintAsJSON(struct {
					containers.Container
					Spec interface{} `json:"Spec,omitempty"`
				}{
					Container: info,
					Spec:      v,
				})
				return nil
			}
			commands.PrintAsJSON(info)
		}

		return nil
	},
}

var deleteCommand = &cli.Command{
	Name:      "delete",
	Usage:     "Delete one or more existing containers",
	ArgsUsage: "[flags] CONTAINER [CONTAINER, ...]",
	Aliases:   []string{"del", "remove", "rm"},
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "keep-snapshot",
			Usage: "Do not clean up snapshot with container",
		},
	},
	Action: func(cliContext *cli.Context) error {
		var exitErr error
		client, ctx, cancel, err := commands.NewClient(cliContext)
		if err != nil {
			return err
		}
		defer cancel()
		deleteOpts := []containerd.DeleteOpts{}
		if !cliContext.Bool("keep-snapshot") {
			deleteOpts = append(deleteOpts, containerd.WithSnapshotCleanup)
		}

		if cliContext.NArg() == 0 {
			return fmt.Errorf("must specify at least one container to delete: %w", errdefs.ErrInvalidArgument)
		}

		for _, arg := range cliContext.Args().Slice() {
			if err := deleteContainer(ctx, client, arg, deleteOpts...); err != nil {
				if exitErr == nil {
					exitErr = err
				}
				log.G(ctx).WithError(err).Errorf("failed to delete container %q", arg)
			}
		}
		return exitErr
	},
}

func deleteContainer(ctx context.Context, client *containerd.Client, id string, opts ...containerd.DeleteOpts) error {
	filters := []string{
		fmt.Sprintf("id~=^%s.*$", regexp.QuoteMeta(id)),
	}

	cntrs, err := client.Containers(ctx, filters...)
	if err != nil {
		return err
	}
	for _, container := range cntrs {
		task, err := container.Task(ctx, cio.Load)
		if err != nil {
			err = container.Delete(ctx, opts...)
			if err != nil {
				return fmt.Errorf("failed to delete container: %w", err)
			}
			continue
		}
		status, err := task.Status(ctx)
		if err != nil {
			return fmt.Errorf("failed to get task status: %w", err)
		}
		if status.Status == containerd.Stopped || status.Status == containerd.Created {
			if _, err := task.Delete(ctx); err != nil {
				return fmt.Errorf("failed to stop container: %w", err)
			}
			err = container.Delete(ctx, opts...)
			if err != nil {
				return fmt.Errorf("failed to delete container after stop container: %w", err)
			}
			continue
		}
		return fmt.Errorf("cannot delete a non stopped container: %v", status)
	}
	return nil
}
