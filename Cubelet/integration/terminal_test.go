// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestTerminalPTYLifecycle(t *testing.T) {
	if !IsCube() {
		t.Skip("interactive terminal integration requires the cube runtime")
	}
	sandbox := createTerminalSandbox(t)
	sessionID := uuid.NewString()
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer streamCancel()
	stream, err := cubeClient.Terminal(streamCtx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(terminalIntegrationOpen(sandbox.id, sessionID, 80, 24)))

	opened, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, sessionID, opened.GetOpened().GetSessionId())
	collector := &terminalOutputCollector{}

	require.NoError(t, stream.Send(terminalIntegrationStdin("stty -echo; printf '\\033[31m__ANSI_RED__\\033[0m\\n'; printf '__TERM_%s__\\n' \"$TERM\"; stty size\n")))
	collector.recvUntil(t, stream, func(output string) bool {
		return strings.Contains(output, "\x1b[31m__ANSI_RED__\x1b[0m") &&
			strings.Contains(output, "__TERM_xterm-256color__") && strings.Contains(output, "24 80")
	})
	require.NoError(t, stream.Send(terminalIntegrationStdin("if test -t 0 && test -t 1; then printf '__TTY_OK__\\n'; else printf '__TTY_BAD__\\n'; fi\n")))
	collector.recvUntil(t, stream, func(output string) bool { return strings.Contains(output, "__TTY_OK__") })
	require.NotContains(t, collector.output.String(), "__TTY_BAD__")

	require.NoError(t, stream.Send(&cubebox.TerminalClientFrame{
		Frame: &cubebox.TerminalClientFrame_Resize{Resize: &cubebox.TerminalResize{Cols: 100, Rows: 40}},
	}))
	require.NoError(t, stream.Send(terminalIntegrationStdin("sleep 0.2; stty size; printf '__RESIZED__\\n'\n")))
	collector.recvUntil(t, stream, func(output string) bool {
		return strings.Contains(output, "40 100") && strings.Contains(output, "__RESIZED__")
	})

	require.NoError(t, stream.Send(terminalIntegrationStdin("printf '__SIGINT_ARMED__\\n'; sleep 30 && printf '__UNEXPECTED_SLEEP_COMPLETION__\\n'\n")))
	collector.recvUntil(t, stream, func(output string) bool {
		return strings.Contains(output, "__SIGINT_ARMED__")
	})
	require.NoError(t, stream.Send(&cubebox.TerminalClientFrame{
		Frame: &cubebox.TerminalClientFrame_Stdin{Stdin: []byte{0x03}},
	}))
	require.NoError(t, stream.Send(terminalIntegrationStdin("printf '__AFTER_SIGINT__\\n'\n")))
	collector.recvUntil(t, stream, func(output string) bool {
		return strings.Contains(output, "__AFTER_SIGINT__")
	})
	require.NotContains(t, collector.output.String(), "__UNEXPECTED_SLEEP_COMPLETION__")

	require.NoError(t, stream.Send(terminalIntegrationStdin("exit\n")))
	collector.recvUntilClosed(t, stream)
	require.NotNil(t, collector.exitCode)
	require.Equal(t, int32(0), *collector.exitCode)
	require.Equal(t, "RUNTIME_EXITED", collector.closeReason)

	assertTerminalExecDeleted(t, sandbox, "cubelet-term-"+sessionID[:12])
}

func TestTerminalDrainsBeforePauseAndReopensAfterResume(t *testing.T) {
	if !IsCube() {
		t.Skip("terminal pause integration requires the cube runtime")
	}
	sandbox := createTerminalSandbox(t)
	sessionID := uuid.NewString()
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer streamCancel()
	stream, err := cubeClient.Terminal(streamCtx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(terminalIntegrationOpen(sandbox.id, sessionID, 80, 24)))
	frame, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, frame.GetOpened())
	collector := &terminalOutputCollector{}
	require.NoError(t, stream.Send(terminalIntegrationStdin("stty -echo; printf '\\033[33m__PAUSE_STARTED__\\033[0m\\n'; sleep 0.3; printf '\\033[35m__PAUSE_DRAIN_TAIL__\\033[0m\\n'\n")))
	collector.recvUntil(t, stream, func(output string) bool {
		return strings.Contains(output, "\x1b[33m__PAUSE_STARTED__\x1b[0m")
	})

	pauseCtx, pauseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	pauseResponse, err := cubeClient.Update(pauseCtx, &cubebox.UpdateCubeSandboxRequest{
		RequestID: uuid.NewString(),
		SandboxID: sandbox.id,
		Annotations: map[string]string{
			constants.MasterAnnotationsUpdateAction: constants.UpdateActionPause,
		},
	})
	pauseCancel()
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, pauseResponse.GetRet().GetRetCode(), pauseResponse.GetRet().GetRetMsg())

	collector.recvUntilClosed(t, stream)
	require.Equal(t, "SANDBOX_TRANSITION", collector.closeReason)
	require.Contains(t, collector.output.String(), "\x1b[35m__PAUSE_DRAIN_TAIL__\x1b[0m")
	assertTerminalExecDeleted(t, sandbox, "cubelet-term-"+sessionID[:12])

	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	resumeResponse, err := cubeClient.Update(resumeCtx, &cubebox.UpdateCubeSandboxRequest{
		RequestID: uuid.NewString(),
		SandboxID: sandbox.id,
		Annotations: map[string]string{
			constants.MasterAnnotationsUpdateAction: constants.UpdateActionResume,
		},
	})
	resumeCancel()
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, resumeResponse.GetRet().GetRetCode(), resumeResponse.GetRet().GetRetMsg())

	reopenCtx, reopenCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer reopenCancel()
	reopened, err := cubeClient.Terminal(reopenCtx)
	require.NoError(t, err)
	reopenedSessionID := uuid.NewString()
	require.NoError(t, reopened.Send(terminalIntegrationOpen(sandbox.id, reopenedSessionID, 80, 24)))
	frame, err = reopened.Recv()
	require.NoError(t, err)
	require.NotNil(t, frame.GetOpened(), "successful resume must remove the terminal admission fence")
	reopenedCollector := &terminalOutputCollector{}
	require.NoError(t, reopened.Send(terminalIntegrationStdin("stty -echo; printf '\\033[36m__RESUME_IO_OK__\\033[0m\\n'\n")))
	reopenedCollector.recvUntil(t, reopened, func(output string) bool {
		return strings.Contains(output, "\x1b[36m__RESUME_IO_OK__\x1b[0m")
	})
	require.NoError(t, reopened.Send(&cubebox.TerminalClientFrame{
		Frame: &cubebox.TerminalClientFrame_Close{Close: &cubebox.TerminalClose{Reason: "USER_CLOSED"}},
	}))
	reopenedCollector.recvUntilClosed(t, reopened)
	require.Equal(t, "USER_CLOSED", reopenedCollector.closeReason)
	assertTerminalExecDeleted(t, sandbox, "cubelet-term-"+reopenedSessionID[:12])
}

type terminalSandbox struct {
	id          string
	namespace   string
	containerID string
}

func createTerminalSandbox(t *testing.T) terminalSandbox {
	t.Helper()
	request := SimpleCubeboxConfig()
	require.NotEmpty(t, request.GetContainers())
	request.Containers[0].Name = "terminal"
	request.Containers[0].Image.Image = "docker.io/library/busybox:latest"
	createCtx, createCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	response, err := cubeClient.Create(createCtx, request)
	createCancel()
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, response.GetRet().GetRetCode(), response.GetRet().GetRetMsg())
	require.NotEmpty(t, response.GetSandboxID())
	sandboxID := response.GetSandboxID()
	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	listResponse, err := cubeClient.List(listCtx, &cubebox.ListCubeSandboxRequest{Id: &sandboxID})
	listCancel()
	require.NoError(t, err)
	require.Len(t, listResponse.GetItems(), 1)
	item := listResponse.GetItems()[0]
	require.NotEmpty(t, item.GetContainers())
	namespace := item.GetNamespace()
	if namespace == "" {
		namespace = namespaces.Default
	}
	sandbox := terminalSandbox{id: sandboxID, namespace: namespace, containerID: item.GetContainers()[0].GetId()}
	require.NotEmpty(t, sandbox.containerID)
	t.Cleanup(func() {
		destroyCtx, destroyCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer destroyCancel()
		destroyResponse, destroyErr := cubeClient.Destroy(destroyCtx, &cubebox.DestroyCubeSandboxRequest{
			RequestID: uuid.NewString(),
			SandboxID: sandboxID,
		})
		require.NoError(t, destroyErr)
		require.Equal(t, errorcode.ErrorCode_Success, destroyResponse.GetRet().GetRetCode(), destroyResponse.GetRet().GetRetMsg())
	})
	return sandbox
}

func terminalIntegrationOpen(sandboxID, sessionID string, cols, rows uint32) *cubebox.TerminalClientFrame {
	return &cubebox.TerminalClientFrame{Frame: &cubebox.TerminalClientFrame_Open{Open: &cubebox.TerminalOpen{
		RequestId: uuid.NewString(),
		SandboxId: sandboxID,
		SessionId: sessionID,
		Cols:      cols,
		Rows:      rows,
	}}}
}

func terminalIntegrationStdin(data string) *cubebox.TerminalClientFrame {
	return &cubebox.TerminalClientFrame{Frame: &cubebox.TerminalClientFrame_Stdin{Stdin: []byte(data)}}
}

type terminalStream interface {
	Recv() (*cubebox.TerminalServerFrame, error)
}

type terminalOutputCollector struct {
	output      bytes.Buffer
	nextOffset  uint64
	exitCode    *int32
	closeReason string
}

func (c *terminalOutputCollector) recvUntil(t *testing.T, stream terminalStream, done func(string) bool) {
	t.Helper()
	for !done(c.output.String()) {
		frame, err := stream.Recv()
		require.NoError(t, err, "terminal output before condition: %q", c.output.String())
		c.consume(t, frame)
	}
}

func (c *terminalOutputCollector) recvUntilClosed(t *testing.T, stream terminalStream) {
	t.Helper()
	for c.closeReason == "" {
		frame, err := stream.Recv()
		require.NoError(t, err, "terminal output before close: %q", c.output.String())
		c.consume(t, frame)
	}
}

func (c *terminalOutputCollector) consume(t *testing.T, frame *cubebox.TerminalServerFrame) {
	t.Helper()
	if stdout := frame.GetStdout(); stdout != nil {
		require.Equal(t, c.nextOffset, stdout.GetOffset(), "stdout offsets must be contiguous")
		c.nextOffset += uint64(len(stdout.GetData()))
		_, _ = c.output.Write(stdout.GetData())
	}
	if terminalExit := frame.GetExit(); terminalExit != nil {
		code := terminalExit.GetExitCode()
		c.exitCode = &code
	}
	if terminalError := frame.GetError(); terminalError != nil {
		require.Failf(t, "terminal returned error", "code=%s output=%q", terminalError.GetCode(), c.output.String())
	}
	if terminalClose := frame.GetClose(); terminalClose != nil {
		c.closeReason = terminalClose.GetReason()
	}
}

func assertTerminalExecDeleted(t *testing.T, sandbox terminalSandbox, execID string) {
	t.Helper()
	client := NewDefaultContainerdClient(t)
	t.Cleanup(func() { _ = client.Close() })
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), sandbox.namespace), time.Second)
		defer cancel()
		container, loadErr := client.LoadContainer(ctx, sandbox.containerID)
		if loadErr != nil {
			return false
		}
		task, taskErr := container.Task(ctx, nil)
		if taskErr != nil {
			return false
		}
		_, processErr := task.LoadProcess(ctx, execID, nil)
		return errdefs.IsNotFound(processErr)
	}, 10*time.Second, 100*time.Millisecond, fmt.Sprintf("terminal exec %s was not deleted", execID))
}
