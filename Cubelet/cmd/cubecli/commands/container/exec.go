// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	gocontext "context"
	"errors"
	"fmt"
	"github.com/containerd/console"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/cmd/ctr/commands/tasks"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubecommands "github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

var ExecCommand = &cli.Command{
	Name:                   "exec",
	Usage:                  "exec [OPTIONS] CONTAINER COMMAND [ARG...]",
	UseShortOptionHandling: true,
	ArgsUsage:              "[flags] CONTAINER COMMAND [ARG...]",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "tty",
			Aliases: []string{"t"},
			Usage:   "(Currently -t needs to correspond to -i)",
		},
		&cli.BoolFlag{
			Name:    "interactive",
			Aliases: []string{"i"},
			Usage:   "Keep STDIN open even if not attached",
		},
		&cli.StringFlag{
			Name:    "workdir",
			Aliases: []string{"w"},
			Usage:   "Working directory inside the container",
		},

		&cli.BoolFlag{
			Name:    "detach",
			Aliases: []string{"d"},
			Usage:   "Detached mode: run command in the background",
		},
	},
	Action: func(context *cli.Context) error {
		var (
			args = context.Args()
		)
		if args.Len() < 2 {
			return fmt.Errorf("requires at least %d arg(s), only received %d", 2, args.Len())
		}

		newArg := []string{}
		if args.Len() >= 2 {
			if args.Get(1) == "--" {
				newArg = append(newArg, args.Slice()[:1]...)
				newArg = append(newArg, args.Slice()[2:]...)
			} else {
				newArg = args.Slice()
			}
		}

		cntdClient, err := containerd.New(context.String("address"),
			containerd.WithDefaultPlatform(platforms.Default()))
		if err != nil {
			return fmt.Errorf("init containerd connect failed.%s", err)
		}
		cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))

		containerIDKey := newArg[0]
		containers, err := findContainer(cntCtx, containerIDKey, cntdClient)
		if err != nil {
			return err
		}
		if len(containers) == 0 {
			containerIDKey, err = findCubeboxContainer(context, containerIDKey)
			if err != nil {
				return err
			}
			containers, err = findContainer(cntCtx, containerIDKey, cntdClient)
			if err != nil {
				return err
			}
			if len(containers) == 0 {
				return fmt.Errorf("no such container %s", containerIDKey)
			}
		}

		flagI := context.Bool("interactive")
		flagT := context.Bool("tty")
		flagD := context.Bool("detach")

		if flagI {
			if flagD {
				return errors.New("currently flag -i and -d cannot be specified together (FIXME)")
			}
		}

		if flagT {
			if flagD {
				return errors.New("currently flag -t and -d cannot be specified together (FIXME)")
			}
		}

		container := containers[0]
		pspec, err := generateExecProcessSpec(cntCtx, context, newArg, container, cntdClient)
		if err != nil {
			return fmt.Errorf("failed to generate exec process spec: %w", err)
		}

		task, err := container.Task(cntCtx, nil)
		if err != nil {
			return fmt.Errorf("failed to get task: %w", err)
		}
		var (
			ioCreator cio.Creator
			stdinC    = &StdinCloser{
				Stdin: os.Stdin,
			}
			con console.Console
		)

		cioOpts := []cio.Opt{cio.WithFIFODir("/data/cubelet/fifo")}
		if flagT {
			con = console.Current()
			defer con.Reset()
			if err := con.SetRaw(); err != nil {
				return err
			}
			cioOpts = append(cioOpts, cio.WithStreams(con, con, nil), cio.WithTerminal, cio.WithFIFODir("/data/cubelet/fifo"))
		} else {
			cioOpts = append(cioOpts, cio.WithStreams(stdinC, os.Stdout, os.Stderr))
		}
		ioCreator = cio.NewCreator(cioOpts...)

		execID := "exec-" + utils.GenerateID()
		process, err := task.Exec(cntCtx, execID, pspec, ioCreator)
		if err != nil {
			return fmt.Errorf("failed to exec: %w", err)
		}
		stdinC.Closer = func() {
			process.CloseIO(cntCtx, containerd.WithStdinCloser)
		}

		if !flagD {
			defer process.Delete(cntCtx)
		}

		statusC, err := process.Wait(cntCtx)
		if err != nil {
			return fmt.Errorf("failed to get wait channel: %w", err)
		}

		if !flagD {
			if flagT {
				if err := tasks.HandleConsoleResize(cntCtx, process, con); err != nil {
					logrus.WithError(err).Error("console resize")
				}
			} else {
				sigc := commands.ForwardAllSignals(cntCtx, process)
				defer commands.StopCatch(sigc)
			}
		}

		if err := process.Start(cntCtx); err != nil {
			return err
		}
		if flagD {
			return nil
		}
		status := <-statusC
		code, _, err := status.Result()
		if err != nil {
			return fmt.Errorf("failed to get exit code: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("exec failed with exit code %d", code)
		}
		return nil
	},
}

func findContainer(ctx gocontext.Context, id string, cntdClient *containerd.Client) ([]containerd.Container, error) {
	filters := []string{
		fmt.Sprintf("id~=^%s.*$", regexp.QuoteMeta(id)),
	}

	containers, err := cntdClient.Containers(ctx, filters...)
	if err != nil {
		return nil, err
	}

	if len(containers) > 1 {
		return nil, fmt.Errorf("ambiguous ID %q", id)
	}

	return containers, nil
}

func findCubeboxContainer(cliCtx *cli.Context, containerID string) (string, error) {
	var (
		id = ""
	)
	conn, ctx, cancel, err := cubecommands.NewGrpcConn(cliCtx)
	if err != nil {
		return id, err
	}
	defer conn.Close()
	defer cancel()
	client := cubebox.NewCubeboxMgrClient(conn)

	req := &cubebox.ListCubeSandboxRequest{}
	req.Filter = &cubebox.CubeSandboxFilter{}

	resp, err := client.List(ctx, req)
	if err != nil {
		return id, err
	}

	for _, sb := range resp.Items {
		if strings.HasPrefix(sb.Id, containerID) {
			for _, sc := range sb.Containers {
				if sc.Type == "sandbox" {
					id = sc.Id
					return id, nil
				}
			}
		}
	}
	return id, fmt.Errorf("container %s not found", containerID)
}

func generateExecProcessSpec(ctx gocontext.Context, context *cli.Context, args []string, container containerd.Container, client *containerd.Client) (*specs.Process, error) {
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get container spec: %w", err)
	}
	pspec := spec.Process
	pspec.Terminal = context.Bool("tty")
	pspec.Args = args[1:]

	if context.String("workdir") != "" {
		pspec.Cwd = context.String("workdir")
	}
	return pspec, nil
}

type StdinCloser struct {
	mu     sync.Mutex
	Stdin  *os.File
	Closer func()
	closed bool
}

func (s *StdinCloser) Read(p []byte) (int, error) {
	n, err := s.Stdin.Read(p)
	if errors.Is(err, io.EOF) && s.Closer != nil {
		s.Closer()
	}
	return n, err
}
