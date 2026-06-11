// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"context"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

var (
	cubelogPackage string = "github.com/tencentcloud/CubeSandbox/cubelog"

	minimumCallerDepth int

	callerInitOnce sync.Once

	skipCallerDepth int

	CallerPrettyfier func(*runtime.Frame) (file string)
)

const (
	maximumCallerDepth int = 25
)

const (
	KeyAlarmType contextKey = "alarm_type"
	KeyAlarmName contextKey = "alarm_name"
)

type logFormat int

const (
	TextFormat logFormat = iota
	JSONFormat
)

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
	FATAL
	OFF
)

var (
	logLevel = DEBUG

	longFilePath = false

	mu           sync.Mutex
	loggerMap    = make(map[string]*Logger)
	logQueueSize = 10000
	logQueue     = make(chan *logValue, logQueueSize)
)

type Logger struct {
	name         string
	format       logFormat
	writer       io.Writer
	debug        bool
	mu           sync.Mutex
	entryPool    sync.Pool
	customFields Fields
}

type LogLevel uint8

type logValue struct {
	level  LogLevel
	value  []byte
	fileNo string
	logger *Logger
}

func init() {

	skipCallerDepth = 0
	minimumCallerDepth = 1
	go flushLog(true)
}

func (lv *LogLevel) String() string {
	switch *lv {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	case FATAL:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

func GetLogger(name string) *Logger {
	mu.Lock()
	defer mu.Unlock()

	if lg, ok := loggerMap[name]; ok {
		return lg
	}
	lg := &Logger{
		name:   name,
		format: JSONFormat,
		writer: &ConsoleWriter{},
		debug:  true,
		mu:     sync.Mutex{},
	}
	loggerMap[name] = lg
	return lg
}

func SetLevel(level LogLevel) {
	logLevel = level
}

func GetLevel() LogLevel {
	return logLevel
}

func SetSkipCallerDepth(depth int) {
	skipCallerDepth = depth
}

func SetCallerPrettyfier(f func(*runtime.Frame) (file string)) {
	CallerPrettyfier = f
}

func EnableLongFilePath() {
	longFilePath = true
}

func StringToLevel(level string) LogLevel {
	switch level {
	case "DEBUG":
		return DEBUG
	case "INFO":
		return INFO
	case "WARN":
		return WARN
	case "ERROR":
		return ERROR
	case "FATAL":
		return FATAL
	default:
		return DEBUG
	}
}

func (logger *Logger) SetLogName(name string) {
	logger.name = name
}

func (logger *Logger) SetLogFormat(format logFormat) {
	logger.format = format
}

func (logger *Logger) EnableFileLog() {
	logger.debug = true
}

func (logger *Logger) SetFileRoller(logpath string, num int, sizeMB int) error {
	if err := os.MkdirAll(logpath, 0755); err != nil {
		panic(err)
	}
	w := NewRollFileWriter(logpath, logger.name, num, sizeMB)
	logger.writer = w
	return nil
}

func (logger *Logger) IsConsoleWriter() bool {
	if reflect.TypeOf(logger.writer) == reflect.TypeOf(&ConsoleWriter{}) {
		return true
	}
	return false
}

func (logger *Logger) SetOutput(w io.Writer) {
	logger.writer = w
}

func (logger *Logger) SetDayRoller(logpath string, num int) error {
	if err := os.MkdirAll(logpath, 0755); err != nil {
		return err
	}
	w := NewDateWriter(logpath, logger.name, DAY, num)
	logger.writer = w
	return nil
}

func (logger *Logger) SetHourRoller(logpath string, num int) error {
	if err := os.MkdirAll(logpath, 0755); err != nil {
		return err
	}
	w := NewDateWriter(logpath, logger.name, HOUR, num)
	logger.writer = w
	return nil
}

func (logger *Logger) SetConsole() {
	logger.writer = &ConsoleWriter{}
}

func (logger *Logger) SetCustomFields(fields Fields) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.customFields = fields
}

func (logger *Logger) GetCustomFields() Fields {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.customFields
}

func (logger *Logger) WithContext(ctx context.Context) *Entry {
	e := logger.newEntry()
	defer logger.releaseEntry(e)
	entry := e.WithContext(ctx)

	if logger.customFields != nil && len(logger.customFields) > 0 {
		entry = entry.WithFields(logger.customFields)
	}

	return entry
}

func (logger *Logger) WithFields(fields Fields) *Entry {
	e := logger.newEntry()
	defer logger.releaseEntry(e)

	if logger.customFields != nil && len(logger.customFields) > 0 {
		mergedFields := make(Fields, len(logger.customFields)+len(fields))
		for k, v := range logger.customFields {
			mergedFields[k] = v
		}
		for k, v := range fields {
			mergedFields[k] = v
		}
		return e.WithFields(mergedFields)
	}

	return e.WithFields(fields)
}

func (logger *Logger) writef(ctx context.Context, level LogLevel, format string, v []interface{}) {
	e := logger.newEntry()
	defer logger.releaseEntry(e)

	if logger.customFields != nil && len(logger.customFields) > 0 {
		e = e.WithFields(logger.customFields)
	}

	e.writef(ctx, level, format, v)
}

func (logger *Logger) Debug(v ...interface{}) {
	logger.writef(context.TODO(), DEBUG, "", v)
}

func (logger *Logger) Info(v ...interface{}) {
	logger.writef(context.TODO(), INFO, "", v)
}

func (logger *Logger) Warn(v ...interface{}) {
	logger.writef(context.TODO(), WARN, "", v)
}

func (logger *Logger) Error(v ...interface{}) {
	logger.writef(context.TODO(), ERROR, "", v)
}

func (logger *Logger) Fatal(v ...interface{}) {
	logger.writef(context.TODO(), FATAL, "", v)
}

func (logger *Logger) Debugf(format string, v ...interface{}) {
	logger.writef(context.TODO(), DEBUG, format, v)
}

func (logger *Logger) Infof(format string, v ...interface{}) {
	logger.writef(context.TODO(), INFO, format, v)
}

func (logger *Logger) Warnf(format string, v ...interface{}) {
	logger.writef(context.TODO(), WARN, format, v)
}

func (logger *Logger) Errorf(format string, v ...interface{}) {
	logger.writef(context.TODO(), ERROR, format, v)
}

func (logger *Logger) Fatalf(format string, v ...interface{}) {
	logger.writef(context.TODO(), FATAL, format, v)
}

func getPackageName(f string) string {
	for {
		lastPeriod := strings.LastIndex(f, ".")
		lastSlash := strings.LastIndex(f, "/")
		if lastPeriod > lastSlash {
			f = f[:lastPeriod]
		} else {
			break
		}
	}

	return f
}

func (logger *Logger) newEntry() *Entry {
	entry, ok := logger.entryPool.Get().(*Entry)
	if ok {
		return entry
	}
	return NewEntry(logger)
}

func (logger *Logger) releaseEntry(entry *Entry) {
	entry.data = map[string]interface{}{}
	logger.entryPool.Put(entry)
}

func (l *Logger) WriteLog(msg []byte) {
	select {
	case logQueue <- &logValue{value: msg, logger: l}:
	default:

	}
}

func flushLog(sync bool) {
	if sync {
		for v := range logQueue {
			v.logger.writer.Write(v.value)
		}
	} else {
		for {
			select {
			case v := <-logQueue:

				v.logger.writer.Write(v.value)
				continue
			default:
				return
			}
		}
	}
}
