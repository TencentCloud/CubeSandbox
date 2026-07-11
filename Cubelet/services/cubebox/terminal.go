// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"fmt"
	"io"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		Payload: &cubebox.TerminalServerMessage_Output{Output: append([]byte(nil), data...)},
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

	ctx := namespaces.WithNamespace(stream.Context(), namespaces.Default)
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
	task, err := container.Container.Task(ctx, nil)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "container is not running: %v", err)
	}

	processSpec, err := terminalProcessSpec(ctx, task, open)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid terminal process: %v", err)
	}
	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	output := &terminalStreamWriter{stream: stream}
	process, err := task.Exec(ctx, "cubelet-terminal-"+uuid.NewString(), processSpec,
		cio.NewCreator(cio.WithStreams(stdinReader, output, output), cio.WithTerminal, cio.WithFIFODir("/data/cubelet/fifo")))
	if err != nil {
		return status.Errorf(codes.Internal, "create terminal process: %v", err)
	}
	defer process.Delete(context.Background())

	exitStatus, err := process.Wait(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "wait for terminal process: %v", err)
	}
	if err := process.Start(ctx); err != nil {
		return status.Errorf(codes.Internal, "start terminal process: %v", err)
	}
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
				if _, receiveErr = stdinWriter.Write(payload.Stdin); receiveErr != nil {
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
	case <-ctx.Done():
		return ctx.Err()
	}
}

func terminalProcessSpec(ctx context.Context, task containerd.Task, open *cubebox.TerminalOpenRequest) (*specs.Process, error) {
	spec, err := task.Spec(ctx)
	if err != nil {
		return nil, err
	}
	process := spec.Process
	process.Terminal = true
	process.Args = open.GetArgs()
	if len(process.Args) == 0 {
		process.Args = []string{"/bin/sh"}
	}
	if open.GetCwd() != "" {
		process.Cwd = open.GetCwd()
	}
	process.Env = mergeTerminalEnv(spec.Process.Env, open.GetEnv())
	return process, nil
}

func mergeTerminalEnv(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, value := range append(append([]string(nil), base...), overrides...) {
		for index, char := range value {
			if char == '=' {
				values[value[:index]] = value[index+1:]
				break
			}
		}
	}
	merged := make([]string, 0, len(values))
	for key, value := range values {
		merged = append(merged, fmt.Sprintf("%s=%s", key, value))
	}
	return merged
}

func resizeTerminal(ctx context.Context, process containerd.Process, cols, rows uint32) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return process.Resize(ctx, cols, rows)
}
