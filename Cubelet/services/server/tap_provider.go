// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network"
	netproto "github.com/tencentcloud/CubeSandbox/Cubelet/network/proto"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type Req struct {
	Name      string `json:"name"`
	SandboxId string `json:"sandboxId"`
}

type tapProvider struct {
}

type errCode int

const (
	Success         errCode = 0
	ParseJsonFailed         = iota + 1000
	CannotFindDevice
	TapDoesNotMatchId
)

const (
	instanceMark       = ""
	TapAllTime         = "tap_all_time"
	TapChangeGoRoutine = "tap_change_goroutine"
	TapReadTime        = "tap_read_time"
	TapSearchFd        = "tap_search_fd"
	TapUnmarshal       = "tap_unmarshal"
	TapMapLoad         = "tap_map_load"
	TapWriteMsg        = "tap_write_msg"
)

func matchTapFd(buf []byte) (errCode errCode, retFile *os.File, du1, du2 time.Duration) {

	du1 = 0
	du2 = 0

	startTime := time.Now()

	var req Req
	err := json.Unmarshal(buf, &req)
	if err != nil {
		return ParseJsonFailed, nil, du1, du2
	}

	du1 = time.Since(startTime)
	startTime = time.Now()

	mvmNet, exist := network.Name2MvmNet.Load(req.Name)
	if !exist {
		return CannotFindDevice, nil, du1, du2
	}
	m := mvmNet.(*netproto.MvmNet)

	if m.ID != req.SandboxId {
		return TapDoesNotMatchId, nil, du1, du2
	}

	du2 = time.Since(startTime)

	return Success, m.Tap.File, du1, du2
}

func run(beginTime time.Time, conn *net.UnixConn) {
	defer conn.Close()

	d0 := time.Since(beginTime)
	startTime := time.Now()

	n := 0
	buf := make([]byte, 1024)
	readCount := 0
	for {
		err := conn.SetReadDeadline(time.Now().Add(time.Millisecond * 5))
		if err != nil {
			CubeLog.Error("SetReadDeadline error")
			return
		}
		n, err = conn.Read(buf)
		if err == nil {
			break
		} else {
			readCount++
			if readCount >= 200 {
				CubeLog.Error("No data was received in more than 1 second")
				return
			}
		}
	}

	d1 := time.Since(startTime)
	startTime = time.Now()

	errCode, retFile, du1, du2 := matchTapFd(buf[0:n])
	var b []byte
	successFlag := false
	switch errCode {
	case Success:
		b = []byte(`{"errCode":"0","errMsg":"Success"}`)
		successFlag = true
	case ParseJsonFailed:
		b = []byte(`{"errCode":"1001","errMsg":"Parse json failed"}`)
	case CannotFindDevice:
		b = []byte(`{"errCode":"1002","errMsg":"Device cannot be found by current name"}`)
	case TapDoesNotMatchId:
		b = []byte(`{"errCode":"1003","errMsg":"The device name does not match the sandboxID"}`)
	default:
		b = []byte(`{"errCode":"-1","errMsg":"Unknown"}`)
	}

	if !successFlag {
		CubeLog.Errorf("get fd error, req: %s, rsp: %s", string(buf[0:n]), string(b))
	}

	var fds []int
	if retFile != nil {
		fds = append(fds, int(retFile.Fd()))
	}

	d2 := time.Since(startTime)
	startTime = time.Now()

	data := syscall.UnixRights(fds...)

	writeCount := 0
	for {
		err := conn.SetWriteDeadline(time.Now().Add(time.Millisecond * 5))
		if err != nil {
			CubeLog.Errorf("SetReadDeadline error")
			return
		}

		_, _, err = conn.WriteMsgUnix(b, data, nil)
		if err == nil {
			break
		} else {
			CubeLog.Errorf("WriteMsgUnix error: %v", err)
			writeCount++
			if writeCount >= 200 {
				CubeLog.Error("No data was sent for more than 1 second")
				return
			}
		}
	}

	d3 := time.Since(startTime)
	all := time.Since(beginTime)

	if readCount > 0 || writeCount > 0 {
		CubeLog.Debugf("req: %s, all time: %v, changeGoroutine time: %v, read time：%v, "+
			"search FD: %v (Unmarshal:%v, mapLoad:%v), WriteMsg time: %v, readCount: %v, writeCount: %v",
			string(buf[0:n]), all, d0, d1, d2, du1, du2, d3, readCount, writeCount)
	} else {
		CubeLog.Debugf("req: %s, all time: %v, changeGoroutine time: %v, read time：%v, "+
			"search FD: %v (Unmarshal:%v, mapLoad:%v), WriteMsg time: %v",
			string(buf[0:n]), all, d0, d1, d2, du1, du2, d3)
	}
}

func (g *tapProvider) Serve(lis net.Listener) error {

	l, ok := lis.(*net.UnixListener)
	if !ok {
		return fmt.Errorf("resolve UnixListener failed")
	}

	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return fmt.Errorf("AcceptUnix failed")
		}

		beginTime := time.Now()
		go run(beginTime, conn)
	}
}
