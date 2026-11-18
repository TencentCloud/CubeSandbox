// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"sync"
	"testing"
	"time"
)

// TestWritersReadClockWhileItRefreshes runs log writers while the clock is
// refreshed, as the background ticker does once per second. Run with -race:
// the clock used to be plain package variables written by the ticker
// goroutine and read by every Write.
func TestWritersReadClockWhileItRefreshes(t *testing.T) {
	dir := t.TempDir()
	roll := NewRollFileWriter(dir, "roll", 2, 1)
	day := NewDateWriter(dir, "day", DAY, 1)
	hour := NewDateWriter(dir, "hour", HOUR, 1)
	t.Cleanup(func() {
		for _, f := range []interface{ Close() error }{roll.currFile, day.currFile, hour.currFile} {
			if f != nil {
				_ = f.Close()
			}
		}
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				currClock.Store(newLogClock(time.Now()))
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
	close(stop)
	wg.Wait()
}

func TestLogClockSnapshotIsConsistent(t *testing.T) {
	now := time.Date(2026, 11, 18, 23, 59, 59, 0, time.Local)
	c := newLogClock(now)
	if c.unixTime != now.Unix() || c.dateHour != "2026111823" || c.dateDay != "20261118" {
		t.Fatalf("snapshot = %+v", *c)
	}
}
