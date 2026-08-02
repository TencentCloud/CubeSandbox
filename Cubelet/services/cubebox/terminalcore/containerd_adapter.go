// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"context"
	"errors"
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
	"github.com/opencontainers/runtime-spec/specs-go"
)

type ContainerdTarget struct {
	Meta      TargetMetadata
	Container containerd.Container
}

func (t *ContainerdTarget) Metadata() TargetMetadata { return t.Meta }

type ContainerResolver func(context.Context, string, string) (*ContainerdTarget, error)

type ContainerdAdapter struct {
	client  *containerd.Client
	resolve ContainerResolver
	fifoDir string
}

type shellStartError struct{ err error }

func (e *shellStartError) Error() string { return e.err.Error() }
func (e *shellStartError) Unwrap() error { return e.err }

const containerdSetupCleanupTimeout = 10 * time.Second

func NewContainerdAdapter(client *containerd.Client, resolver ContainerResolver, fifoDir string) (*ContainerdAdapter, error) {
	if client == nil {
		return nil, errors.New("terminal containerd client is nil")
	}
	if resolver == nil {
		return nil, errors.New("terminal container resolver is nil")
	}
	if fifoDir == "" {
		return nil, errors.New("terminal fifo directory is empty")
	}
	if err := os.MkdirAll(fifoDir, 0o700); err != nil {
		return nil, fmt.Errorf("create terminal fifo directory: %w", err)
	}
	return &ContainerdAdapter{client: client, resolve: resolver, fifoDir: fifoDir}, nil
}

func (a *ContainerdAdapter) Resolve(ctx context.Context, sandboxID, containerID string) (Target, error) {
	target, err := a.resolve(ctx, sandboxID, containerID)
	if err != nil {
		return nil, err
	}
	if target == nil || target.Container == nil {
		return nil, Errorf(CodeTargetNotFound, "target container is unavailable")
	}
	return target, nil
}

func (a *ContainerdAdapter) StartPTY(ctx context.Context, target Target, spec PTYSpec) (PTYProcess, error) {
	containerTarget, ok := target.(*ContainerdTarget)
	if !ok || containerTarget.Container == nil {
		return nil, Errorf(CodeInternal, "terminal target has an unexpected runtime type")
	}
	metadata := containerTarget.Metadata()
	namespace := namespaceOrDefault(metadata.Namespace)
	execCtx := namespaces.WithNamespace(ctx, namespace)
	task, err := containerTarget.Container.Task(execCtx, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, WrapError(CodeTargetNotRunning, err)
		}
		return nil, WrapError(CodeInternal, err)
	}
	status, err := task.Status(execCtx)
	if err != nil {
		return nil, WrapError(CodeTargetNotRunning, err)
	}
	if status.Status != containerd.Running {
		return nil, Errorf(CodeTargetNotRunning, "target task is %s", status.Status)
	}

	baseSpec, err := containerTarget.Container.Spec(execCtx)
	if err != nil {
		return nil, WrapError(CodeInternal, err)
	}
	if baseSpec.Process == nil {
		return nil, Errorf(CodeInternal, "target container has no process spec")
	}

	shells := [][]string{{"/bin/bash", "-il"}, {"/bin/sh", "-il"}}
	var notFoundErrors []error
	for _, shell := range shells {
		process, startErr := a.startShell(execCtx, task, baseSpec.Process, spec, namespace, shell)
		if startErr == nil {
			return process, nil
		}
		var executableErr *shellStartError
		if !errors.As(startErr, &executableErr) || !isExecutableNotFound(executableErr.err, shell[0]) {
			return nil, WrapError(CodeInternal, startErr)
		}
		notFoundErrors = append(notFoundErrors, executableErr.err)
	}
	return nil, WrapError(CodeShellNotFound, errors.Join(notFoundErrors...))
}

func (a *ContainerdAdapter) startShell(
	ctx context.Context,
	task containerd.Task,
	base *specs.Process,
	spec PTYSpec,
	namespace string,
	args []string,
) (PTYProcess, error) {
	processSpec := buildTerminalProcessSpec(base, args)
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	ioCreator := cio.NewCreator(
		cio.WithStreams(stdinReader, stdoutWriter, nil),
		cio.WithTerminal,
		cio.WithFIFODir(a.fifoDir),
	)
	process, err := task.Exec(ctx, spec.ExecID, processSpec, ioCreator)
	if err != nil {
		closeTerminalPipes(stdinReader, stdinWriter, stdoutReader, stdoutWriter)
		return nil, err
	}

	waitCtx, waitCancel := context.WithCancel(context.WithoutCancel(ctx))
	statusCh, err := process.Wait(waitCtx)
	if err != nil {
		waitCancel()
		closeTerminalPipes(stdinReader, stdinWriter, stdoutReader, stdoutWriter)
		_, _ = process.Delete(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := process.Start(ctx); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), containerdSetupCleanupTimeout)
		cleanupErr := cleanupUnstartedProcess(cleanupCtx, process, waitCancel, stdinReader, stdinWriter, stdoutReader, stdoutWriter)
		cleanupCancel()
		if cleanupErr != nil {
			return nil, fmt.Errorf("start shell %s: %v; delete failed exec: %w", args[0], err, cleanupErr)
		}
		return nil, &shellStartError{err: err}
	}
	if err := process.Resize(ctx, spec.Cols, spec.Rows); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), containerdSetupCleanupTimeout)
		cleanupStartedProcess(cleanupCtx, process, statusCh, waitCancel, stdinReader, stdinWriter, stdoutReader, stdoutWriter)
		cleanupCancel()
		return nil, fmt.Errorf("set initial terminal size: %w", err)
	}

	p := &containerdPTYProcess{
		process:    process,
		namespace:  namespace,
		stdin:      stdinWriter,
		stdout:     stdoutReader,
		waitCancel: waitCancel,
		exitCh:     make(chan ExitStatus, 1),
		closeResources: func() {
			closeTerminalPipes(stdinReader, stdinWriter, stdoutReader, stdoutWriter)
		},
	}
	go p.forwardExit(statusCh)
	go func() {
		if process.IO() != nil {
			process.IO().Wait()
		}
		_ = stdoutWriter.Close()
	}()
	return p, nil
}

func (a *ContainerdAdapter) CleanupOrphan(ctx context.Context, record JournalRecord) error {
	metadata := record.Target
	cleanupCtx := namespaces.WithNamespace(ctx, namespaceOrDefault(metadata.Namespace))
	container, err := a.client.LoadContainer(cleanupCtx, metadata.RuntimeContainerID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	task, err := container.Task(cleanupCtx, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	process, err := task.LoadProcess(cleanupCtx, record.ExecID,
		cio.NewAttach(cio.WithStreams(nil, io.Discard, nil)))
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	statusCh, err := process.Wait(cleanupCtx)
	if err != nil {
		return fmt.Errorf("wait for orphan terminal exec %s: %w", record.ExecID, err)
	}
	if err := process.Kill(cleanupCtx, syscall.SIGKILL); err != nil && !isProcessStopped(err) {
		return err
	}
	select {
	case <-statusCh:
	case <-cleanupCtx.Done():
		return cleanupCtx.Err()
	}
	if _, err := process.Delete(cleanupCtx); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func buildTerminalProcessSpec(base *specs.Process, args []string) *specs.Process {
	copySpec := *base
	copySpec.Args = append([]string(nil), args...)
	copySpec.Env = withEnvironment(base.Env, "TERM", "xterm-256color")
	copySpec.CommandLine = ""
	copySpec.Terminal = true
	return &copySpec
}

func withEnvironment(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, item)
	}
	if !replaced {
		result = append(result, prefix+value)
	}
	return result
}

type containerdPTYProcess struct {
	process   containerd.Process
	namespace string

	stdin          io.WriteCloser
	stdout         io.Reader
	closeResources func()
	waitCancel     context.CancelFunc
	exitCh         chan ExitStatus

	stdinCloseOnce sync.Once
	deleteMu       sync.Mutex
	deleted        bool
}

func (p *containerdPTYProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *containerdPTYProcess) Stdout() io.Reader     { return p.stdout }

func (p *containerdPTYProcess) Resize(ctx context.Context, cols, rows uint32) error {
	return p.process.Resize(p.namespacedContext(ctx), cols, rows)
}

func (p *containerdPTYProcess) CloseStdin(ctx context.Context) error {
	var pipeErr error
	p.stdinCloseOnce.Do(func() {
		pipeErr = p.stdin.Close()
	})
	runtimeErr := p.process.CloseIO(p.namespacedContext(ctx), containerd.WithStdinCloser)
	if isProcessStopped(runtimeErr) {
		runtimeErr = nil
	}
	return errors.Join(pipeErr, runtimeErr)
}

func (p *containerdPTYProcess) Exited() <-chan ExitStatus { return p.exitCh }

func (p *containerdPTYProcess) Kill(ctx context.Context) error {
	err := p.process.Kill(p.namespacedContext(ctx), syscall.SIGKILL)
	if isProcessStopped(err) {
		return nil
	}
	return err
}

func (p *containerdPTYProcess) Delete(ctx context.Context) error {
	p.deleteMu.Lock()
	defer p.deleteMu.Unlock()
	if p.deleted {
		return nil
	}
	defer func() {
		// Release the containerd Wait registration and the local pipes even when
		// Delete fails: the session cleanup kills and reaps the process before
		// Delete runs, so these resources must not leak on the error path.
		p.waitCancel()
		if p.closeResources != nil {
			p.closeResources()
		}
	}()
	_, err := p.process.Delete(p.namespacedContext(ctx))
	if err != nil && !isNotFound(err) {
		return err
	}
	p.deleted = true
	return nil
}

func (p *containerdPTYProcess) namespacedContext(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, p.namespace)
}

func (p *containerdPTYProcess) forwardExit(statusCh <-chan containerd.ExitStatus) {
	defer close(p.exitCh)
	status, ok := <-statusCh
	if !ok {
		p.exitCh <- ExitStatus{Code: int32(containerd.UnknownExitStatus), Err: errors.New("containerd exit channel closed")}
		return
	}
	code, _, err := status.Result()
	p.exitCh <- ExitStatus{Code: int32(code), Err: err}
}

func cleanupUnstartedProcess(
	ctx context.Context,
	process containerd.Process,
	waitCancel context.CancelFunc,
	stdinReader *io.PipeReader,
	stdinWriter *io.PipeWriter,
	stdoutReader *io.PipeReader,
	stdoutWriter *io.PipeWriter,
) error {
	closeTerminalPipes(stdinReader, stdinWriter, stdoutReader, stdoutWriter)
	_, err := process.Delete(ctx)
	waitCancel()
	if isNotFound(err) {
		return nil
	}
	return err
}

func cleanupStartedProcess(
	ctx context.Context,
	process containerd.Process,
	statusCh <-chan containerd.ExitStatus,
	waitCancel context.CancelFunc,
	stdinReader *io.PipeReader,
	stdinWriter *io.PipeWriter,
	stdoutReader *io.PipeReader,
	stdoutWriter *io.PipeWriter,
) {
	_ = stdinWriter.Close()
	_ = process.CloseIO(ctx, containerd.WithStdinCloser)
	_ = process.Kill(ctx, syscall.SIGKILL)
	select {
	case <-statusCh:
	case <-ctx.Done():
	}
	_, _ = process.Delete(ctx)
	waitCancel()
	closeTerminalPipes(stdinReader, stdinWriter, stdoutReader, stdoutWriter)
}

func closeTerminalPipes(
	stdinReader *io.PipeReader,
	stdinWriter *io.PipeWriter,
	stdoutReader *io.PipeReader,
	stdoutWriter *io.PipeWriter,
) {
	_ = stdinReader.Close()
	_ = stdinWriter.Close()
	_ = stdoutReader.Close()
	_ = stdoutWriter.Close()
}

func namespaceOrDefault(namespace string) string {
	if namespace == "" {
		return namespaces.Default
	}
	return namespace
}

func isExecutableNotFound(err error, executable string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return true
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, strings.ToLower(executable)) {
		return false
	}
	return strings.Contains(message, "no such file or directory") ||
		strings.Contains(message, "executable file not found") ||
		strings.Contains(message, "executable not found")
}

func isNotFound(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || errdefs.IsNotFound(err))
}

func isProcessStopped(err error) bool {
	return err == nil || isNotFound(err) || errdefs.IsFailedPrecondition(err)
}
