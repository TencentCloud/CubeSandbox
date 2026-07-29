// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
)

type terminalClientCloseRecorder struct {
	grpc.ClientStream

	mu           sync.Mutex
	events       []string
	closeSendErr error
}

func (f *terminalClientCloseRecorder) Send(*cubebox.TerminalClientFrame) error {
	return nil
}

func (f *terminalClientCloseRecorder) Recv() (*cubebox.TerminalServerFrame, error) {
	return nil, io.EOF
}

func (f *terminalClientCloseRecorder) CloseSend() error {
	f.record("close_send")
	return f.closeSendErr
}

func (f *terminalClientCloseRecorder) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *terminalClientCloseRecorder) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func TestTerminalStreamCloseOrdersHalfCloseBeforePoolRelease(t *testing.T) {
	closeSendErr := errors.New("close send failed")
	releaseErr := errors.New("release failed")
	recorder := &terminalClientCloseRecorder{closeSendErr: closeSendErr}
	stream := &TerminalStream{
		CubeboxMgr_TerminalClient: recorder,
		release: func() error {
			recorder.record("release")
			return releaseErr
		},
	}

	const callers = 16
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- stream.Close()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.ErrorIs(t, err, closeSendErr)
		require.ErrorIs(t, err, releaseErr)
	}
	require.Equal(t, []string{"close_send", "release"}, recorder.snapshot())
	require.ErrorIs(t, stream.CloseSend(), closeSendErr)
	require.Equal(t, []string{"close_send", "release"}, recorder.snapshot())
}
