package image

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
)

func TestPipeExportClosesBothEndsWhenTarCannotStart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFuncReturn(os.Pipe, reader, writer, nil)
	err = pipeExportToDir(context.Background(), "unused", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "start tar extract") {
		t.Fatalf("error=%v", err)
	}
	for _, file := range []*os.File{reader, writer} {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("pipe end remains open: %v", err)
		}
	}
}

func TestPipeExportStopsWhenTarExitsEarly(t *testing.T) {
	dir := t.TempDir()
	installFakeCommand(t, dir, "docker", "exec /bin/cat /dev/zero")
	installFakeCommand(t, dir, "tar", "exit 1")
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := pipeExportToDir(ctx, "unused", t.TempDir()); err == nil {
		t.Fatal("failed extraction reported success")
	}
	if ctx.Err() != nil {
		t.Fatal("export blocked after tar exited until the context expired")
	}
}
