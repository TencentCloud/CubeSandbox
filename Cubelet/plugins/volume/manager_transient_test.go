// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
)

// recordingPlugin records the ref-count the Manager hands to the plugin.
type recordingPlugin struct {
	attachRef []int64
	detachRef []int64
}

func (p *recordingPlugin) Name() string                             { return "rec" }
func (p *recordingPlugin) PluginType() PluginType                   { return PluginTypeBuiltin }
func (p *recordingPlugin) Init(context.Context, PluginConfig) error { return nil }
func (p *recordingPlugin) Close() error                             { return nil }
func (p *recordingPlugin) Attach(_ context.Context, r *AttachRequest) (*AttachResult, error) {
	p.attachRef = append(p.attachRef, r.RefCount)
	return &AttachResult{VolumeID: r.VolumeID, HostPath: "/base/" + r.VolumeID}, nil
}
func (p *recordingPlugin) Detach(_ context.Context, r *DetachRequest) error {
	p.detachRef = append(p.detachRef, r.RefCount)
	return nil
}

func newTransientTestManager(t *testing.T) (*Manager, *recordingPlugin) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "rc.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := refcount.New(db)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{byName: make(map[string]VolumePlugin)}
	plug := &recordingPlugin{}
	m.Register(plug)
	m.SetRefCountStore(store)
	return m, plug
}

type step struct {
	attach      bool
	holder      string
	wantReport  bool  // NodeRefFirstAttach / NodeRefLastDetach
	wantPlugRef int64 // ref-count handed to the plugin
}

// Content-request holders must pin the mount (plugin ref-count) without ever
// starting or hiding a node-level transition that CubeMaster tracks, in either
// order relative to a sandbox. Otherwise CubeMaster's delete protection drifts.
func TestTransientHoldersDoNotAffectReportedTransitions(t *testing.T) {
	const content, sandbox = refcount.TransientHolderPrefix + "aaaa", "sb-1"

	cases := []struct {
		name  string
		steps []step
	}{
		{"content then sandbox", []step{
			{true, content, false, 0},
			{true, sandbox, true, 1},   // sandbox start must still be reported
			{false, content, false, 1}, // sandbox still holds the mount
			{false, sandbox, true, 0},  // last sandbox gone: report, tear down
		}},
		{"sandbox then content", []step{
			{true, sandbox, true, 0},
			{true, content, false, 1},
			{false, sandbox, true, 1},  // reported so delete is allowed; mount stays for the request
			{false, content, false, 0}, // request ends: tear down, nothing to report
		}},
		{"content only", []step{
			{true, content, false, 0},
			{false, content, false, 0},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, plug := newTransientTestManager(t)
			ctx := context.Background()
			plug.attachRef, plug.detachRef = nil, nil
			for i, s := range tc.steps {
				if s.attach {
					req := &AttachRequest{SandboxID: s.holder, Namespace: "ns", VolumeID: "vol-1", Driver: "rec"}
					if _, err := m.Attach(ctx, req); err != nil {
						t.Fatalf("step %d attach: %v", i, err)
					}
					if req.NodeRefFirstAttach != s.wantReport {
						t.Errorf("step %d attach %s: NodeRefFirstAttach=%v, want %v", i, s.holder, req.NodeRefFirstAttach, s.wantReport)
					}
					if got := plug.attachRef[len(plug.attachRef)-1]; got != s.wantPlugRef {
						t.Errorf("step %d attach %s: plugin RefCount=%d, want %d", i, s.holder, got, s.wantPlugRef)
					}
					continue
				}
				req := &DetachRequest{SandboxID: s.holder, Namespace: "ns", VolumeID: "vol-1", Driver: "rec"}
				if err := m.Detach(ctx, req); err != nil {
					t.Fatalf("step %d detach: %v", i, err)
				}
				if req.NodeRefLastDetach != s.wantReport {
					t.Errorf("step %d detach %s: NodeRefLastDetach=%v, want %v", i, s.holder, req.NodeRefLastDetach, s.wantReport)
				}
				if got := plug.detachRef[len(plug.detachRef)-1]; got != s.wantPlugRef {
					t.Errorf("step %d detach %s: plugin RefCount=%d, want %d", i, s.holder, got, s.wantPlugRef)
				}
			}
		})
	}
}

// Two real sandboxes keep the existing behaviour: only the first attach and the
// last detach on the node are reported.
func TestSandboxTransitionsUnchanged(t *testing.T) {
	m, _ := newTransientTestManager(t)
	ctx := context.Background()
	want := []struct {
		attach bool
		id     string
		report bool
	}{
		{true, "sb-1", true}, {true, "sb-2", false}, {false, "sb-1", false}, {false, "sb-2", true},
	}
	for i, w := range want {
		if w.attach {
			req := &AttachRequest{SandboxID: w.id, Namespace: "ns", VolumeID: "vol-2", Driver: "rec"}
			if _, err := m.Attach(ctx, req); err != nil {
				t.Fatal(err)
			}
			if req.NodeRefFirstAttach != w.report {
				t.Errorf("step %d: NodeRefFirstAttach=%v, want %v", i, req.NodeRefFirstAttach, w.report)
			}
			continue
		}
		req := &DetachRequest{SandboxID: w.id, Namespace: "ns", VolumeID: "vol-2", Driver: "rec"}
		if err := m.Detach(ctx, req); err != nil {
			t.Fatal(err)
		}
		if req.NodeRefLastDetach != w.report {
			t.Errorf("step %d: NodeRefLastDetach=%v, want %v", i, req.NodeRefLastDetach, w.report)
		}
	}
}
