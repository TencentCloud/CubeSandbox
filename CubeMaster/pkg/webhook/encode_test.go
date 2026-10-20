// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func decodeBody(t *testing.T, e *Event) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(e.Body(), &body))
	return body
}

func ptr[T any](v T) *T { return &v }

func TestEncodeSixEvents(t *testing.T) {
	for _, tc := range []struct {
		typ      string
		protocol ProtocolEventData
		wantData string
	}{
		{TypeCreated, ProtocolEventData{}, ""},
		{TypeCheckpointed, ProtocolEventData{}, ""},
		{TypeResumed, ProtocolEventData{ResumeMode: "restore"}, `"event_data":{"resume_mode":"restore"},`},
		{TypePaused, ProtocolEventData{PauseMode: "full"}, `"event_data":{"pause_mode":"full","execution":{"started_at":""}},`},
		{TypeUpdated, ProtocolEventData{SetTimeout: ptr("2026-10-14T21:00:00.123+08:00")}, `"event_data":{"set_timeout":"2026-10-14T13:00:00Z"},`},
		{TypeKilled, ProtocolEventData{}, `"event_data":{"kill_reason":"unknown","execution":{"started_at":""}},`},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			c := captured(t, tc.typ)
			e, err := Encode(c, &tc.protocol)
			require.NoError(t, err)
			require.JSONEq(t, `{`+tc.wantData+`
				"id":"d12727fc-ab97-43d8-80cf-556cc1215017",
				"version":"v2","type":"`+tc.typ+`","timestamp":"2026-10-14T12:00:00Z",
				"event_category":"","event_label":"","sandbox_id":"sb-1",
				"sandbox_execution_id":"","sandbox_template_id":"tpl-1",
				"sandbox_build_id":"","sandbox_team_id":""}`, string(e.Body()))
			again, err := Encode(c, &tc.protocol)
			require.NoError(t, err)
			require.Equal(t, e.Body(), again.Body())
			require.Equal(t, c.input.ID, e.ID())
			require.Equal(t, tc.typ, e.Type())
			require.Equal(t, c.input.OccurredAt, e.OccurredAt())
		})
	}
}

func TestMapAndRequiredProtocolData(t *testing.T) {
	for _, typ := range eventTypes {
		t.Run(typ, func(t *testing.T) {
			c := captured(t, typ)
			e, err := Encode(c, nil)
			require.ErrorIs(t, err, ErrInvalidEvent)
			require.Nil(t, e)
			p, err := MapToProtocol(c)
			require.NoError(t, err)
			require.Equal(t, &ProtocolEventData{}, p)
			e, err = Encode(c, p)
			if typ == TypePaused || typ == TypeResumed || typ == TypeUpdated {
				require.ErrorIs(t, err, ErrInvalidEvent)
				require.Nil(t, e)
			} else {
				require.NoError(t, err)
				require.NotNil(t, e)
			}
		})
	}
	for _, c := range []*CapturedEvent{nil, {}} {
		p, err := MapToProtocol(c)
		require.ErrorIs(t, err, ErrInvalidEvent)
		require.Nil(t, p)
		e, err := Encode(c, &ProtocolEventData{})
		require.ErrorIs(t, err, ErrInvalidEvent)
		require.Nil(t, e)
	}
	for _, origin := range []Origin{OriginResumeTimeoutRefresh, OriginCreateFailover, OriginPauseTeardown, "", "other"} {
		c := captured(t, TypeCreated)
		// Exercise the second boundary using same-package access, not a public setter.
		c.input.Origin = origin
		p, err := MapToProtocol(c)
		require.ErrorIs(t, err, ErrSuppressed)
		require.Nil(t, p)
		e, err := Encode(c, &ProtocolEventData{})
		require.ErrorIs(t, err, ErrSuppressed)
		require.Nil(t, e)
	}
}

func TestEncodeRejectsFieldsOnWrongType(t *testing.T) {
	for _, field := range []struct {
		name    string
		p       ProtocolEventData
		allowed []string
	}{
		{"execution", ProtocolEventData{Execution: &ExecutionData{}}, []string{TypePaused, TypeKilled}},
		{"pause", ProtocolEventData{PauseMode: "full"}, []string{TypePaused}},
		{"resume", ProtocolEventData{ResumeMode: "restore"}, []string{TypeResumed}},
		{"reason", ProtocolEventData{KillReason: "request"}, []string{TypeKilled}},
		{"timeout", ProtocolEventData{SetTimeout: ptr("2026-10-14T12:00:00Z")}, []string{TypeUpdated}},
		{"network", ProtocolEventData{NetworkEgress: &NetworkEgressData{}}, []string{TypeUpdated}},
	} {
		for _, typ := range eventTypes {
			allowed := false
			for _, a := range field.allowed {
				allowed = allowed || typ == a
			}
			if allowed {
				continue
			}
			t.Run(field.name+"/"+typ, func(t *testing.T) {
				p := field.p
				// Supply the type's required data so the wrong field is the failure.
				switch typ {
				case TypePaused:
					p.PauseMode = "full"
				case TypeResumed:
					p.ResumeMode = "restore"
				case TypeUpdated:
					p.NetworkEgress = &NetworkEgressData{}
				}
				e, err := Encode(captured(t, typ), &p)
				require.ErrorIs(t, err, ErrInvalidEvent)
				require.Nil(t, e)
			})
		}
	}
}

func TestEncodeProtocolValues(t *testing.T) {
	for _, tc := range []struct {
		name, typ string
		p         ProtocolEventData
	}{
		{"incremental", TypePaused, ProtocolEventData{PauseMode: "incremental"}},
		{"soft dirty", TypePaused, ProtocolEventData{PauseMode: "soft-dirty"}},
		{"unknown resume", TypeResumed, ProtocolEventData{ResumeMode: "full"}},
		{"empty timeout", TypeUpdated, ProtocolEventData{SetTimeout: ptr("")}},
		{"invalid timeout with network", TypeUpdated, ProtocolEventData{SetTimeout: ptr("tomorrow"), NetworkEgress: &NetworkEgressData{}}},
		{"invalid start", TypeKilled, ProtocolEventData{Execution: &ExecutionData{StartedAt: "yesterday"}}},
		{"negative cpu", TypeKilled, ProtocolEventData{Execution: &ExecutionData{VCPUCount: ptr(int64(-1))}}},
		{"negative memory", TypePaused, ProtocolEventData{PauseMode: "full", Execution: &ExecutionData{MemoryMB: ptr(int64(-1))}}},
		{"negative duration", TypeKilled, ProtocolEventData{Execution: &ExecutionData{ExecutionTimeMS: ptr(int64(-1))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Encode(captured(t, tc.typ), &tc.p)
			require.ErrorIs(t, err, ErrInvalidEvent)
			require.Nil(t, e)
		})
	}
	for _, tc := range []struct {
		typ  string
		p    ProtocolEventData
		want string
	}{
		{TypePaused, ProtocolEventData{PauseMode: "filesystem", Execution: &ExecutionData{
			StartedAt: "2026-10-14T20:00:00+08:00", VCPUCount: ptr(int64(0)),
			MemoryMB: ptr(int64(128)), ExecutionTimeMS: ptr(int64(0)),
		}}, `{"pause_mode":"filesystem","execution":{"started_at":"2026-10-14T12:00:00Z","vcpu_count":0,"memory_mb":128,"execution_time":0}}`},
		{TypeResumed, ProtocolEventData{ResumeMode: "reboot"}, `{"resume_mode":"reboot"}`},
		{TypeKilled, ProtocolEventData{KillReason: "mapped-reason"}, `{"kill_reason":"mapped-reason","execution":{"started_at":""}}`},
		{TypeUpdated, ProtocolEventData{NetworkEgress: &NetworkEgressData{}}, `{"network_egress":{"allowed_cidrs":null,"denied_cidrs":null,"allowed_domains":null}}`},
		{TypeUpdated, ProtocolEventData{SetTimeout: ptr("2026-10-14T12:00:00Z"), NetworkEgress: &NetworkEgressData{
			AllowedCIDRs: []string{}, DeniedCIDRs: []string{}, AllowedDomains: []string{},
		}}, `{"set_timeout":"2026-10-14T12:00:00Z","network_egress":{"allowed_cidrs":[],"denied_cidrs":[],"allowed_domains":[]}}`},
		{TypeCreated, ProtocolEventData{SandboxMetadata: map[string]string{}}, `{"sandbox_metadata":{}}`},
	} {
		t.Run(tc.typ+"/"+tc.want, func(t *testing.T) {
			e, err := Encode(captured(t, tc.typ), &tc.p)
			require.NoError(t, err)
			data, err := json.Marshal(decodeBody(t, e)["event_data"])
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(data))
		})
	}
}

func TestEncodedBodyOwnsItsData(t *testing.T) {
	p := &ProtocolEventData{
		SandboxMetadata: map[string]string{"key": "value"},
		SetTimeout:      ptr("2026-10-14T12:00:00Z"),
		NetworkEgress:   &NetworkEgressData{AllowedCIDRs: []string{"10.0.0.0/8"}, AllowedDomains: []string{"example.com"}},
	}
	e, err := Encode(captured(t, TypeUpdated), p)
	require.NoError(t, err)
	before := e.Body()
	p.SandboxMetadata["key"] = "changed"
	*p.SetTimeout = ""
	p.NetworkEgress.AllowedCIDRs[0] = "changed"
	p.NetworkEgress.AllowedDomains[0] = "changed"
	body := e.Body()
	for i := range body {
		body[i] = 'x'
	}
	require.Equal(t, before, e.Body())

	execution := &ExecutionData{VCPUCount: ptr(int64(1)), MemoryMB: ptr(int64(256)), ExecutionTimeMS: ptr(int64(0))}
	e, err = Encode(captured(t, TypeKilled), &ProtocolEventData{Execution: execution})
	require.NoError(t, err)
	before = e.Body()
	*execution.VCPUCount, *execution.MemoryMB, *execution.ExecutionTimeMS = 9, 9, 9
	execution.StartedAt = "changed"
	require.Equal(t, before, e.Body())
}
