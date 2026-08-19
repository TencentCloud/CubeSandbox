// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package hotswap

import (
	"os"
	"path/filepath"
	"testing"
)

type reloadProbeCfg struct {
	Level string `yaml:"level"`
	Host  string `yaml:"host"`
}

func newReloadProbe(t *testing.T, body string) (*FileOperator, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conf.yaml")
	write(t, path, body)

	op, err := NewWatcher(path, 0, &reloadProbeCfg{})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if !op.reload(path) {
		t.Fatalf("first reload did not establish a baseline")
	}
	return op, path
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReloadDetectsCaseOnlyKeyValueChange(t *testing.T) {
	op, path := newReloadProbe(t, "level: INFO\nhost: foo.internal\n")

	write(t, path, "level: info\nhost: foo.internal\n")
	if !op.reload(path) {
		t.Fatal("reload reported no change for a case-only edit (INFO -> info)")
	}
}

func TestReloadDetectsCaseOnlyHostChange(t *testing.T) {
	op, path := newReloadProbe(t, "level: info\nhost: Foo.internal\n")

	write(t, path, "level: info\nhost: foo.internal\n")
	if !op.reload(path) {
		t.Fatal("reload reported no change for a case-only host edit (Foo -> foo)")
	}
}

func TestReloadReportsNoChangeForIdenticalConfig(t *testing.T) {
	op, path := newReloadProbe(t, "level: info\nhost: foo.internal\n")

	if op.reload(path) {
		t.Fatal("reload reported a change for an identical config")
	}
}

func TestReloadDetectsOrdinaryChange(t *testing.T) {
	op, path := newReloadProbe(t, "level: info\nhost: foo.internal\n")

	write(t, path, "level: debug\nhost: foo.internal\n")
	if !op.reload(path) {
		t.Fatal("reload reported no change for a genuine edit")
	}
}
