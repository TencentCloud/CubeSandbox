// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package telnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"


	"encoding/json"

	"github.com/tatsushid/go-fastping"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const (
	maxRespBodyLength = 8 * 1 << 10
)

var (
	defaultDialTimeOut = 100 * time.Millisecond
	defaultMaxTTL      = 5 * time.Millisecond
	transport          = &http.Transport{}
)

type ProbeConfig struct {
	Addr string

	Port int32

	InitialDelay time.Duration

	Timeout time.Duration

	Period time.Duration

	SuccessThreshold int32

	FailureThreshold int32

	ProbeTimeout time.Duration

	Action  int
	PingUDP bool

	HttpGetRequest *http.Request
	InstanceType   string
}

const (
	ActionTCPSocket = iota
	ActionPing
	ActionHTTPGet
)

func probe(ctx context.Context, p *ProbeConfig, retCh chan error) {
	defer utils.Recover()

	if p.InitialDelay != 0 {
		select {
		case <-time.After(p.InitialDelay):
		case <-ctx.Done():
			return
		}
	}

	select {
	case <-ctx.Done():
		return
	default:
	}

	succCnt := int32(0)
	failureCnt := int32(0)
	innerCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	var err error
	end := false
	for {
		select {
		case <-innerCtx.Done():
			err = innerCtx.Err()
			goto exitDetect
		case <-ctx.Done():
			return
		default:

			switch p.Action {
			case ActionPing:
				err = doPing(innerCtx, p.Addr, p.Period, p.PingUDP)
			case ActionTCPSocket:
				err = doTCPDetect(fmt.Sprintf("%s:%d", p.Addr, p.Port), p.ProbeTimeout)
			case ActionHTTPGet:
				err, end = doHTTPGet(p.HttpGetRequest, p.ProbeTimeout, p.InstanceType)
			default:
				retCh <- ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "unknown probe action: %v", p.Action)
				return
			}
			log.G(ctx).Tracef("probe result %v", err)

			if end {
				retCh <- wrapError(err, errorcode.ErrorCode_PortBindingFailed)
				return
			}

			if err == nil {
				succCnt += 1

				failureCnt = 0
			} else {
				succCnt = 0
				failureCnt += 1
			}
			if succCnt >= p.SuccessThreshold {
				retCh <- nil
				return
			}

			if failureCnt >= p.FailureThreshold {
				retCh <- wrapError(err, errorcode.ErrorCode_PortBindingFailed)
				return
			}
		}
		time.Sleep(p.Period)
	}
exitDetect:
	if succCnt >= p.SuccessThreshold {
		retCh <- nil
		return
	}

	if failureCnt >= p.FailureThreshold || innerCtx.Err() != nil {
		retCh <- wrapError(err, errorcode.ErrorCode_PortBindingFailed)
		return
	}
}

func wrapError(err error, code errorcode.ErrorCode) error {
	if err == nil {
		return nil
	}
	_, ok := err.(*ret.Error)
	if ok {
		return err
	}
	return ret.Errorf(code, err.Error())
}

func Telnet(ctx context.Context, p *ProbeConfig) chan error {
	retCh := make(chan error, 1)
	if p.ProbeTimeout == 0 {
		p.ProbeTimeout = defaultDialTimeOut
	}
	go probe(ctx, p, retCh)
	return retCh
}

func doTCPDetect(address string, dialTimeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return logError(err)
	}

	defer func() {
		_ = conn.Close()
	}()
	return nil
}

func logError(err error) error {
	netErr, ok := err.(net.Error)
	if !ok {
		return nil
	}

	opErr, ok := netErr.(*net.OpError)
	if !ok {
		return nil
	}

	return fmt.Errorf("network connectivity probe failed: %+v, timeout: %+v", opErr, netErr.Timeout())
}

func doPing(ctx context.Context, address string, dialTimeout time.Duration, udp bool) error {
	p := fastping.NewPinger()
	if udp {
		_, _ = p.Network("udp")
	}
	p.MaxRTT = defaultMaxTTL
	err := p.AddIP(address)
	if err != nil {
		return err
	}
	onRecv, timeout := make(chan struct{}), make(chan bool)
	p.OnRecv = func(addr *net.IPAddr, t time.Duration) {
		select {
		case onRecv <- struct{}{}:
		default:
		}
	}
	p.OnIdle = func() {
		select {
		case timeout <- true:
		default:
		}
	}
	go func() {
		defer p.Stop()
		err = p.Run()
		if err != nil {
			select {
			case timeout <- true:
			default:
			}
		}
	}()
	innerCtx, cancel := context.WithTimeout(ctx, defaultMaxTTL)
	defer cancel()
	select {
	case <-onRecv:
		return nil
	case <-timeout:
		return fmt.Errorf("ping timeout")
	case <-ctx.Done():
		return ctx.Err()
	case <-innerCtx.Done():
		return innerCtx.Err()
	}
}

type httpProbeRet struct {
	ErrorCode int    `json:"errcode"`
	ErrorMsg  string `json:"errmsg"`
}

func doHTTPGet(req *http.Request, timeout time.Duration, instanceType string) (err error, end bool) {
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	res, err := client.Do(req)
	if err != nil {
		return err, false
	}
	defer res.Body.Close()

	if res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusBadRequest {
		return nil, false
	}

	if instanceType == "cubebox" {
		_, _ = io.Copy(io.Discard, res.Body)
		return fmt.Errorf("statuscode:%d", res.StatusCode), false
	}

	b, err := utils.ReadAtMost(res.Body, maxRespBodyLength)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err), false
	}

	pRes := &httpProbeRet{}
	err = json.Unmarshal(b, pRes)
	if err != nil {
		pRes.ErrorCode = int(errorcode.ErrorCode_PortBindingFailed)
		pRes.ErrorMsg = string(b)
	}

	if res.StatusCode >= http.StatusInternalServerError {
		return ret.Errorf(errorcode.ErrorCode(pRes.ErrorCode), pRes.ErrorMsg), true
	}

	return ret.Errorf(errorcode.ErrorCode(pRes.ErrorCode), pRes.ErrorMsg), false
}
