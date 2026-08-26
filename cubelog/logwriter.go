// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	DAY DateType = iota
	HOUR
)

// Defaults match CubeMaster's shipped 10 files / 100MB. Cubelet applies its
// own 500MB default before calling this constructor. NewRollFileWriter used
// to treat 0 as "never rename", so a missing file_num/file_size left a single
// unbounded .log (seen as multi-GB cubemaster-req.log files).
const (
	defaultRollFileNum    = 10
	defaultRollFileSizeMB = 100
)

type ConsoleWriter struct {
}

type RollFileWriter struct {
	logpath  string
	name     string
	num      int
	size     int64
	currSize int64
	currFile *os.File
	openTime int64
}

type DateWriter struct {
	logpath   string
	name      string
	dateType  DateType
	num       int
	currDate  string
	currFile  *os.File
	openTime  int64
	hasPrefix bool
}

type HourWriter struct {
}

type DateType uint8

func reOpenFile(path string, currFile **os.File, openTime *int64) {
	*openTime = currUnixTime
	if *currFile != nil {
		(*currFile).Close()
	}
	of, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err == nil {
		*currFile = of
	} else {
		fmt.Println("open log file error", err)
	}
}

func (w *ConsoleWriter) Write(v []byte) (int, error) {
	return os.Stdout.Write(v)
}

func (w *RollFileWriter) Write(v []byte) (int, error) {
	if w.currFile == nil || w.openTime+10 < currUnixTime {
		fullPath := filepath.Join(w.logpath, fmt.Sprintf("%s.log", w.name))
		reOpenFile(fullPath, &w.currFile, &w.openTime)
	}
	if w.currFile == nil {
		return 0, errors.New("w.currFile was nil")
	}
	n, _ := w.currFile.Write(v)
	w.currSize += int64(n)
	if w.size > 0 && w.num >= 1 && w.currSize >= w.size {
		// file_num is total retained files (live + numbered backups), matching
		// Cubelet config.toml. Shift only through `.log.(num-1)` so we do
		// not keep an extra `.log.num` after dropping the racy async delete.
		live := filepath.Join(w.logpath, fmt.Sprintf("%s.log", w.name))
		if w.num == 1 {
			if w.currFile != nil {
				_ = w.currFile.Close()
				w.currFile = nil
			}
			if err := os.Truncate(live, 0); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "cubelog: failed to truncate %s: %v\n", live, err)
				return n, nil
			}
			w.currSize = 0
			reOpenFile(live, &w.currFile, &w.openTime)
			return n, nil
		}
		var liveRenameErr error
		for i := w.num - 1; i >= 1; i-- {
			var n1, n2 string
			if i > 1 {
				n1 = strconv.Itoa(i - 1)
			}
			n2 = strconv.Itoa(i)
			p1 := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, n1))
			p2 := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, n2))
			if n1 == "" {
				p1 = live
			}
			err := os.Rename(p1, p2)
			if i == 1 {
				liveRenameErr = err
			}
			if err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "cubelog: failed to rotate %s to %s: %v\n", p1, p2, err)
			}
		}
		if liveRenameErr != nil && !os.IsNotExist(liveRenameErr) {
			// Keep currSize so a failed live rename cannot restart the
			// unbounded-append path this PR is closing. ENOENT means the
			// live path is gone (e.g. operator deleted it); fall through
			// and reopen so later writes are not lost on the unlinked inode.
			// This guard only fires when `.log.1` itself cannot be replaced
			// (typical num==2). For num>=3 a blocker at `.log.1` is shifted
			// to `.log.2` first, so the live rename usually succeeds.
			return n, nil
		}
		w.currSize = 0
		reOpenFile(live, &w.currFile, &w.openTime)
	}

	return n, nil
}

func normalizeRollArgs(num, sizeMB int) (int, int) {
	// 0 is the YAML zero value for unset fields, not a misconfiguration.
	if num < 0 {
		fmt.Fprintf(os.Stderr, "cubelog: invalid roll file_num=%d, defaulting to %d\n", num, defaultRollFileNum)
		num = defaultRollFileNum
	} else if num == 0 {
		num = defaultRollFileNum
	}
	if sizeMB < 0 {
		fmt.Fprintf(os.Stderr, "cubelog: invalid roll file_size=%d, defaulting to %d MB\n", sizeMB, defaultRollFileSizeMB)
		sizeMB = defaultRollFileSizeMB
	} else if sizeMB == 0 {
		sizeMB = defaultRollFileSizeMB
	}
	return num, sizeMB
}

func NewRollFileWriter(logpath, name string, num, sizeMB int) *RollFileWriter {
	num, sizeMB = normalizeRollArgs(num, sizeMB)
	w := &RollFileWriter{
		logpath: logpath,
		name:    name,
		num:     num,
		size:    int64(sizeMB) * 1024 * 1024,
	}
	fullPath := filepath.Join(logpath, name+".log")
	st, _ := os.Stat(fullPath)
	if st != nil {
		w.currSize = st.Size()
	}
	return w
}

func (w *DateWriter) Write(v []byte) (int, error) {
	if w.currFile == nil || w.openTime+10 < currUnixTime {
		fullPath := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, w.currDate))
		reOpenFile(fullPath, &w.currFile, &w.openTime)
	}
	if w.currFile == nil {
		return 0, errors.New("w.currFile was nil")
	}

	currDate := w.getCurrDate()
	if w.currDate != currDate {
		w.currDate = currDate
		w.cleanOldLogs()
		fullPath := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, w.currDate))
		reOpenFile(fullPath, &w.currFile, &w.openTime)
	}

	n, _ := w.currFile.Write(v)
	return n, nil
}

func NewDateWriter(logpath, name string, dateType DateType, num int) *DateWriter {
	w := &DateWriter{
		logpath:   logpath,
		name:      name,
		num:       num,
		dateType:  dateType,
		hasPrefix: true,
	}
	w.currDate = w.getCurrDate()
	return w
}

func (w *DateWriter) cleanOldLogs() {
	format := "20060102"
	duration := -time.Hour * 24
	if w.dateType == HOUR {
		format = "2006010215"
		duration = -time.Hour
	}

	t := time.Now()
	t = t.Add(duration * time.Duration(w.num))
	for i := 0; i < 30; i++ {
		t = t.Add(duration)
		k := t.Format(format)
		fullPath := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, k))
		if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
			os.Remove(fullPath)
		}
	}
	return
}

func (w *DateWriter) getCurrDate() string {
	if w.dateType == HOUR {
		return currDateHour
	}
	return currDateDay
}
