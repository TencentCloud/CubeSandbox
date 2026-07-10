// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const terminalFIFOPath = "/data/cubelet/fifo"

var errTerminalClientClosed = errors.New("terminal client closed the session")

// Terminal runs one independent interactive process for each gRPC stream.
func (s *service) Terminal(stream grpc.BidiStreamingServer[cubebox.TerminalMessage, cubebox.TerminalMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "terminal open message is required: %v", err)
	}
	open := first.GetOpen()
	if err := validateTerminalOpen(open); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	ctx := namespaces.WithNamespace(stream.Context(), namespaces.Default)
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, open.SandboxId)
	if err != nil {
		return status.Errorf(codes.NotFound, "failed to get sandbox %q: %v", open.SandboxId, err)
	}
	terminalNamespace := namespaces.Default
	if sb.Namespace != "" {
		terminalNamespace = sb.Namespace
		ctx = namespaces.WithNamespace(ctx, terminalNamespace)
	}
	container, err := sb.Get(open.ContainerId)
	if err != nil {
		return status.Errorf(codes.NotFound, "container %q does not belong to sandbox %q: %v", open.ContainerId, open.SandboxId, err)
	}
	task, err := container.Container.Task(ctx, nil)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "container %q is not running: %v", open.ContainerId, err)
	}

	args := open.Args
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}
	processSpec, err := generateExecProcessSpec(ctx, task, &cubebox.ExecCubeSandboxRequest{
		SandboxId:   open.SandboxId,
		ContainerId: open.ContainerId,
		Terminal:    true,
		Args:        args,
		Env:         open.Env,
		Cwd:         open.Cwd,
	})
	if err != nil {
		return status.Errorf(codes.Internal, "failed to build terminal process spec: %v", err)
	}

	stdinReader, stdinWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ioCreator := cio.NewCreator(
		cio.WithStreams(stdinReader, outputWriter, outputWriter),
		cio.WithTerminal,
		cio.WithFIFODir(terminalFIFOPath),
	)
	execID := "terminal-" + uuid.NewString()
	process, err := task.Exec(ctx, execID, processSpec, ioCreator)
	if err != nil {
		closeTerminalPipes(stdinReader, stdinWriter, outputReader, outputWriter)
		return status.Errorf(codes.Internal, "failed to create terminal process: %v", err)
	}

	cleanupOnce := sync.Once{}
	cleanup := func() {
		cleanupOnce.Do(func() {
			closeTerminalPipes(stdinReader, stdinWriter, outputReader, outputWriter)
			cleanupCtx, cancel := terminalCleanupContext(terminalNamespace)
			defer cancel()
			_ = process.CloseIO(cleanupCtx, containerd.WithStdinCloser)
			if _, err := process.Delete(cleanupCtx, containerd.WithProcessKill); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to delete terminal process %s", execID)
			}
		})
	}
	defer cleanup()

	statusC, err := process.Wait(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to wait for terminal process: %v", err)
	}
	if err := process.Start(ctx); err != nil {
		return status.Errorf(codes.Internal, "failed to start terminal process: %v", err)
	}
	if open.Cols > 0 && open.Rows > 0 {
		if err := process.Resize(ctx, open.Cols, open.Rows); err != nil {
			return status.Errorf(codes.Internal, "failed to set initial terminal size: %v", err)
		}
	}

	sender := &terminalStreamSender{stream: stream}
	if err := sender.send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Started{
		Started: &cubebox.TerminalStarted{ExecId: execID},
	}}); err != nil {
		return err
	}

	outputDone := make(chan error, 1)
	go func() {
		outputDone <- copyTerminalOutput(outputReader, sender)
	}()
	receiveDone := make(chan error, 1)
	go func() {
		receiveDone <- receiveTerminalInput(ctx, stream, process, stdinWriter)
	}()

	log.G(ctx).WithFields(map[string]interface{}{
		"requestID":   open.RequestId,
		"sandboxID":   open.SandboxId,
		"containerID": open.ContainerId,
		"execID":      execID,
	}).Info("terminal process started")

	for {
		select {
		case exitStatus := <-statusC:
			code, _, resultErr := exitStatus.Result()
			if resultErr != nil {
				_ = sender.sendError(fmt.Sprintf("failed to read terminal exit status: %v", resultErr))
				return resultErr
			}
			ioWaitDone := make(chan struct{})
			go func() {
				process.IO().Wait()
				close(ioWaitDone)
			}()
			select {
			case <-ioWaitDone:
			case <-time.After(time.Second):
			}
			_ = outputWriter.Close()
			if outputDone != nil {
				select {
				case <-outputDone:
				case <-time.After(time.Second):
				}
			}
			return sender.send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Exit{
				Exit: &cubebox.TerminalExit{Code: code},
			}})
		case err := <-receiveDone:
			if err == nil || errors.Is(err, errTerminalClientClosed) || errors.Is(err, io.EOF) {
				return nil
			}
			_ = sender.sendError(err.Error())
			return err
		case err := <-outputDone:
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				outputDone = nil
				continue
			}
			return err
		case <-stream.Context().Done():
			return nil
		}
	}
}

func terminalCleanupContext(namespace string) (context.Context, context.CancelFunc) {
	ctx := namespaces.WithNamespace(context.Background(), namespace)
	return context.WithTimeout(ctx, 5*time.Second)
}

func validateTerminalOpen(open *cubebox.TerminalOpen) error {
	if open == nil {
		return errors.New("the first terminal message must be open")
	}
	if open.SandboxId == "" {
		return errors.New("sandbox_id is required")
	}
	if open.ContainerId == "" {
		return errors.New("container_id is required")
	}
	if (open.Cols == 0) != (open.Rows == 0) {
		return errors.New("cols and rows must both be positive or both be omitted")
	}
	return nil
}

type terminalMessageStream interface {
	Send(*cubebox.TerminalMessage) error
	Recv() (*cubebox.TerminalMessage, error)
}

type terminalStreamSender struct {
	stream terminalMessageStream
	mu     sync.Mutex
}

func (s *terminalStreamSender) send(message *cubebox.TerminalMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(message)
}

func (s *terminalStreamSender) sendError(message string) error {
	return s.send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Error{
		Error: &cubebox.TerminalError{Message: message},
	}})
}

type terminalProcess interface {
	Resize(context.Context, uint32, uint32) error
}

func receiveTerminalInput(
	ctx context.Context,
	stream terminalMessageStream,
	process terminalProcess,
	stdin *io.PipeWriter,
) error {
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		switch payload := message.Message.(type) {
		case *cubebox.TerminalMessage_Input:
			if len(payload.Input) > 0 {
				if _, err := stdin.Write(payload.Input); err != nil {
					return fmt.Errorf("failed to write terminal input: %w", err)
				}
			}
		case *cubebox.TerminalMessage_Resize:
			if payload.Resize == nil || payload.Resize.Cols == 0 || payload.Resize.Rows == 0 {
				return errors.New("terminal resize requires positive cols and rows")
			}
			if err := process.Resize(ctx, payload.Resize.Cols, payload.Resize.Rows); err != nil {
				return fmt.Errorf("failed to resize terminal: %w", err)
			}
		case *cubebox.TerminalMessage_Close:
			return errTerminalClientClosed
		default:
			return errors.New("terminal stream accepts only input, resize, or close after open")
		}
	}
}

func copyTerminalOutput(reader io.Reader, sender *terminalStreamSender) error {
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			output := append([]byte(nil), buffer[:n]...)
			if sendErr := sender.send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Output{Output: output}}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			return err
		}
	}
}

func closeTerminalPipes(pipes ...io.Closer) {
	for _, pipe := range pipes {
		_ = pipe.Close()
	}
}
