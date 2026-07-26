// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	terminalFIFOPath             = "/data/cubelet/fifo"
	terminalInputQueueDepth      = 32
	maxTerminalInputMessageBytes = 256 * 1024
	maxTerminalOpenBytes         = 64 * 1024
	maxTerminalDimension         = 1000
	terminalCleanupTimeout       = 5 * time.Second
	terminalOutputDrainTimeout   = time.Second
)

var errTerminalClientClosed = errors.New("terminal client closed the session")

// Terminal runs one independent interactive process in the selected container.
func (s *service) Terminal(stream grpc.BidiStreamingServer[cubebox.TerminalMessage, cubebox.TerminalMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "terminal open message is required: %v", err)
	}
	open := first.GetOpen()
	if err := validateTerminalOpen(open); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	open.SandboxId = strings.TrimSpace(open.SandboxId)
	open.ContainerId = strings.TrimSpace(open.ContainerId)

	ctx := namespaces.WithNamespace(stream.Context(), namespaces.Default)
	ctx, task, namespace, err := s.resolveExecTask(ctx, open.SandboxId, open.ContainerId)
	if err != nil {
		code := codes.FailedPrecondition
		var targetErr *execTargetError
		if errors.As(err, &targetErr) && targetErr.stage != "task" {
			code = codes.NotFound
		}
		return status.Error(code, err.Error())
	}

	args := open.Args
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}
	processSpec, err := generateExecProcessSpec(ctx, task, &cubebox.ExecCubeSandboxRequest{
		RequestID:   open.RequestId,
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
	processSpec.Env = terminalEnv(processSpec.Env)
	// Web terminal tickets currently authorize a root shell. Keep that contract
	// explicit instead of inheriting an image's non-root default user.
	// TODO: propagate an authorized execution identity before enabling non-root users.
	processSpec.User.UID = 0
	processSpec.User.GID = 0
	processSpec.User.AdditionalGids = nil

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

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cleanupTerminalProcess(ctx, namespace, execID, process, stdinReader, stdinWriter, outputReader, outputWriter)
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
	if open.Cols > 0 {
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

	outputDoneCh := make(chan error, 1)
	go func() { outputDoneCh <- copyTerminalOutput(outputReader, sender) }()
	var outputDone <-chan error = outputDoneCh

	inputQueue := make(chan []byte, terminalInputQueueDepth)
	inputDoneCh := make(chan error, 1)
	go func() { inputDoneCh <- writeTerminalInput(stdinWriter, inputQueue) }()
	var inputDone <-chan error = inputDoneCh

	receiveDone := make(chan error, 1)
	go func() {
		defer close(inputQueue)
		receiveDone <- receiveTerminalCommands(ctx, stream, process, inputQueue)
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
				return sendTerminalFailure(sender, fmt.Sprintf("failed to read terminal exit status: %v", resultErr))
			}
			if !waitForTerminalOutput(process, outputWriter, outputDone) {
				return nil
			}
			return sender.send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Exit{
				Exit: &cubebox.TerminalExit{Code: code},
			}})
		case err := <-receiveDone:
			if err == nil || errors.Is(err, errTerminalClientClosed) || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return sendTerminalFailure(sender, err.Error())
		case err := <-inputDone:
			inputDone = nil
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				return sendTerminalFailure(sender, err.Error())
			}
		case err := <-outputDone:
			outputDone = nil
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				return sendTerminalFailure(sender, fmt.Sprintf("failed to read terminal output: %v", err))
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func terminalCleanupContext(namespace string) (context.Context, context.CancelFunc) {
	ctx := namespaces.WithNamespace(context.Background(), namespace)
	return context.WithTimeout(ctx, terminalCleanupTimeout)
}

type terminalCleanupProcess interface {
	CloseIO(context.Context, ...containerd.IOCloserOpts) error
	Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error)
}

func cleanupTerminalProcess(
	logCtx context.Context,
	namespace string,
	execID string,
	process terminalCleanupProcess,
	pipes ...io.Closer,
) {
	closeTerminalPipes(pipes...)
	cleanupCtx, cancel := terminalCleanupContext(namespace)
	defer cancel()
	_ = process.CloseIO(cleanupCtx, containerd.WithStdinCloser)
	if _, err := process.Delete(cleanupCtx, containerd.WithProcessKill); err != nil {
		log.G(logCtx).WithError(err).Warnf("failed to delete terminal process %s", execID)
	}
}

func terminalEnv(env []string) []string {
	result := append([]string(nil), env...)
	present := make(map[string]bool, len(result))
	for _, value := range result {
		if key, _, ok := strings.Cut(value, "="); ok {
			present[key] = true
		}
	}
	for _, fallback := range []string{"TERM=xterm-256color", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"} {
		key, _, _ := strings.Cut(fallback, "=")
		if !present[key] {
			result = append(result, fallback)
		}
	}
	return result
}

func validateTerminalOpen(open *cubebox.TerminalOpen) error {
	if open == nil {
		return errors.New("the first terminal message must be open")
	}
	if strings.TrimSpace(open.SandboxId) == "" {
		return errors.New("sandbox_id is required")
	}
	if strings.TrimSpace(open.ContainerId) == "" {
		return errors.New("container_id is required")
	}
	if (open.Cols == 0) != (open.Rows == 0) {
		return errors.New("cols and rows must both be positive or both be omitted")
	}
	if open.Cols > maxTerminalDimension || open.Rows > maxTerminalDimension {
		return fmt.Errorf("terminal dimensions must not exceed %d", maxTerminalDimension)
	}
	if open.Cwd != "" && (!strings.HasPrefix(open.Cwd, "/") || strings.ContainsRune(open.Cwd, '\x00')) {
		return errors.New("terminal cwd must be an absolute path without NUL characters")
	}
	if len(open.Args) > 0 && strings.TrimSpace(open.Args[0]) == "" {
		return errors.New("terminal args must start with a command")
	}
	for _, env := range open.Env {
		key, _, ok := strings.Cut(env, "=")
		if !ok || key == "" || strings.ContainsRune(env, '\x00') {
			return errors.New("terminal environment entries must use non-empty KEY=VALUE form")
		}
	}

	total := len(open.RequestId) + len(open.SandboxId) + len(open.ContainerId) + len(open.Cwd)
	for _, arg := range open.Args {
		total += len(arg)
	}
	for _, env := range open.Env {
		total += len(env)
	}
	if total > maxTerminalOpenBytes {
		return fmt.Errorf("terminal open message exceeds %d bytes", maxTerminalOpenBytes)
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

func receiveTerminalCommands(
	ctx context.Context,
	stream terminalMessageStream,
	process terminalProcess,
	input chan<- []byte,
) error {
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		switch payload := message.Message.(type) {
		case *cubebox.TerminalMessage_Input:
			if len(payload.Input) == 0 {
				continue
			}
			if len(payload.Input) > maxTerminalInputMessageBytes {
				return fmt.Errorf("terminal input exceeds %d bytes", maxTerminalInputMessageBytes)
			}
			data := append([]byte(nil), payload.Input...)
			select {
			case input <- data:
			case <-ctx.Done():
				return ctx.Err()
			default:
				return errors.New("terminal input backlog exceeded")
			}
		case *cubebox.TerminalMessage_Resize:
			if payload.Resize == nil || payload.Resize.Cols == 0 || payload.Resize.Rows == 0 {
				return errors.New("terminal resize requires positive cols and rows")
			}
			if payload.Resize.Cols > maxTerminalDimension || payload.Resize.Rows > maxTerminalDimension {
				return fmt.Errorf("terminal dimensions must not exceed %d", maxTerminalDimension)
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

func writeTerminalInput(writer io.Writer, input <-chan []byte) error {
	for data := range input {
		if _, err := writer.Write(data); err != nil {
			return fmt.Errorf("failed to write terminal input: %w", err)
		}
	}
	return nil
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

func waitForTerminalOutput(process containerd.Process, outputWriter io.Closer, outputDone <-chan error) bool {
	ioWaitDone := make(chan struct{})
	go func() {
		process.IO().Wait()
		close(ioWaitDone)
	}()
	select {
	case <-ioWaitDone:
	case <-time.After(terminalOutputDrainTimeout):
	}
	_ = outputWriter.Close()
	if outputDone == nil {
		return true
	}
	select {
	case <-outputDone:
		return true
	case <-time.After(terminalOutputDrainTimeout):
		return false
	}
}

func sendTerminalFailure(sender *terminalStreamSender, message string) error {
	if err := sender.sendError(message); err != nil {
		return err
	}
	return nil
}

func closeTerminalPipes(pipes ...io.Closer) {
	for _, pipe := range pipes {
		_ = pipe.Close()
	}
}
