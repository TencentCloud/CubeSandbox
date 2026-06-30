// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
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
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	envdInitPath = "/init"
	// Keep create-time envd init within a bounded sub-second budget so the
	// safeguard absorbs brief restore jitter without turning sandbox create
	// into an unbounded slow path.
	envdInitAttemptTimeout             = 150 * time.Millisecond
	envdInitMaxAttempts                = 3
	envdInitRetryDelay                 = 25 * time.Millisecond
	defaultEnvdInitPort                = 49983
	missingEnvdSupportAnnotationDetail = "template does not carry envd support annotation"
)

func newEnvdInitFailure(msg string, hasEnvdCapability bool, err error) error {
	if !hasEnvdCapability {
		return ret.Errorf(errorcode.ErrorCode_ExecCommandInSandboxFailed,
			"%s; %s: %v", msg, missingEnvdSupportAnnotationDetail, err)
	}
	return ret.Errorf(errorcode.ErrorCode_ExecCommandInSandboxFailed,
		"%s: %v", msg, err)
}

func (l *local) getEnvdInitPort() int {
	if l != nil && l.envdInitPort > 0 {
		return l.envdInitPort
	}
	return defaultEnvdInitPort
}

func (l *local) getEnvdHTTPClient() *http.Client {
	if l != nil && l.envdHTTPClient != nil {
		return l.envdHTTPClient
	}
	return newEnvdHTTPClient()
}

func newEnvdHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
}

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
			return ret.Errorf(errorcode.ErrorCode_PortBindingFailed, "The initialization timeout or"+
				" detecting %s failed.", ci.IP)
		}
	case <-ctx.Done():
		log.G(ctx).Errorf("probe [%s] timeout, costtime:%+v, err:%v",
			ci.IP, time.Since(startTime), ctx.Err())
		return ret.Errorf(errorcode.ErrorCode_PortBindingFailed, "The initialization timeout or"+
			" detecting %s port failed.", ci.IP)
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
			return ret.Errorf(errorcode.ErrorCode_PortBindingFailed, "The initialization timeout or"+
				" detecting %s failed.", ci.IP)
		}
	default:
	}
	return nil
}

func (l *local) doCreateTimeEnvdInit(ctx context.Context, req *cubebox.RunCubeSandboxRequest, sandBox *cubeboxstore.CubeBox) error {
	if req == nil || sandBox == nil || req.Annotations == nil {
		return nil
	}
	raw := strings.TrimSpace(req.Annotations[constants.MasterAnnotationCreateTimeEnvVars])
	if raw == "" {
		return nil
	}
	envVars := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &envVars); err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid create_time_env_vars annotation: %v", err)
	}
	if len(envVars) == 0 {
		return nil
	}
	if strings.TrimSpace(sandBox.IP) == "" {
		return ret.Err(errorcode.ErrorCode_CreateNetworkFailed, "sandbox IP is empty for create_time_env_vars init")
	}

	body, err := json.Marshal(struct {
		EnvVars map[string]string `json:"envVars"`
	}{
		EnvVars: envVars,
	})
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "marshal create_time_env_vars init request failed: %v", err)
	}

	port := l.getEnvdInitPort()
	hasEnvdCapability := strings.TrimSpace(req.Annotations[constants.MasterAnnotationComponentEnvdVersion]) != ""
	if !hasEnvdCapability {
		// Templates built before envd capability propagation do not carry the
		// annotation. Keep them backward-compatible by probing the default envd
		// init endpoint with the same bounded retry instead of rejecting upfront.
		log.G(ctx).WithFields(CubeLog.Fields{
			"sandboxID":    sandBox.ID,
			"templateID":   strings.TrimSpace(req.Annotations[constants.MasterAnnotationAppSnapshotTemplateID]),
			"envdInitPort": port,
		}).Warnf("missing envd support annotation; probing default envd init endpoint with bounded retry")
	}

	return l.doCreateTimeEnvdInitWithRetry(ctx, sandBox.IP, port, hasEnvdCapability, body)
}

func (l *local) doCreateTimeEnvdInitWithRetry(ctx context.Context, sandboxIP string, port int, hasEnvdCapability bool, body []byte) error {
	var lastErr error
	for attempt := 1; attempt <= envdInitMaxAttempts; attempt++ {
		innerCtx, cancel := context.WithTimeout(ctx, envdInitAttemptTimeout)
		retryable, err := l.doCreateTimeEnvdInitAttempt(innerCtx, sandboxIP, port, body)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt == envdInitMaxAttempts {
			return newEnvdInitFailure("create_time_env_vars init failed after bounded retry", hasEnvdCapability, err)
		}
		select {
		case <-time.After(envdInitRetryDelay):
		case <-ctx.Done():
			return newEnvdInitFailure("create_time_env_vars init canceled during bounded retry", hasEnvdCapability, lastErr)
		}
	}
	return newEnvdInitFailure("create_time_env_vars init failed after bounded retry", hasEnvdCapability, lastErr)
}

func (l *local) doCreateTimeEnvdInitAttempt(ctx context.Context, sandboxIP string, port int, body []byte) (bool, error) {
	reqURL := formatURL("http", sandboxIP, port, envdInitPath)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), bytes.NewReader(body))
	if err != nil {
		return false, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "build envd init request failed: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := l.getEnvdHTTPClient().Do(httpReq)
	if err != nil {
		return isRetryableEnvdInitTransportErr(err), ret.Errorf(errorcode.ErrorCode_ExecCommandInSandboxFailed, "envd init request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, ret.Errorf(errorcode.ErrorCode_ExecCommandInSandboxFailed, "read envd init response body failed: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return isRetryableEnvdInitStatusCode(resp.StatusCode), ret.Errorf(
			errorcode.ErrorCode_ExecCommandInSandboxFailed,
			"envd init request returned HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody)),
		)
	}
	return false, nil
}

func isRetryableEnvdInitStatusCode(statusCode int) bool {
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRetryableEnvdInitTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
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
