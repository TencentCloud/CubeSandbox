// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/opencontainers/runtime-spec/specs-go"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	terminalFIFOPathEnv     = "CUBELET_TERMINAL_FIFO_DIR"
	defaultTerminalFIFOPath = "/data/cubelet/fifo"
	terminalCleanupTimeout  = 5 * time.Second
)

func (s *service) AttachTerminal(stream grpc.BidiStreamingServer[cubebox.TerminalClientMessage, cubebox.TerminalServerMessage]) error {
	err := terminalcore.Run(stream, &terminalProcessFactory{service: s})
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

type terminalProcessFactory struct {
	service *service
}

func (f *terminalProcessFactory) Create(ctx context.Context, open *cubebox.TerminalOpenRequest) (terminalcore.Process, error) {
	ctx = namespaces.WithNamespace(ctx, namespaces.Default)
	sandbox, err := f.service.cubeboxMgr.cubeboxManger.Get(ctx, open.GetSandboxId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", open.GetSandboxId())
	}
	namespace := namespaces.Default
	if sandbox.Namespace != "" {
		namespace = sandbox.Namespace
		ctx = namespaces.WithNamespace(ctx, namespace)
	}
	container, err := sandbox.Get(open.GetContainerId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "container %q does not belong to sandbox %q", open.GetContainerId(), open.GetSandboxId())
	}
	task, err := container.Container.Task(ctx, nil)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "container %q is not running", open.GetContainerId())
	}
	processSpec, err := buildTerminalProcessSpec(ctx, task, open)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build terminal process: %v", err)
	}

	stdinReader, stdinWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ioCreator := cio.NewCreator(
		cio.WithStreams(stdinReader, outputWriter, outputWriter),
		cio.WithTerminal,
		cio.WithFIFODir(terminalFIFOPath()),
	)
	execID := "cubelet-terminal-" + uuid.NewString()
	process, err := task.Exec(ctx, execID, processSpec, ioCreator)
	if err != nil {
		closeTerminalIO(stdinReader, stdinWriter, outputReader, outputWriter)
		return nil, status.Errorf(codes.Internal, "create terminal process: %v", err)
	}
	exitStatus, err := process.Wait(ctx)
	if err != nil {
		closeTerminalIO(stdinReader, stdinWriter, outputReader, outputWriter)
		_, _ = process.Delete(ctx, containerd.WithProcessKill)
		return nil, status.Errorf(codes.Internal, "wait for terminal process: %v", err)
	}

	result := &containerdTerminalProcess{
		ctx:          ctx,
		namespace:    namespace,
		process:      process,
		execID:       execID,
		stdinReader:  stdinReader,
		stdinWriter:  stdinWriter,
		outputReader: outputReader,
		outputWriter: outputWriter,
		exit:         make(chan terminalcore.ExitStatus, 1),
		exited:       make(chan struct{}),
	}
	go result.observeExit(exitStatus)
	return result, nil
}

func terminalFIFOPath() string {
	if configured := strings.TrimSpace(os.Getenv(terminalFIFOPathEnv)); configured != "" {
		return configured
	}
	return defaultTerminalFIFOPath
}

func buildTerminalProcessSpec(ctx context.Context, task containerd.Task, open *cubebox.TerminalOpenRequest) (*specs.Process, error) {
	spec, err := task.Spec(ctx)
	if err != nil {
		return nil, err
	}
	if spec.Process == nil {
		return nil, fmt.Errorf("container process specification is missing")
	}
	process := *spec.Process
	process.Terminal = true
	process.Args = terminalcore.Command(open.GetArgs())
	process.Env = terminalcore.MergeEnv(spec.Process.Env, open.GetEnv())
	if open.GetCwd() != "" {
		process.Cwd = open.GetCwd()
	}
	return &process, nil
}

type containerdTerminalProcess struct {
	ctx          context.Context
	namespace    string
	process      containerd.Process
	execID       string
	stdinReader  *io.PipeReader
	stdinWriter  *io.PipeWriter
	outputReader *io.PipeReader
	outputWriter *io.PipeWriter
	exit         chan terminalcore.ExitStatus
	exited       chan struct{}
	cleanupOnce  sync.Once
}

func (p *containerdTerminalProcess) ID() string                           { return p.execID }
func (p *containerdTerminalProcess) Output() io.Reader                    { return p.outputReader }
func (p *containerdTerminalProcess) Wait() <-chan terminalcore.ExitStatus { return p.exit }
func (p *containerdTerminalProcess) Start(ctx context.Context) error      { return p.process.Start(ctx) }
func (p *containerdTerminalProcess) Resize(ctx context.Context, cols, rows uint32) error {
	return p.process.Resize(ctx, cols, rows)
}
func (p *containerdTerminalProcess) WriteStdin(data []byte) error {
	_, err := p.stdinWriter.Write(data)
	return err
}

func (p *containerdTerminalProcess) observeExit(statusC <-chan containerd.ExitStatus) {
	statusValue, ok := <-statusC
	if !ok {
		p.exit <- terminalcore.ExitStatus{Err: fmt.Errorf("terminal exit channel closed")}
		close(p.exited)
		return
	}
	code, _, err := statusValue.Result()
	p.process.IO().Wait()
	_ = p.outputWriter.Close()
	p.exit <- terminalcore.ExitStatus{Code: int32(code), Err: err}
	close(p.exited)
}

func (p *containerdTerminalProcess) Cleanup() error {
	var cleanupErr error
	p.cleanupOnce.Do(func() {
		// Closing the local pipe endpoints first unblocks containerd's IO copy
		// goroutines even when the WebSocket consumer stopped reading output.
		closeTerminalIO(p.stdinWriter, p.outputReader)
		closeIOCtx, cancelCloseIO := terminalCleanupContext(p.namespace)
		_ = p.process.CloseIO(closeIOCtx, containerd.WithStdinCloser)
		cancelCloseIO()

		select {
		case <-p.exited:
		default:
			killCtx, cancelKill := terminalCleanupContext(p.namespace)
			if err := p.process.Kill(killCtx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
				cleanupErr = fmt.Errorf("kill terminal process: %w", err)
			}
			cancelKill()

			waitTimer := time.NewTimer(terminalCleanupTimeout)
			select {
			case <-p.exited:
				if !waitTimer.Stop() {
					<-waitTimer.C
				}
			case <-waitTimer.C:
				if cleanupErr == nil {
					cleanupErr = fmt.Errorf("wait for terminal cleanup: %w", context.DeadlineExceeded)
				}
			}
		}
		// Delete always gets a fresh context. A timeout while waiting for exit
		// must not turn deletion into an immediate no-op with an expired context.
		deleteCtx, cancelDelete := terminalCleanupContext(p.namespace)
		_, deleteErr := p.process.Delete(deleteCtx, containerd.WithProcessKill)
		cancelDelete()
		if deleteErr != nil && !errdefs.IsNotFound(deleteErr) && cleanupErr == nil {
			cleanupErr = fmt.Errorf("delete terminal process: %w", deleteErr)
		}
		closeTerminalIO(p.stdinReader, p.outputReader, p.outputWriter)
	})
	if cleanupErr != nil {
		log.G(p.ctx).WithError(cleanupErr).Warnf("terminal process %s cleanup failed", p.execID)
	}
	return cleanupErr
}

func terminalCleanupContext(namespace string) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		namespaces.WithNamespace(context.Background(), namespace),
		terminalCleanupTimeout,
	)
}

func closeTerminalIO(closers ...io.Closer) {
	for _, closer := range closers {
		if closer != nil {
			_ = closer.Close()
		}
	}
}
