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
	if w.currSize >= w.size {
		w.currSize = 0
		for i := w.num; i >= 1; i-- {
			var n1, n2 string
			if i > 1 {
				n1 = strconv.Itoa(i - 1)
			}
			n2 = strconv.Itoa(i)
			p1 := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, n1))
			p2 := filepath.Join(w.logpath, fmt.Sprintf("%s.log.%s", w.name, n2))
			if n1 == "" {
				p1 = filepath.Join(w.logpath, fmt.Sprintf("%s.log", w.name))
			}
			if _, err := os.Stat(p1); !os.IsNotExist(err) {
				os.Rename(p1, p2)
			}
		}
		go func() {
			os.Remove(filepath.Join(w.logpath, fmt.Sprintf("%s.log.%d", w.name, w.num)))
		}()
		fullPath := filepath.Join(w.logpath, fmt.Sprintf("%s.log", w.name))
		reOpenFile(fullPath, &w.currFile, &w.openTime)
	}

	return n, nil
}

func NewRollFileWriter(logpath, name string, num, sizeMB int) *RollFileWriter {
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
