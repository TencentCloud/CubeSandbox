// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWritersReadClockWhileItRefreshes runs log writers while the clock is
// refreshed, as the background ticker does once per second. It only fails
// under the race detector (make cubelog-test runs it with -race): the clock
// used to be plain package variables written by the ticker goroutine and
// read by every Write without synchronization.
func TestWritersReadClockWhileItRefreshes(t *testing.T) {
	oldClock := currClock.Load()
	t.Cleanup(func() { currClock.Store(oldClock) })

	dir := t.TempDir()
	roll := NewRollFileWriter(dir, "roll", 2, 1)
	day := NewDateWriter(dir, "day", DAY, 1)
	hour := NewDateWriter(dir, "hour", HOUR, 1)
	t.Cleanup(func() {
		for _, f := range []*os.File{roll.currFile, day.currFile, hour.currFile} {
			if f != nil {
				_ = f.Close()
			}
		}
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		close(stop)
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				currClock.Store(newLogClock(time.Now()))
				runtime.Gosched()
			}
		}
	}()

	line := []byte("log line\n")
	for i := 0; i < 200; i++ {
		for _, w := range []interface{ Write([]byte) (int, error) }{roll, day, hour} {
			if _, err := w.Write(line); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
		}
	}

	st, err := os.Stat(filepath.Join(dir, "roll.log"))
	if err != nil {
		t.Fatalf("roll.log missing after writes: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("roll.log is empty after writes")
	}
}

// TestLogClockSnapshotFormats checks the fields of one snapshot. It does not
// test concurrency; TestWritersReadClockWhileItRefreshes does.
func TestLogClockSnapshotFormats(t *testing.T) {
	now := time.Date(2026, 11, 18, 23, 59, 59, 0, time.Local)
	c := newLogClock(now)
	if c.unixTime != now.Unix() || c.dateHour != "2026111823" || c.dateDay != "20261118" {
		t.Fatalf("snapshot = %+v", *c)
	}
}

// waitJustAfterSecond returns shortly after the package ticker has run, so a
// test that Stores a fixed snapshot has almost a full second before the
// ticker overwrites it with the real time.
func waitJustAfterSecond(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ns := time.Duration(time.Now().Nanosecond())
		if ns >= 10*time.Millisecond && ns <= 100*time.Millisecond {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("could not reach a post-tick quiet window")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Fatalf("%s = %q, want it to contain %q", path, string(b), want)
	}
}

// TestDateWriterNamesFileFromClock pins the writer to a fixed snapshot and
// checks the file name it picks. The fixed date is far in the past so the
// real-time cleanOldLogs window never touches it.
func TestDateWriterNamesFileFromClock(t *testing.T) {
	oldClock := currClock.Load()
	t.Cleanup(func() { currClock.Store(oldClock) })

	dir := t.TempDir()
	fixed := time.Date(2020, 3, 4, 5, 6, 7, 0, time.Local)

	waitJustAfterSecond(t)
	currClock.Store(newLogClock(fixed))

	day := NewDateWriter(dir, "day", DAY, 1)
	hour := NewDateWriter(dir, "hour", HOUR, 1)
	t.Cleanup(func() {
		for _, f := range []*os.File{day.currFile, hour.currFile} {
			if f != nil {
				_ = f.Close()
			}
		}
	})

	if _, err := day.Write([]byte("day\n")); err != nil {
		t.Fatalf("day write: %v", err)
	}
	assertFileExists(t, filepath.Join(dir, "day.log.20200304"))

	if _, err := hour.Write([]byte("hour\n")); err != nil {
		t.Fatalf("hour write: %v", err)
	}
	assertFileExists(t, filepath.Join(dir, "hour.log.2020030405"))
}

func TestDateWriterSwitchDay(t *testing.T) {
	oldClock := currClock.Load()
	t.Cleanup(func() { currClock.Store(oldClock) })

	dir := t.TempDir()
	t1 := time.Date(2020, 3, 4, 23, 59, 59, 0, time.Local)
	t2 := t1.Add(2 * time.Second)

	waitJustAfterSecond(t)
	currClock.Store(newLogClock(t1))

	w := NewDateWriter(dir, "day", DAY, 1)
	t.Cleanup(func() {
		if w.currFile != nil {
			_ = w.currFile.Close()
		}
	})
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}

	currClock.Store(newLogClock(t2))
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	assertFileExists(t, filepath.Join(dir, "day.log.20200304"))
	assertFileContains(t, filepath.Join(dir, "day.log.20200305"), "second")
}

func TestDateWriterSwitchHour(t *testing.T) {
	oldClock := currClock.Load()
	t.Cleanup(func() { currClock.Store(oldClock) })

	dir := t.TempDir()
	t1 := time.Date(2020, 3, 4, 5, 59, 59, 0, time.Local)
	t2 := t1.Add(2 * time.Second)

	waitJustAfterSecond(t)
	currClock.Store(newLogClock(t1))

	w := NewDateWriter(dir, "hour", HOUR, 1)
	t.Cleanup(func() {
		if w.currFile != nil {
			_ = w.currFile.Close()
		}
	})
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}

	currClock.Store(newLogClock(t2))
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	assertFileExists(t, filepath.Join(dir, "hour.log.2020030405"))
	assertFileContains(t, filepath.Join(dir, "hour.log.2020030406"), "second")
}

// clockInitializedAtPkgInit runs during package-variable initialization,
// before any init(), so it catches a regression that moves the currClock
// Store back into init().
var clockInitializedAtPkgInit = func() bool {
	if currClock == nil {
		return false
	}
	c := currClock.Load()
	return c != nil && c.unixTime > 0 && len(c.dateHour) == 10 && len(c.dateDay) == 8
}()

func TestClockInitializedBeforeInit(t *testing.T) {
	if !clockInitializedAtPkgInit {
		t.Fatal("currClock must be initialized by its variable initializer, not in init()")
	}
}
