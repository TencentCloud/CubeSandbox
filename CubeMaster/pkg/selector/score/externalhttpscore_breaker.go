// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

const (
	circuitStateClosed = iota
	circuitStateHalfOpen
	circuitStateOpen

	defaultCircuitFailureThreshold  = 5
	defaultCircuitOpenDuration      = 5 * time.Second
	defaultCircuitHalfOpenMaxProbes = 1
)

var errExternalHTTPScoreCircuitOpen = errors.New("external_http_score circuit is open")

type externalHTTPScoreBreaker struct {
	mu                  sync.Mutex
	target              string
	state               int
	consecutiveFailures int
	openedAt            time.Time
	halfOpenInFlight    int
	failureThreshold    int
	openDuration        time.Duration
	halfOpenMaxProbes   int
}

type noopExternalHTTPScoreBreaker struct{}

type externalHTTPScoreGate interface {
	allow() error
	recordSuccess()
	recordFailure()
}

var (
	externalHTTPScoreBreakersMu sync.Mutex
	externalHTTPScoreBreakers   = map[string]*externalHTTPScoreBreaker{}
)

func resetExternalHTTPScoreRuntime() {
	externalHTTPScoreBreakersMu.Lock()
	externalHTTPScoreBreakers = map[string]*externalHTTPScoreBreaker{}
	externalHTTPScoreBreakersMu.Unlock()
	resetExternalHTTPScoreCircuitMetrics()
}

// circuitTargetLabel returns a bounded Prometheus/circuit key: host[:port] only.
// Userinfo, path, and query are dropped so credentials never appear as labels.
func circuitTargetLabel(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "invalid"
	}
	return u.Host
}

func getExternalHTTPScoreBreaker(endpoint string, cfg *config.ExternalHTTPScoreCircuitBreaker) externalHTTPScoreGate {
	if cfg != nil && cfg.Disable {
		return noopExternalHTTPScoreBreaker{}
	}

	target := circuitTargetLabel(endpoint)
	externalHTTPScoreBreakersMu.Lock()
	defer externalHTTPScoreBreakersMu.Unlock()
	if b, ok := externalHTTPScoreBreakers[target]; ok {
		b.applyConfig(cfg)
		return b
	}
	b := newExternalHTTPScoreBreaker(target, cfg)
	externalHTTPScoreBreakers[target] = b
	return b
}

func newExternalHTTPScoreBreaker(target string, cfg *config.ExternalHTTPScoreCircuitBreaker) *externalHTTPScoreBreaker {
	b := &externalHTTPScoreBreaker{
		target:            target,
		state:             circuitStateClosed,
		failureThreshold:  defaultCircuitFailureThreshold,
		openDuration:      defaultCircuitOpenDuration,
		halfOpenMaxProbes: defaultCircuitHalfOpenMaxProbes,
	}
	b.applyConfigLocked(cfg)
	return b
}

func (b *externalHTTPScoreBreaker) applyConfig(cfg *config.ExternalHTTPScoreCircuitBreaker) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applyConfigLocked(cfg)
}

func (b *externalHTTPScoreBreaker) applyConfigLocked(cfg *config.ExternalHTTPScoreCircuitBreaker) {
	if cfg == nil {
		return
	}
	if cfg.FailureThreshold > 0 {
		b.failureThreshold = cfg.FailureThreshold
	}
	if cfg.OpenDuration > 0 {
		b.openDuration = cfg.OpenDuration
	}
	if cfg.HalfOpenMaxProbes > 0 {
		b.halfOpenMaxProbes = cfg.HalfOpenMaxProbes
	}
}

func (b *externalHTTPScoreBreaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case circuitStateOpen:
		if time.Since(b.openedAt) >= b.openDuration {
			b.state = circuitStateHalfOpen
			b.halfOpenInFlight = 1
			setExternalHTTPScoreCircuitState(b.target, circuitStateHalfOpen)
			return nil
		}
		return errExternalHTTPScoreCircuitOpen
	case circuitStateHalfOpen:
		if b.halfOpenInFlight >= b.halfOpenMaxProbes {
			return errExternalHTTPScoreCircuitOpen
		}
		b.halfOpenInFlight++
		return nil
	default:
		return nil
	}
}

func (b *externalHTTPScoreBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures = 0
	b.halfOpenInFlight = 0
	b.state = circuitStateClosed
	setExternalHTTPScoreCircuitState(b.target, circuitStateClosed)
}

func (b *externalHTTPScoreBreaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures++
	b.halfOpenInFlight = 0
	if b.state == circuitStateHalfOpen || b.consecutiveFailures >= b.failureThreshold {
		b.state = circuitStateOpen
		b.openedAt = time.Now()
		setExternalHTTPScoreCircuitState(b.target, circuitStateOpen)
	}
}

func (noopExternalHTTPScoreBreaker) allow() error   { return nil }
func (noopExternalHTTPScoreBreaker) recordSuccess() {}
func (noopExternalHTTPScoreBreaker) recordFailure() {}
