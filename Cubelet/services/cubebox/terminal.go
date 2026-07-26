// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	terminalProcessCleanupTimeout = 5 * time.Second
	terminalFIFODir               = "/data/cubelet/fifo"
	terminalMaxStdinFrame         = 64 * 1024
	terminalStdinQueueDepth       = 8
	terminalMaxCols               = 512
	terminalMaxRows               = 256
)

// terminalStreamWriter serializes container stdout/stderr into the gRPC stream.
// containerd may copy both pipes concurrently, while gRPC permits only one
// concurrent Send call per stream.
type terminalStreamWriter struct {
	stream cubebox.CubeboxMgr_AttachTerminalServer
	mu     sync.Mutex
}

func (w *terminalStreamWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.stream.Send(&cubebox.TerminalServerMessage{
		Payload: &cubebox.TerminalServerMessage_Output{Output: data},
	}); err != nil {
		return 0, err
	}
	return len(data), nil
}

// AttachTerminal owns one interactive process for the lifetime of the gRPC
// stream. The first frame selects the sandbox/container; later frames only
// provide stdin or resize events, so a connected browser cannot retarget a
// session to another container.
func (s *service) AttachTerminal(stream cubebox.CubeboxMgr_AttachTerminalServer) error {
	openFrame, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "terminal open frame is required")
		}
		return err
	}
	open := openFrame.GetOpen()
	if open == nil || open.GetSandboxId() == "" || open.GetContainerId() == "" {
		return status.Error(codes.InvalidArgument, "terminal open frame must provide sandbox_id and container_id")
	}

	ctx, cancel := context.WithCancel(
		namespaces.WithNamespace(stream.Context(), namespaces.Default),
	)
	defer cancel()
	sandbox, err := s.cubeboxMgr.cubeboxManger.Get(ctx, open.GetSandboxId())
	if err != nil {
		return status.Errorf(codes.NotFound, "sandbox %q not found: %v", open.GetSandboxId(), err)
	}
	if sandbox.Namespace != "" {
		ctx = namespaces.WithNamespace(ctx, sandbox.Namespace)
	}
	container, err := sandbox.Get(open.GetContainerId())
	if err != nil {
		return status.Errorf(codes.NotFound, "container %q not found: %v", open.GetContainerId(), err)
	}
	if container.IsPod {
		return status.Error(codes.PermissionDenied, "terminal target must be a workload container")
	}
	task, err := container.Container.Task(ctx, nil)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "container task is not available: %v", err)
	}

	processSpec, err := terminalProcessSpec(ctx, task, open)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid terminal process: %v", err)
	}
	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	stdinQueue := make(chan []byte, terminalStdinQueueDepth)
	stdinErr := make(chan error, 1)
	go func() {
		for {
			select {
			case data := <-stdinQueue:
				if _, writeErr := stdinWriter.Write(data); writeErr != nil {
					stdinErr <- writeErr
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	output := &terminalStreamWriter{stream: stream}
	process, err := task.Exec(ctx, "cubelet-terminal-"+uuid.NewString(), processSpec,
		cio.NewCreator(cio.WithStreams(stdinReader, output, output), cio.WithTerminal, cio.WithFIFODir(terminalFIFODir)))
	if err != nil {
		return status.Errorf(codes.Internal, "create terminal process: %v", err)
	}
	var processExited atomic.Bool
	processStarted := false
	defer func() {
		cleanupTerminalProcess(ctx, process, processStarted, processExited.Load())
	}()

	// containerd requires subscribing with Wait before Start so a short-lived
	// exec cannot exit before its notification channel is registered.
	exitStatus, err := process.Wait(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "wait for terminal process: %v", err)
	}
	if err := process.Start(ctx); err != nil {
		return status.Errorf(codes.Internal, "start terminal process: %v", err)
	}
	processStarted = true
	if err := resizeTerminal(ctx, process, open.GetCols(), open.GetRows()); err != nil {
		return status.Errorf(codes.Internal, "resize terminal: %v", err)
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			frame, receiveErr := stream.Recv()
			if receiveErr != nil {
				recvErr <- receiveErr
				return
			}
			switch payload := frame.GetPayload().(type) {
			case *cubebox.TerminalClientMessage_Stdin:
				if receiveErr = enqueueTerminalStdin(stdinQueue, payload.Stdin); receiveErr != nil {
					recvErr <- receiveErr
					return
				}
			case *cubebox.TerminalClientMessage_Resize:
				if receiveErr = resizeTerminal(ctx, process, payload.Resize.GetCols(), payload.Resize.GetRows()); receiveErr != nil {
					recvErr <- receiveErr
					return
				}
			default:
				recvErr <- status.Error(codes.InvalidArgument, "terminal frame type is not valid after open")
				return
			}
		}
	}()

	select {
	case result := <-exitStatus:
		processExited.Store(true)
		process.IO().Wait()
		code, _, resultErr := result.Result()
		if resultErr != nil {
			return status.Errorf(codes.Internal, "read terminal exit status: %v", resultErr)
		}
		return stream.Send(&cubebox.TerminalServerMessage{
			Payload: &cubebox.TerminalServerMessage_Exit{Exit: &cubebox.TerminalExit{ExitCode: int32(code)}},
		})
	case receiveErr := <-recvErr:
		if receiveErr == io.EOF || receiveErr == context.Canceled {
			return nil
		}
		return receiveErr
	case writeErr := <-stdinErr:
		return writeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func enqueueTerminalStdin(queue chan<- []byte, data []byte) error {
	if len(data) > terminalMaxStdinFrame {
		return status.Errorf(codes.ResourceExhausted,
			"terminal stdin frame exceeds %d bytes", terminalMaxStdinFrame)
	}
	cloned := append([]byte(nil), data...)
	select {
	case queue <- cloned:
		return nil
	default:
		return status.Error(codes.ResourceExhausted, "terminal stdin queue is full")
	}
}

func cleanupTerminalProcess(
	ctx context.Context,
	process containerd.Process,
	processStarted bool,
	processExited bool,
) {
	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		namespace = namespaces.Default
	}
	cleanupCtx, cancel := context.WithTimeout(
		namespaces.WithNamespace(context.Background(), namespace),
		terminalProcessCleanupTimeout,
	)
	defer cancel()

	if processStarted && !processExited {
		exitStatus, err := process.Wait(cleanupCtx)
		if err != nil {
			log.G(cleanupCtx).Warnf("terminal: wait for exec process cleanup failed: %v", err)
		}
		if err := process.Kill(cleanupCtx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
			log.G(cleanupCtx).Warnf("terminal: kill exec process failed: %v", err)
		}
		if exitStatus != nil {
			select {
			case <-exitStatus:
			case <-cleanupCtx.Done():
				log.G(cleanupCtx).Warn("terminal: timed out waiting for exec process to exit")
			}
		}
	}
	if _, err := process.Delete(cleanupCtx); err != nil && !errdefs.IsNotFound(err) {
		log.G(cleanupCtx).Warnf("terminal: delete exec process failed: %v", err)
	}
}

func terminalProcessSpec(ctx context.Context, task containerd.Task, open *cubebox.TerminalOpenRequest) (*specs.Process, error) {
	spec, err := task.Spec(ctx)
	if err != nil {
		return nil, err
	}
	if spec.Process == nil {
		return nil, fmt.Errorf("container process spec is nil")
	}
	return terminalProcessFromSpec(spec.Process, open), nil
}

func terminalProcessFromSpec(base *specs.Process, open *cubebox.TerminalOpenRequest) *specs.Process {
	process := *base
	process.Terminal = true
	process.Args = slices.Clone(open.GetArgs())
	if len(process.Args) == 0 {
		process.Args = []string{"/bin/sh"}
	}
	if open.GetCwd() != "" {
		process.Cwd = open.GetCwd()
	}
	process.Env = mergeTerminalEnv(base.Env, open.GetEnv())
	// The struct copy above shares slice backing arrays with base; clone the
	// remaining mutable slice so later overrides can never mutate the base spec.
	process.Rlimits = slices.Clone(base.Rlimits)
	return &process
}

func mergeTerminalEnv(base, overrides []string) []string {
	merged := make([]string, 0, len(base)+len(overrides))
	indices := make(map[string]int, len(base)+len(overrides))
	apply := func(entries []string) {
		for _, value := range entries {
			for index, char := range value {
				if char != '=' {
					continue
				}
				key := value[:index]
				if existing, ok := indices[key]; ok {
					merged[existing] = value
				} else {
					indices[key] = len(merged)
					merged = append(merged, value)
				}
				break
			}
		}
	}
	apply(base)
	apply(overrides)
	return merged
}

func resizeTerminal(ctx context.Context, process containerd.Process, cols, rows uint32) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	if cols > terminalMaxCols {
		cols = terminalMaxCols
	}
	if rows > terminalMaxRows {
		rows = terminalMaxRows
	}
	return process.Resize(ctx, cols, rows)
}
