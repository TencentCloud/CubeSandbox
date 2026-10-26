// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
Package hotswap implements file hot update,

	support load init config and notify watchers when file update
*/
package hotswap

import (
	"context"
	"fmt"
	"time"
)

type ConfigOperator interface {
	Init() (interface{}, error)

	Close() error

	AppendWatcher(listener Listener)

	Load() interface{}

	SetLogger(l Logger)
}

type Logger interface {
	Debugf(ctx context.Context, format string, v ...interface{})

	Infof(ctx context.Context, format string, v ...interface{})

	Warnf(ctx context.Context, format string, v ...interface{})

	Errorf(ctx context.Context, format string, v ...interface{})

	Fatalf(ctx context.Context, format string, v ...interface{})
}

type defaultLogger struct {
}

func (l *defaultLogger) Debugf(ctx context.Context, format string, v ...interface{}) {
	fmt.Printf("%v,"+format+"\n",
		append([]interface{}{fmt.Sprintf("%v", time.Now().Format(time.RFC3339Nano))}, v...)...)
}

func (l *defaultLogger) Infof(ctx context.Context, format string, v ...interface{}) {
	fmt.Printf("%v,"+format+"\n",
		append([]interface{}{fmt.Sprintf("%v", time.Now().Format(time.RFC3339Nano))}, v...)...)
}

func (l *defaultLogger) Warnf(ctx context.Context, format string, v ...interface{}) {
	fmt.Printf("%v,"+format+"\n",
		append([]interface{}{fmt.Sprintf("%v", time.Now().Format(time.RFC3339Nano))}, v...)...)
}

func (l *defaultLogger) Errorf(ctx context.Context, format string, v ...interface{}) {
	fmt.Printf("%v,"+format+"\n",
		append([]interface{}{fmt.Sprintf("%v", time.Now().Format(time.RFC3339Nano))}, v...)...)
}

func (l *defaultLogger) Fatalf(ctx context.Context, format string, v ...interface{}) {
	fmt.Printf("%v,"+format+"\n",
		append([]interface{}{fmt.Sprintf("%v", time.Now().Format(time.RFC3339Nano))}, v...)...)
}
