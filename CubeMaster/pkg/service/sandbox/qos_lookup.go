package sandbox

import (
	"context"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/qos"
)

type TemplateQosLookupHook func(context.Context, string) (*qos.Config, error)

var (
	templateQosLookupMu   sync.RWMutex
	templateQosLookupHook TemplateQosLookupHook
)

func SetTemplateQosLookupHook(hook TemplateQosLookupHook) {
	templateQosLookupMu.Lock()
	templateQosLookupHook = hook
	templateQosLookupMu.Unlock()
}

func lookupTemplateQos(ctx context.Context, templateID string) (*qos.Config, error) {
	templateQosLookupMu.RLock()
	hook := templateQosLookupHook
	templateQosLookupMu.RUnlock()
	if hook == nil {
		return nil, nil
	}
	return hook(ctx, templateID)
}
