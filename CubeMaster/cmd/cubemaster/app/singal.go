// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/server"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"golang.org/x/sys/unix"
)

var handledSignals = []os.Signal{
	unix.SIGTERM,
	unix.SIGINT,
	unix.SIGUSR1,
	unix.SIGPIPE,
}

func handleSignals(ctx context.Context, signals chan os.Signal, serverC chan *server.Server,
	cancelOutside func()) chan struct{} {
	done := make(chan struct{}, 1)
	go func() {
		var server *server.Server
		for {
			select {
			case s := <-serverC:
				server = s
			case s := <-signals:

				if s == unix.SIGPIPE {
					continue
				}

				CubeLog.WithContext(ctx).Errorf("received signal:%v", s.String())
				switch s {
				case unix.SIGUSR1:
					dumpStacks(true)
				default:
					cancelOutside()
					if server != nil {
						server.Stop()
					}
					graceFullStop()
					close(done)
					return
				}
			}
		}
	}()
	return done
}

func dumpStacks(writeToFile bool) {
	var (
		buf       []byte
		stackSize int
	)
	bufferLen := 16384
	for stackSize == len(buf) {
		buf = make([]byte, bufferLen)
		stackSize = runtime.Stack(buf, true)
		bufferLen *= 2
	}
	buf = buf[:stackSize]

	if writeToFile {

		name := filepath.Join(fmt.Sprintf("dump.%d.stacks.log.%d", os.Getpid(), time.Now().UnixNano()))
		f, err := os.Create(name)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(buf)

	}
}
