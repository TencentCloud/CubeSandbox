// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"sync/atomic"
	"time"
)

// logClock is the time snapshot shared by every log writer. A background
// goroutine refreshes it once per second while writers read it on every
// call, so it is published atomically: a reader sees one complete snapshot
// and never a torn string or a time from a different tick.
type logClock struct {
	unixTime int64
	dateHour string
	dateDay  string
}

// currClock is set by its initializer, not in init(): Go initializes a
// package-level variable before any variable whose initializer refers to
// it and before every init(), so no code in this package can Load a nil
// clock, whatever order the files are compiled in.
var currClock = func() *atomic.Pointer[logClock] {
	p := new(atomic.Pointer[logClock])
	p.Store(newLogClock(time.Now()))
	return p
}()

func newLogClock(now time.Time) *logClock {
	return &logClock{
		unixTime: now.Unix(),
		dateHour: now.Format("2006010215"),
		dateDay:  now.Format("20060102"),
	}
}

func currUnixTime() int64 { return currClock.Load().unixTime }

func currDateHour() string { return currClock.Load().dateHour }

func currDateDay() string { return currClock.Load().dateDay }

func init() {
	go func() {
		tm := time.NewTimer(time.Second)
		for {
			now := time.Now()
			d := time.Second - time.Duration(now.Nanosecond())
			tm.Reset(d)
			<-tm.C
			currClock.Store(newLogClock(time.Now()))
		}
	}()
}
