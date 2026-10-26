// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
Package hotswap implements file hot update,

	support load init config and notify watchers when file update
*/
package hotswap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"runtime/debug"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	yaml "gopkg.in/yaml.v3"
)

type FileOperator struct {
	*fsnotify.Watcher
	Logger
	sync.RWMutex
	listeners    []Listener
	backupOldCfg []byte
	syncInterval int
	path         string
	obj          reflect.Type
	data         interface{}
	stop         chan struct{}
}

func NewWatcher(path string, syncInterval int, conf interface{}) (*FileOperator, error) {
	t, err := getType(conf)
	if err != nil {
		return nil, err
	}
	return &FileOperator{
		obj:          t,
		path:         path,
		syncInterval: syncInterval,
		listeners:    make([]Listener, 0),
		Logger:       &defaultLogger{},
		stop:         make(chan struct{}),
	}, nil
}

func getType(conf interface{}) (reflect.Type, error) {
	if conf == nil {
		return nil, errors.New("nil")
	}
	v := reflect.ValueOf(conf)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	return v.Type(), nil
}

func (o *FileOperator) Init() (interface{}, error) {
	config, err := o.load(o.path)
	if err != nil {
		return nil, err
	}
	new, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	o.backupOldCfg = new
	err = o.listenFile(o.path)
	if err != nil {
		return nil, err
	}
	o.data = config
	return config, nil
}

func (o *FileOperator) SetLogger(l Logger) {
	o.Logger = l
}

func (o *FileOperator) reload(path string) bool {
	config, err := o.load(path)
	if err != nil {
		return false
	}
	new, err := yaml.Marshal(config)
	if len(o.backupOldCfg) == len(new) && bytes.EqualFold(o.backupOldCfg, new) {
		return false
	}
	o.backupOldCfg = new
	o.data = config
	o.notify(config)
	return true
}

func (o *FileOperator) load(path string) (interface{}, error) {
	conf := reflect.New(o.obj).Interface()
	data, err := os.ReadFile(path)
	if err != nil {
		o.Errorf(context.Background(), "Read file:%s fail:%v", path, err)
		return nil, err
	}
	if err := yaml.Unmarshal(data, conf); err != nil {
		o.Errorf(context.Background(), "Unmarshal file:%s fail:%v", path, err)
		return nil, err
	}
	return conf, nil
}

func (o *FileOperator) listenFile(path string) error {
	fileWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	o.Watcher = fileWatcher
	recov.GoWithRetry(func() {
		ticker := time.NewTicker(time.Duration(o.syncInterval) * time.Second)
		for {
			select {
			case _, ok := <-o.stop:
				if !ok {
					return
				}
			case event, ok := <-o.Watcher.Events:
				if !ok {
					return
				}
				o.Infof(context.Background(), "OnFileEvent:%v\n", event)
				switch {
				case event.Op&fsnotify.Write == fsnotify.Write:
					o.reload(path)
				case event.Op&fsnotify.Remove == fsnotify.Remove:
					time.Sleep(time.Millisecond * 200)
					if err := o.Watcher.Add(path); err != nil {
						o.Errorf(context.Background(), "fileWatcher reWatch file error:%v", err)
					}
					o.reload(path)
				case event.Op&fsnotify.Create == fsnotify.Create:
					o.reload(path)
				}
			case err, ok := <-o.Watcher.Errors:
				if !ok {
					return
				}
				o.Errorf(context.Background(), "FileWatcher watch:%s err:%v", path, err)
			case <-ticker.C:

				success := o.reload(path)

				if success {
					_ = o.Watcher.Remove(path)
					if err = o.Watcher.Add(path); err != nil {
						o.Errorf(context.Background(), "Add watch:%s fail:%v", path, err)
					}
				}
			}
		}
	}, 10, func(panicError interface{}) {
		o.Errorf(context.Background(), "listen file:%s fail:%v", path, string(debug.Stack()))
	})
	if err = o.Watcher.Add(path); err != nil {
		return err
	}
	o.Infof(context.Background(), "listen file[%s]", path)
	return nil
}

func (o *FileOperator) notify(config interface{}) {
	if len(o.listeners) == 0 {
		return
	}
	for _, l := range o.listeners {
		l.OnEvent(config)
	}
}

func (o *FileOperator) Load() interface{} {
	return o.data
}

func (o *FileOperator) Close() error {
	select {
	case _, ok := <-o.stop:
		if !ok {
			return nil
		}
		close(o.stop)
	default:
		close(o.stop)
	}
	return o.Watcher.Close()

}

type Listener interface {
	OnEvent(data interface{})
}

func (o *FileOperator) AppendWatcher(listener Listener) {
	o.Lock()
	defer o.Unlock()
	o.listeners = append(o.listeners, listener)
}
