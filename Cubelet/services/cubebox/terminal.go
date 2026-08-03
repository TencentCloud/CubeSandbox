// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

const terminalEventOpened = "terminal_opened"

var terminalSendTimeout = 10 * time.Second

type terminalOpenedLogEvent struct {
	RequestID   string
	SessionID   string
	SandboxID   string
	ContainerID string
	Resume      bool
	ExecID      string
}

type terminalOpenRejectedLogEvent struct {
	RequestID string
	SessionID string
	SandboxID string
	ErrorKind string
}

var (
	terminalOpenedLogger = func(ctx context.Context, event terminalOpenedLogEvent) {
		log.G(ctx).WithFields(map[string]interface{}{
			"event":        terminalEventOpened,
			"request_id":   event.RequestID,
			"session_id":   event.SessionID,
			"sandbox_id":   event.SandboxID,
			"container_id": event.ContainerID,
			"resume":       event.Resume,
			"exec_id":      event.ExecID,
		}).Warn("terminal opened")
	}
	terminalOpenRejectedLogger = func(ctx context.Context, event terminalOpenRejectedLogEvent) {
		log.G(ctx).WithFields(map[string]interface{}{
			"request_id": event.RequestID,
			"session_id": event.SessionID,
			"sandbox_id": event.SandboxID,
			"error_kind": event.ErrorKind,
		}).Warn("terminal open rejected")
	}
)

func (s *service) Terminal(stream grpc.BidiStreamingServer[cubebox.TerminalClientFrame, cubebox.TerminalServerFrame]) (returnErr error) {
	var attachment *terminalcore.Attachment
	defer func() {
		if panicValue := recover(); panicValue != nil {
			if attachment != nil {
				attachment.Detach()
			}
			log.G(stream.Context()).Error("terminal handler panic")
			returnErr = status.Error(codes.Internal, "terminal internal error")
		}
	}()
	if s.terminal == nil {
		return status.Error(codes.Unavailable, "terminal service is unavailable")
	}

	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	openFrame, ok := first.GetFrame().(*cubebox.TerminalClientFrame_Open)
	if !ok || openFrame.Open == nil {
		return sendTerminalError(stream, terminalcore.CodeProtocolError)
	}
	open := openFrame.Open
	request := terminalcore.OpenRequest{
		RequestID:   open.GetRequestId(),
		SandboxID:   open.GetSandboxId(),
		ContainerID: open.GetContainerId(),
		SessionID:   open.GetSessionId(),
		Cols:        open.GetCols(),
		Rows:        open.GetRows(),
	}
	if resume := open.GetResume(); resume != nil {
		request.Resume = &terminalcore.ResumeRequest{
			SessionID:  resume.GetSessionId(),
			LastOffset: resume.GetLastOffset(),
		}
	}

	attachment, opened, err := s.terminal.Open(stream.Context(), request)
	if err != nil {
		terminalOpenRejectedLogger(stream.Context(), terminalOpenRejectedLogEvent{
			RequestID: request.RequestID,
			SessionID: request.SessionID,
			SandboxID: request.SandboxID,
			ErrorKind: terminalcore.CodeOf(err),
		})
		return sendTerminalError(stream, terminalcore.CodeOf(err))
	}
	if err := sendTerminalFrame(stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Opened{Opened: &cubebox.TerminalOpened{
			SessionId:       opened.SessionID,
			ReplayFrom:      opened.ReplayFrom,
			ReplayTruncated: opened.ReplayTruncated,
		}},
	}); err != nil {
		attachment.Detach()
		return nil
	}
	terminalOpenedLogger(stream.Context(), terminalOpenedLogEvent{
		RequestID:   request.RequestID,
		SessionID:   opened.SessionID,
		SandboxID:   opened.Target.SandboxID,
		ContainerID: opened.Target.ContainerID,
		Resume:      request.Resume != nil,
		ExecID:      opened.ExecID,
	})

	go s.receiveTerminalFrames(stream, attachment)
	for {
		event, err := attachment.Next(stream.Context())
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				attachment.Detach()
				return err
			}
			return nil
		}
		frame := terminalServerFrame(event)
		if frame == nil {
			continue
		}
		if err := sendTerminalFrame(stream, frame); err != nil {
			attachment.Detach()
			return nil
		}
		if event.Kind == terminalcore.EventClose {
			return nil
		}
	}
}

func (s *service) receiveTerminalFrames(
	stream grpc.BidiStreamingServer[cubebox.TerminalClientFrame, cubebox.TerminalServerFrame],
	attachment *terminalcore.Attachment,
) {
	// This runs on a separate goroutine from the handler's recover, so a panic
	// here would otherwise unwind into the gRPC transport and crash the whole
	// cubelet. Mirror the handler's containment: detach so the session stays
	// resumable, then return.
	defer func() {
		if panicValue := recover(); panicValue != nil {
			if attachment != nil {
				attachment.Detach()
			}
			log.G(stream.Context()).Error("terminal receive panic")
		}
	}()
	protocolError := func() {
		attachment.ProtocolError()
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			attachment.Detach()
			return
		}
		switch payload := frame.GetFrame().(type) {
		case *cubebox.TerminalClientFrame_Open:
			protocolError()
			return
		case *cubebox.TerminalClientFrame_Stdin:
			if len(payload.Stdin) > s.terminal.MaxFrameBytes() {
				protocolError()
				return
			}
			if err := attachment.SendStdin(payload.Stdin); err != nil {
				code := terminalcore.CodeOf(err)
				attachment.NotifyError(code)
				if code == terminalcore.CodeSlowProducer {
					continue
				}
				return
			}
		case *cubebox.TerminalClientFrame_Resize:
			if payload.Resize == nil {
				protocolError()
				return
			}
			if err := attachment.Resize(payload.Resize.GetCols(), payload.Resize.GetRows()); err != nil {
				attachment.NotifyError(terminalcore.CodeOf(err))
				_ = attachment.Close(terminalcore.CloseProtocolError)
				return
			}
		case *cubebox.TerminalClientFrame_Close:
			if payload.Close == nil {
				protocolError()
				return
			}
			_ = attachment.Close(payload.Close.GetReason())
			return
		default:
			protocolError()
			return
		}
	}
}

func sendTerminalError(
	stream grpc.BidiStreamingServer[cubebox.TerminalClientFrame, cubebox.TerminalServerFrame],
	code string,
) error {
	return sendTerminalFrame(stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Error{Error: &cubebox.TerminalError{Code: code}},
	})
}

func sendTerminalFrame(
	stream grpc.BidiStreamingServer[cubebox.TerminalClientFrame, cubebox.TerminalServerFrame],
	frame *cubebox.TerminalServerFrame,
) error {
	result := make(chan error, 1)
	// gRPC cancels the stream context when the handler returns. A timed-out
	// Send therefore remains the only writer and is released by transport
	// cancellation as Terminal returns; the handler never starts a later Send.
	go func() { result <- stream.Send(frame) }()
	timer := time.NewTimer(terminalSendTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-stream.Context().Done():
		return stream.Context().Err()
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func terminalServerFrame(event terminalcore.Event) *cubebox.TerminalServerFrame {
	switch event.Kind {
	case terminalcore.EventStdout:
		return &cubebox.TerminalServerFrame{Frame: &cubebox.TerminalServerFrame_Stdout{Stdout: &cubebox.TerminalStdout{
			Data: event.Data, Offset: event.Offset,
		}}}
	case terminalcore.EventExit:
		return &cubebox.TerminalServerFrame{Frame: &cubebox.TerminalServerFrame_Exit{Exit: &cubebox.TerminalExit{
			ExitCode: event.ExitCode,
		}}}
	case terminalcore.EventError:
		return &cubebox.TerminalServerFrame{Frame: &cubebox.TerminalServerFrame_Error{Error: &cubebox.TerminalError{
			Code: event.Code,
		}}}
	case terminalcore.EventClose:
		return &cubebox.TerminalServerFrame{Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{
			Reason: event.Reason,
		}}}
	default:
		return nil
	}
}
