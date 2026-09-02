// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package taskio

import (
	"bytes"
	"io"
	"os"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	readBufSize     = 4096
	logChanBufSize  = 10000
	maxBytesPerLine = 8192
)

type LoggerWriter struct {
	buff LogBuffer
}

func (w *LoggerWriter) Write(data []byte) (int, error) {
	return w.buff.buf.Write(data)
}

type LogBuffer struct {
	line bytes.Buffer
	buf  bytes.Buffer
}

func (l *LogBuffer) Write(data []byte) [][]byte {
	l.buf.Write(data)

	var lines [][]byte
	for {
		line, err := l.buf.ReadBytes('\n')
		if err != nil {
			if len(line) > 0 {

				l.line.Write(line)

				if l.line.Len() > maxBytesPerLine {
					l.line.Truncate(maxBytesPerLine)
				}
			}
			break
		} else {
			if l.line.Len() != 0 {

				l.line.Write(line)
				lines = append(lines, l.line.Bytes())
				l.line.Reset()
			} else {
				lines = append(lines, line)
			}
		}
	}
	return lines
}

func Consume(filename string) (<-chan string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	ch := make(chan string, logChanBufSize)
	go consume(file, ch)
	return ch, nil
}

func consume(f *os.File, ch chan string) {
	defer f.Close()
	defer func() {
		if err := recover(); err != nil {
			CubeLog.Errorf("panic on consume logs: %+v, stack: %s", err, recov.DumpStacktrace(3, err))
		}
	}()

	var logBuf LogBuffer
	for {
		var buf [readBufSize]byte
		n, err := f.Read(buf[:])
		if err == nil {
			lines := logBuf.Write(buf[:n])
			for _, line := range lines {
				ch <- string(line)
			}
			continue
		}

		if err == io.EOF {
			CubeLog.Errorf("read EOF from %s, consume logs done", f.Name())
		} else {
			CubeLog.Errorf("read %s failed: %+v", f.Name(), err)
		}
		close(ch)
		return
	}
}
