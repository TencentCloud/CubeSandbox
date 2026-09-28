// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"io"
	"os"
	"testing"

	pkgversion "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
	"github.com/urfave/cli"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func runVersion(args ...string) error {
	app := cli.NewApp()
	app.Name = "cubemastercli"
	app.Commands = []cli.Command{Command}
	return app.Run(append([]string{"cubemastercli"}, args...))
}

func TestVersionCommandDefault(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runVersion("version"); err != nil {
			t.Fatal(err)
		}
	})

	want := pkgversion.VersionString("cubemastercli") + "\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestVersionCommandVersionOnly(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runVersion("version", "--versiononly"); err != nil {
			t.Fatal(err)
		}
	})

	want := pkgversion.Version + "\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestVersionCommandWithClient(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runVersion("version", "--withclient"); err != nil {
			t.Fatal(err)
		}
	})

	want := pkgversion.VersionString("cubemastercli") + "\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}
