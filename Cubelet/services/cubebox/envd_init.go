// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

const (
	// createEnvVarsAnnotation is the contract with CubeAPI: it carries the
	// create-time env_vars as a JSON object string {"K":"V"}. CubeMaster forwards
	// it here automatically via the cube.master prefix passthrough.
	createEnvVarsAnnotation = "cube.master.sandbox.create_env_vars"

	// Retry budget for /init: envd may not be listening yet right after the
	// sandbox becomes ready, so we short-poll until it is up or we time out.
	envdInitMaxWait    = 15 * time.Second
	envdInitInterval   = 200 * time.Millisecond
	envdInitReqTimeout = 3 * time.Second
)

// envdServerPort is the HTTP port envd listens on inside the guest (E2B envd is
// fixed at 49983). It is a var so tests can point /init at a local stub server.
var envdServerPort = 49983

// envdInitBody is the envd POST /init request body (e2b-dev/infra). We only use
// envVars and timestamp here:
//   - envVars: stored into envd's global defaults.EnvVars and merged into child
//     process environments on Process/Start;
//   - timestamp: used by envd to accept only newer requests, and to correct the
//     guest clock as a side effect.
type envdInitBody struct {
	EnvVars   map[string]string `json:"envVars,omitempty"`
	Timestamp string            `json:"timestamp,omitempty"`
}

// syncCreateEnvToEnvd injects the create-time env_vars (carried via annotation)
// into the guest envd.
//
// It mirrors the E2B orchestrator initEnvd flow: the host side connects directly
// to the guest's ip:49983 and calls the native POST /init. envd stores these
// variables into its global defaults.EnvVars, so processes it later starts
// (commands.run / run_code) inherit them (precedence: global < per-command, where
// per-command can override). envd merges additively and never clears existing
// entries, so any env already present in the runtime is preserved.
//
// Nothing is written to rootfs/profile and nothing goes into the OCI container
// spec, so neither the image/template nor the template snapshot is polluted with
// instance-level secrets. Sandboxes without create env are a no-op.
func (l *local) syncCreateEnvToEnvd(ctx context.Context, sandBox *cubeboxstore.CubeBox,
	ci *cubeboxstore.Container) error {
	raw, ok := sandBox.Annotations[createEnvVarsAnnotation]
	if !ok || raw == "" {
		return nil
	}

	envVars := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &envVars); err != nil {
		return fmt.Errorf("parse create env_vars annotation failed: %w", err)
	}
	if len(envVars) == 0 {
		return nil
	}

	ip := ci.IP
	if ip == "" || ip == "<nil>" {
		ip = sandBox.IP
	}
	if ip == "" || ip == "<nil>" {
		return fmt.Errorf("sync create env to envd: empty sandbox IP")
	}

	addr := fmt.Sprintf("http://%s/init", net.JoinHostPort(ip, strconv.Itoa(envdServerPort)))
	body, err := json.Marshal(envdInitBody{
		EnvVars:   envVars,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("marshal envd init body failed: %w", err)
	}

	start := time.Now()
	deadline := start.Add(envdInitMaxWait)
	client := &http.Client{Timeout: envdInitReqTimeout}

	var (
		lastErr error
		attempt int
	)
	for {
		attempt++
		lastErr = postEnvdInit(ctx, client, addr, body)
		if lastErr == nil {
			log.G(ctx).Infof("envd /init ok: sandbox=%s ip=%s vars=%d attempts=%d cost=%v",
				sandBox.SandboxID, ip, len(envVars), attempt, time.Since(start))
			return nil
		}

		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
		case <-time.After(envdInitInterval):
		}
		if ctx.Err() != nil {
			break
		}
	}

	return fmt.Errorf("envd /init failed: sandbox=%s ip=%s attempts=%d cost=%v: %w",
		sandBox.SandboxID, ip, attempt, time.Since(start), lastErr)
}

// postEnvdInit sends one /init request; only 204/200 is treated as success.
func postEnvdInit(ctx context.Context, client *http.Client, addr string, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, envdInitReqTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, addr, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build envd init request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("envd /init unexpected status %d", resp.StatusCode)
	}
	return nil
}
