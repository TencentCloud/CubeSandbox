// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminalcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

const outputDrainTimeout = time.Second

var errClientClosed = errors.New("terminal client closed")

type ExitStatus struct {
	Code int32
	Err  error
}

type Process interface {
	ID() string
	Output() io.Reader
	Wait() <-chan ExitStatus
	Start(context.Context) error
	Resize(context.Context, uint32, uint32) error
	WriteStdin([]byte) error
	Cleanup() error
}

type ProcessFactory interface {
	Create(context.Context, *cubebox.TerminalOpenRequest) (Process, error)
}

type Stream interface {
	Context() context.Context
	Recv() (*cubebox.TerminalClientMessage, error)
	Send(*cubebox.TerminalServerMessage) error
}

type lockedSender struct {
	stream Stream
	mu     sync.Mutex
}

func (s *lockedSender) send(frame *cubebox.TerminalServerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(frame)
}

func Run(stream Stream, factory ProcessFactory) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("terminal open frame is required: %w", err)
	}
	open := first.GetOpen()
	if open == nil {
		return errors.New("the first terminal frame must be open")
	}
	if err := ValidateOpen(open.GetSandboxId(), open.GetContainerId(), open.GetCols(), open.GetRows()); err != nil {
		return err
	}

	process, err := factory.Create(stream.Context(), open)
	if err != nil {
		return fmt.Errorf("create terminal process: %w", err)
	}
	defer process.Cleanup()

	if err := process.Start(stream.Context()); err != nil {
		return fmt.Errorf("start terminal process: %w", err)
	}
	if err := process.Resize(stream.Context(), open.GetCols(), open.GetRows()); err != nil {
		return fmt.Errorf("set initial terminal size: %w", err)
	}

	sender := &lockedSender{stream: stream}
	if err := sender.send(&cubebox.TerminalServerMessage{
		Payload: &cubebox.TerminalServerMessage_Ready{Ready: &cubebox.TerminalReady{ExecId: process.ID()}},
	}); err != nil {
		return err
	}

	outputDone := make(chan error, 1)
	go func() { outputDone <- copyOutput(process.Output(), sender) }()
	recvDone := make(chan error, 1)
	go func() { recvDone <- receiveInput(stream, process) }()

	for {
		select {
		case status := <-process.Wait():
			if status.Err != nil {
				return fmt.Errorf("terminal process exit: %w", status.Err)
			}
			select {
			case outputErr := <-outputDone:
				if outputErr != nil && !errors.Is(outputErr, io.EOF) && !errors.Is(outputErr, io.ErrClosedPipe) {
					return outputErr
				}
			case <-time.After(outputDrainTimeout):
			}
			return sender.send(&cubebox.TerminalServerMessage{
				Payload: &cubebox.TerminalServerMessage_Exit{Exit: &cubebox.TerminalExit{
					ExitCode: status.Code,
					Reason:   "process_exited",
				}},
			})
		case recvErr := <-recvDone:
			if errors.Is(recvErr, errClientClosed) || errors.Is(recvErr, io.EOF) || errors.Is(recvErr, context.Canceled) {
				return nil
			}
			return recvErr
		case outputErr := <-outputDone:
			if outputErr != nil && !errors.Is(outputErr, io.EOF) && !errors.Is(outputErr, io.ErrClosedPipe) {
				return outputErr
			}
			outputDone = nil
		case <-stream.Context().Done():
			return nil
		}
	}
}

func receiveInput(stream Stream, process Process) error {
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		switch payload := frame.GetPayload().(type) {
		case *cubebox.TerminalClientMessage_Stdin:
			if len(payload.Stdin) > 0 {
				if err := process.WriteStdin(payload.Stdin); err != nil {
					return fmt.Errorf("write terminal input: %w", err)
				}
			}
		case *cubebox.TerminalClientMessage_Resize:
			if payload.Resize == nil {
				return errors.New("terminal resize payload is required")
			}
			if err := ValidateSize(payload.Resize.GetCols(), payload.Resize.GetRows()); err != nil {
				return err
			}
			if err := process.Resize(stream.Context(), payload.Resize.GetCols(), payload.Resize.GetRows()); err != nil {
				return fmt.Errorf("resize terminal: %w", err)
			}
		case *cubebox.TerminalClientMessage_Close:
			return errClientClosed
		default:
			return errors.New("only stdin, resize, or close is allowed after terminal open")
		}
	}
}

func copyOutput(reader io.Reader, sender *lockedSender) error {
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			output := append([]byte(nil), buffer[:n]...)
			if sendErr := sender.send(&cubebox.TerminalServerMessage{
				Payload: &cubebox.TerminalServerMessage_Output{Output: output},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			return err
		}
	}
}
