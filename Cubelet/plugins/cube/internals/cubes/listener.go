// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

const (
	CubeboxEventTypeUpdate = "UPDATE"
)

type CubeboxSaveOptions struct {
	NoEvent bool
}

type UpdateCubeboxOpt func(*CubeboxSaveOptions)

func WithNoEvent(o *CubeboxSaveOptions) {
	o.NoEvent = true
}

type CubeboxEvent struct {
	EventType string
	Cubebox   *cubeboxstore.CubeBox
}

type CubeboxEventListener interface {
	OnCubeboxEvent(ctx context.Context, event *CubeboxEvent) error
}

type CubeboxEventListenerRegistry interface {
	Register(listener CubeboxEventListener)
}

func (l *local) Register(f CubeboxEventListener) {
	l.listeners = append(l.listeners, f)

	all := l.List()
	for _, cb := range all {
		ctx := namespaces.WithNamespace(context.Background(), cb.Namespace)
		err := f.OnCubeboxEvent(ctx, &CubeboxEvent{
			EventType: CubeboxEventTypeUpdate,
			Cubebox:   cb,
		})
		if err != nil {
			log.L.Errorf("listener OnCubeboxEvent failed for cuebox %s: %v", cb.ID, err)
		}
	}
}

func (l *local) dispatch(event *CubeboxEvent) {
	if l.eventChan != nil && event != nil && event.Cubebox != nil {
		l.eventChan <- event
	}
}

func (l *local) startListener() {
	for event := range l.eventChan {
		go func(event *CubeboxEvent) {
			recov.HandleCrash(func(panicErr interface{}) {
				log.L.Errorf("listener OnCubeboxEvent panic: %v", panicErr)
			})
			for _, listener := range l.listeners {
				ctx := namespaces.WithNamespace(context.Background(), event.Cubebox.Namespace)
				err := listener.OnCubeboxEvent(ctx, event)
				if err != nil {
					log.L.Errorf("listener OnCubeboxEvent failed for cuebox %s: %v", event.Cubebox.ID, err)
				}
			}
		}(event)
	}
}
