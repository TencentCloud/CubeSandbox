// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telnet"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func (l *local) doProbe(ctx context.Context, c *cubebox.ContainerConfig, ci *cubeboxstore.Container) (retErr error) {
	startTime := time.Now()
	defer func() {
		workflow.RecordCreateMetric(ctx, retErr, constants.CubeProbeId, time.Since(startTime))
	}()

	telnetCh := make(chan error, 1)
	if c.GetProbe() != nil && c.GetProbe().GetProbeHandler() != nil {
		if ci.IP == "" || ci.IP == "<nil>" {
			return ret.Err(errorcode.ErrorCode_CreateNetworkFailed, "invalid NetworkInfo")
		}
		if c.GetProbe().TimeoutMs <= 0 {
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid probe TimeoutMs[%v]",
				c.GetProbe().TimeoutMs)
		}
		if c.GetProbe().PeriodMs <= 2 {
			c.Probe.PeriodMs = 2
		}
		if c.GetProbe().GetProbeTimeoutMs() <= 5 {
			c.Probe.ProbeTimeoutMs = 100
		}
		cfg := &telnet.ProbeConfig{
			Addr:             ci.IP,
			InitialDelay:     time.Duration(c.GetProbe().InitialDelayMs) * time.Millisecond,
			Timeout:          time.Duration(c.GetProbe().TimeoutMs) * time.Millisecond,
			Period:           time.Duration(c.GetProbe().PeriodMs) * time.Millisecond,
			SuccessThreshold: c.GetProbe().SuccessThreshold,
			FailureThreshold: c.GetProbe().FailureThreshold,
			InstanceType:     ci.InstanceType,
			ProbeTimeout:     time.Duration(c.GetProbe().GetProbeTimeoutMs()) * time.Millisecond,
		}

		if cfg.SuccessThreshold < 1 {
			cfg.SuccessThreshold = 1
		}
		if cfg.FailureThreshold < 1 {
			cfg.FailureThreshold = 1
		}

		handler := c.GetProbe().GetProbeHandler()
		if tcp := handler.GetTcpSocket(); tcp != nil {
			cfg.Action = telnet.ActionTCPSocket
			cfg.Port = tcp.GetPort()
			if cfg.Port <= 0 {
				return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid probe port[%v]", cfg.Port)
			}
		} else if ping := handler.GetPing(); ping != nil {
			cfg.Action = telnet.ActionPing
			cfg.PingUDP = ping.GetUdp()
		} else if httpGet := handler.GetHttpGet(); httpGet != nil {
			cfg.Action = telnet.ActionHTTPGet
			cfg.Port = httpGet.GetPort()
			req, err := NewRequestForHTTPGetAction(ctx, httpGet, cfg.Addr)
			if err != nil {
				return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid http probe[%d]:%v", cfg.Port, err)
			}
			cfg.HttpGetRequest = req
		} else {
			return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "invalid probe cfg")
		}

		log.G(ctx).Debugf("probe [%s] start:%s", ci.IP, utils.InterfaceToString(cfg))
		telnetCh = telnet.Telnet(ctx, cfg)
	} else {
		telnetCh <- nil
	}
	select {
	case telnetRet := <-telnetCh:
		if telnetRet != nil {
			log.G(ctx).Errorf("telnet [%s] failed. errno:%v, costtime:%+v",
				ci.IP, ret.FetchErrorCode(telnetRet), time.Since(startTime))
			return telnetRet
		} else {
			log.G(ctx).Debugf("telnet [%s] done,costtime:%+v",
				ci.IP, time.Since(startTime))
		}
	case taskRet := <-ci.ExitCh:
		if taskRet.Error() == nil {
			log.G(ctx).Errorf("telnet [%s] failed[container exit], costtime:%+v",
				ci.IP, time.Since(startTime))
			return ret.Err(errorcode.ErrorCode_PortBindingFailed, "Failed to initialize the container. "+
				"Please confirm that the container can be started locally.")
		} else {
			log.G(ctx).Errorf("probe [%s] failed[context canceled], costtime:%+v",
				ci.IP, time.Since(startTime))
			return ret.Errorf(errorcode.ErrorCode_PortBindingFailed,
				"Sandbox probe canceled after %v on %s. "+
					"The parent context was canceled, possibly due to a timeout. "+
					"Check sandbox networking (cube-dev, CIDR, routes) if this persists.",
				time.Since(startTime).Round(time.Second), ci.IP)
		}
	case <-ctx.Done():
		probeDesc := describeProbe(c.GetProbe())
		log.G(ctx).Errorf("probe [%s] timeout, costtime:%+v, err:%v, probe_config=%s",
			ci.IP, time.Since(startTime), ctx.Err(), probeDesc)
		return ret.Errorf(errorcode.ErrorCode_PortBindingFailed,
			"Sandbox probe timed out after %v on %s. %s "+
				"This may indicate a network connectivity issue between the host and sandbox. "+
				"Check that the cube-dev interface and CIDR configuration are consistent.",
			time.Since(startTime).Round(time.Second), ci.IP, probeDesc)
	}

	select {
	case taskRet := <-ci.ExitCh:
		if taskRet.Error() == nil {
			log.G(ctx).Errorf("telnet [%s] failed[container exit], costtime:%+v",
				ci.IP, time.Since(startTime))
			return ret.Err(errorcode.ErrorCode_PortBindingFailed, "Failed to initialize the container. "+
				"Please confirm that the container can be started locally.")
		} else {
			log.G(ctx).Errorf("probe [%s] failed[context canceled], costtime:%+v",
				ci.IP, time.Since(startTime))
			return ret.Errorf(errorcode.ErrorCode_PortBindingFailed,
				"Sandbox probe canceled after %v on %s. "+
					"Check sandbox networking (cube-dev, CIDR, routes) if this persists.",
				time.Since(startTime).Round(time.Second), ci.IP)
		}
	default:
	}
	return nil
}

func doPreStop(ctx context.Context, ci *cubeboxstore.Container) {
	c := ci.Config
	if c.GetPrestop() == nil || c.GetPrestop().GetLifecyleHandler() == nil {
		return
	}

	if c.GetPrestop().TerminationGracePeriodMs <= 0 {
		log.G(ctx).Errorf("%v", ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid TerminationGracePeriodMs[%v]",
			c.GetPrestop().TerminationGracePeriodMs))
		return
	}

	doStopHooks(ctx, ci, c.GetPrestop().GetLifecyleHandler(), constants.CubePrestopId,
		time.Duration(c.GetPrestop().TerminationGracePeriodMs)*time.Millisecond)
}

func doPostStop(ctx context.Context, ci *cubeboxstore.Container) {
	c := ci.Config
	if c.GetPoststop() == nil || c.GetPoststop().GetLifecyleHandler() == nil {
		return
	}
	if c.GetPoststop().TimeoutMs <= 0 {
		log.G(ctx).Errorf("%v", ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid TimeoutMs[%v]",
			c.GetPoststop().TimeoutMs))
		return
	}

	if ci.Status.Get().PostStop {
		log.G(ctx).Errorf("doPostStop %s already do", ci.ID)
		return
	}

	if err := doStopHooks(ctx, ci, c.GetPoststop().GetLifecyleHandler(), constants.CubePoststopId,
		time.Duration(c.GetPoststop().TimeoutMs)*time.Millisecond); err != nil {
		log.G(ctx).Errorf("doPostStop %s failed:%v", ci.ID, err)
		return
	}

	ci.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.PostStop = true
		return status, nil
	})
}

func doStopHooks(ctx context.Context, ci *cubeboxstore.Container, handler *cubebox.LifecycleHandler,
	action string, timeOut time.Duration) (retErr error) {
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("doStopHooks panic info:%s, stack:%s", panicError, string(debug.Stack()))
		retErr = ret.Err(errorcode.ErrorCode_InvalidParamFormat, "doStopHooks panic")
	})

	if handler.GetHttpGet() == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "invalid handler")
	}
	if ci.IP == "" || ci.IP == "<nil>" {
		log.G(ctx).Errorf("%v", ret.Err(errorcode.ErrorCode_InvalidParamFormat, "IP"))
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "IP")
	}

	startTime := time.Now()
	defer func() {
		workflow.RecordDestroyMetric(ctx, retErr, action, time.Since(startTime))
	}()

	innerCtx, cancel := context.WithTimeout(ctx, timeOut)
	defer cancel()
	req, err := NewRequestForHTTPGetAction(innerCtx, handler.GetHttpGet(), ci.IP)
	if err != nil {
		log.G(ctx).Errorf("invalid %s[%v]:%v", action, handler.GetHttpGet(), err)
		return err
	}
	switch action {
	case constants.CubePoststopId:
		req.Header.Set("container_id", ci.ID)
		if ci.Status.Get().Reason == "OOMKilled" {
			req.Header.Set("status", "OOM")
		} else {
			req.Header.Set("status", "EXIT")
		}
		req.Header.Set("exit_code", fmt.Sprintf("%d", ci.Status.Get().ExitCode))
	case constants.CubePrestopId:
		if v := constants.GetPreStopType(ctx); v != "" {
			req.Header.Set("prestop_type", v)
		}
	default:
	}
	log.G(ctx).Debugf("%s [%s] doStopHooks:%v", action, ci.IP, req)

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.G(ctx).Errorf("%s [%s:%d] timeout", action, ci.IP, handler.GetHttpGet().GetPort())
		} else {
			log.G(ctx).Errorf("%s [%s:%d] err:%v", action, ci.IP, handler.GetHttpGet().GetPort(), err)
		}
		return err
	}
	defer res.Body.Close()
	if res.Body != nil {
		io.Copy(io.Discard, res.Body)
	}

	if res.StatusCode != http.StatusOK {
		log.G(ctx).Errorf("%s [%s:%d] failed.costtime:%+v", action, ci.IP, handler.GetHttpGet().GetPort(), time.Since(startTime))
	}

	log.G(ctx).Debugf("%s [%s:%d] done,costtime:%+v", action, ci.IP, handler.GetHttpGet().GetPort(), time.Since(startTime))
	return nil
}

func NewRequestForHTTPGetAction(ctx context.Context, httpGet *cubebox.HTTPGetAction, addr string) (*http.Request, error) {
	u := formatURL("http", addr, int(httpGet.GetPort()), httpGet.GetPath())

	header := make(http.Header)
	for _, h := range httpGet.GetHttpHeaders() {
		header.Add(h.GetName(), h.GetValue())
	}
	return newProbeRequest(ctx, u, header, "probe")
}

func newProbeRequest(ctx context.Context, url *url.URL, headers http.Header, userAgentFragment string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url.String(), nil)
	if err != nil {
		return nil, err
	}

	if headers == nil {
		headers = http.Header{}
	}
	if _, ok := headers["User-Agent"]; !ok {

		headers.Set("User-Agent", userAgent(userAgentFragment))
	}
	if _, ok := headers["Accept"]; !ok {

		headers.Set("Accept", "*/*")
	} else if headers.Get("Accept") == "" {

		headers.Del("Accept")
	}
	req.Header = headers
	req.Host = headers.Get("Host")

	return req, nil
}

func userAgent(purpose string) string {
	v := version.Version
	return fmt.Sprintf("cubelet-%v/%v", purpose, v)
}

func formatURL(scheme string, host string, port int, path string) *url.URL {
	u, err := url.Parse(path)

	if err != nil {
		u = &url.URL{
			Path: path,
		}
	}
	u.Scheme = scheme
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	return u
}

// describeProbe returns a human-readable summary of the probe configuration
// for diagnostic logging.
func describeProbe(p *cubebox.Probe) string {
	if p == nil {
		return "no probe configured"
	}
	handler := p.GetProbeHandler()
	if handler == nil {
		return "no probe handler"
	}
	switch {
	case handler.GetTcpSocket() != nil:
		return fmt.Sprintf("tcp_socket:%d", handler.GetTcpSocket().GetPort())
	case handler.GetHttpGet() != nil:
		hg := handler.GetHttpGet()
		return fmt.Sprintf("http_get:%s:%d%s", hg.GetHost(), hg.GetPort(), hg.GetPath())
	default:
		return "unknown probe handler"
	}
}
