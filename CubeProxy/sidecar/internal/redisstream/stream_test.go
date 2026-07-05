// SPDX-License-Identifier: Apache-2.0
//

package redisstream

import (
	"testing"
	"time"
)

func TestParseCASResult(t *testing.T) {
	tests := []struct {
		name      string
		raw       any
		wantOK    bool
		wantState string
		wantErr   bool
	}{
		{
			name:      "acquired returns previous state",
			raw:       []any{int64(1), "running"},
			wantOK:    true,
			wantState: "running",
		},
		{
			name:      "not acquired",
			raw:       []any{int64(0), "resuming"},
			wantOK:    false,
			wantState: "resuming",
		},
		{
			name:      "missing current state",
			raw:       []any{int64(0), ""},
			wantOK:    false,
			wantState: "",
		},
		{
			name:    "bad status",
			raw:     []any{int64(2), "running"},
			wantErr: true,
		},
		{
			name:    "bad shape",
			raw:     "OK",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOK, gotState, err := parseCASResult(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCASResult returned error: %v", err)
			}
			if gotOK != tt.wantOK || gotState != tt.wantState {
				t.Fatalf("parseCASResult = (%v, %q), want (%v, %q)",
					gotOK, gotState, tt.wantOK, tt.wantState)
			}
		})
	}
}

func TestCASTTLMillis(t *testing.T) {
	tests := []struct {
		name    string
		ttl     time.Duration
		want    int64
		wantErr bool
	}{
		{name: "zero", ttl: 0, wantErr: true},
		{name: "negative", ttl: -time.Second, wantErr: true},
		{name: "sub millisecond rounds up", ttl: time.Microsecond, want: 1},
		{name: "millisecond", ttl: time.Millisecond, want: 1},
		{name: "second", ttl: time.Second, want: 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := casTTLMillis(tt.ttl)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("casTTLMillis returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("casTTLMillis = %d, want %d", got, tt.want)
			}
		})
	}
}
