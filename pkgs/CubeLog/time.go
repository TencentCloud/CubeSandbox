// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"sync/atomic"
	"time"
)

// logClock is the per-second time snapshot shared by all log writers, published
// atomically so strings are never torn. Accessors load independently: a Write
// can straddle a tick and land one line in the previous hour/day file, but no
// line is lost.
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
