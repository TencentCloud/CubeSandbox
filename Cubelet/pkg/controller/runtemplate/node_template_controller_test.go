// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

type mockCDPHook struct {
	mu           sync.Mutex
	preDeleteErr error
	callCount    int
	lastID       string
	calls        []string
}

func (m *mockCDPHook) Name() string {
	return "mock-cdp-hook-test"
}

func (m *mockCDPHook) PreDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.callCount++
	m.lastID = opt.ID
	m.calls = append(m.calls, opt.ID)
	return m.preDeleteErr
}

func (m *mockCDPHook) PostDelete(ctx context.Context, opt *cdp.DeleteOption, opts ...interface{}) error {
	return nil
}

func (m *mockCDPHook) GetCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

func (m *mockCDPHook) GetLastID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastID
}

func (m *mockCDPHook) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callCount = 0
	m.lastID = ""
	m.calls = nil
	m.preDeleteErr = nil
}

type testLocalCubeRunTemplateManager struct {
	mockStore         *mockLocalTemplateStore
	unusedTemplateMap map[string]*unusedTemplate
	lock              sync.RWMutex
}

func (h *testLocalCubeRunTemplateManager) deleteLocalTemplate(ctx context.Context, localTemplate *templatetypes.LocalRunTemplate) error {
	h.lock.Lock()
	delete(h.unusedTemplateMap, localTemplate.TemplateID)
	h.lock.Unlock()

	err := cdp.PreDelete(ctx, &cdp.DeleteOption{
		ID:           localTemplate.TemplateID,
		ResourceType: cdp.ResourceCubeRunTemplate,
	})
	if err != nil {
		return err
	}

	err = h.mockStore.Delete(localTemplate)
	if err != nil {
		return err
	}
	return nil
}

type localTemplateStore interface {
	Update(*templatetypes.LocalRunTemplate) error
	Delete(*templatetypes.LocalRunTemplate) error
}

type mockLocalTemplateStore struct {
	mu              sync.RWMutex
	templates       map[string]*templatetypes.LocalRunTemplate
	updateErr       error
	deleteErr       error
	deleteCallCount int
	updateCallCount int
}

func newMockLocalTemplateStore() *mockLocalTemplateStore {
	return &mockLocalTemplateStore{
		templates: make(map[string]*templatetypes.LocalRunTemplate),
	}
}

func (m *mockLocalTemplateStore) Update(template *templatetypes.LocalRunTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.updateCallCount++
	if m.updateErr != nil {
		return m.updateErr
	}

	m.templates[template.TemplateID] = template
	return nil
}

func (m *mockLocalTemplateStore) Delete(template *templatetypes.LocalRunTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.deleteCallCount++
	if m.deleteErr != nil {
		return m.deleteErr
	}

	delete(m.templates, template.TemplateID)
	return nil
}

func (m *mockLocalTemplateStore) Get(key string) (*templatetypes.LocalRunTemplate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if template, ok := m.templates[key]; ok {
		return template, nil
	}
	return nil, errors.New("not found")
}

func (m *mockLocalTemplateStore) GetDeleteCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.deleteCallCount
}

func (m *mockLocalTemplateStore) GetUpdateCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.updateCallCount
}

func TestDeleteLocalTemplate(t *testing.T) {
	ctx := context.Background()

	mockHook := &mockCDPHook{}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceCubeRunTemplate, mockHook)

	tests := []struct {
		name              string
		template          *templatetypes.LocalRunTemplate
		preDeleteError    error
		storeDeleteError  error
		expectError       bool
		expectInMap       bool
		expectStoreDelete bool
	}{
		{
			name: "Success - template deleted",
			template: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: "template-001",
					Name:       "test-template",
					Namespace:  "default",
				},
			},
			preDeleteError:    nil,
			storeDeleteError:  nil,
			expectError:       false,
			expectInMap:       false,
			expectStoreDelete: true,
		},
		{
			name: "Error - PreDelete fails",
			template: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: "template-002",
					Name:       "test-template-2",
					Namespace:  "default",
				},
			},
			preDeleteError:    errors.New("pre delete failed"),
			storeDeleteError:  nil,
			expectError:       true,
			expectInMap:       false,
			expectStoreDelete: false,
		},
		{
			name: "Error - Store delete fails",
			template: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: "template-003",
					Name:       "test-template-3",
					Namespace:  "default",
				},
			},
			preDeleteError:    nil,
			storeDeleteError:  errors.New("store delete failed"),
			expectError:       true,
			expectInMap:       false,
			expectStoreDelete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			mockHook.Reset()
			mockHook.preDeleteErr = tt.preDeleteError

			store := newMockLocalTemplateStore()
			store.templates[tt.template.TemplateID] = tt.template
			store.deleteErr = tt.storeDeleteError

			testManager := &testLocalCubeRunTemplateManager{
				mockStore:         store,
				unusedTemplateMap: make(map[string]*unusedTemplate),
			}

			testManager.unusedTemplateMap[tt.template.TemplateID] = &unusedTemplate{
				localTemplate: tt.template,
				detectedTime:  time.Now(),
			}

			err := testManager.deleteLocalTemplate(ctx, tt.template)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			testManager.lock.RLock()
			_, exists := testManager.unusedTemplateMap[tt.template.TemplateID]
			testManager.lock.RUnlock()
			assert.Equal(t, tt.expectInMap, exists, "unusedTemplateMap should not contain deleted template")

			assert.Equal(t, 1, mockHook.GetCallCount(), "PreDelete should be called once")
			assert.Equal(t, tt.template.TemplateID, mockHook.GetLastID(), "PreDelete should be called with correct template ID")

			if tt.expectStoreDelete {
				assert.Equal(t, 1, store.GetDeleteCallCount(), "Store.Delete should be called")
				if !tt.expectError || tt.storeDeleteError != nil {

					_, err := store.Get(tt.template.TemplateID)
					if tt.storeDeleteError == nil {
						assert.Error(t, err, "Template should be deleted from store")
					}
				}
			} else {
				assert.Equal(t, 0, store.GetDeleteCallCount(), "Store.Delete should not be called")
			}
		})
	}
}

func TestUnusedTemplateMap_ConcurrentAccess(t *testing.T) {
	manager := &localCubeRunTemplateManager{
		unusedTemplateMap: make(map[string]*unusedTemplate),
	}

	var wg sync.WaitGroup
	numGoroutines := 100

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			templateID := fmt.Sprintf("template-%d", id)
			template := &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: templateID,
				},
			}

			manager.lock.Lock()
			manager.unusedTemplateMap[templateID] = &unusedTemplate{
				localTemplate: template,
				detectedTime:  time.Now(),
			}
			manager.lock.Unlock()
		}(i)
	}

	wg.Wait()

	manager.lock.RLock()
	count := len(manager.unusedTemplateMap)
	manager.lock.RUnlock()

	assert.Equal(t, numGoroutines, count, "All goroutines should have written to the map")

	for i := 0; i < numGoroutines/2; i++ {
		wg.Add(2)

		go func(id int) {
			defer wg.Done()

			templateID := fmt.Sprintf("template-%d", id)
			manager.lock.RLock()
			_, exists := manager.unusedTemplateMap[templateID]
			manager.lock.RUnlock()

			_ = exists
		}(i)

		go func(id int) {
			defer wg.Done()

			templateID := fmt.Sprintf("template-%d", id)
			manager.lock.Lock()
			delete(manager.unusedTemplateMap, templateID)
			manager.lock.Unlock()
		}(i)
	}

	wg.Wait()

	manager.lock.RLock()
	finalCount := len(manager.unusedTemplateMap)
	manager.lock.RUnlock()

	assert.Less(t, finalCount, numGoroutines, "Some templates should have been deleted")
	assert.GreaterOrEqual(t, finalCount, numGoroutines/2, "At least half should remain")
}

func TestUnusedTemplateMap_Initialization(t *testing.T) {
	manager := &localCubeRunTemplateManager{}

	manager.lock.RLock()
	assert.Nil(t, manager.unusedTemplateMap)
	manager.lock.RUnlock()

	manager.lock.Lock()
	if manager.unusedTemplateMap == nil {
		manager.unusedTemplateMap = make(map[string]*unusedTemplate)
	}
	manager.lock.Unlock()

	manager.lock.RLock()
	assert.NotNil(t, manager.unusedTemplateMap)
	assert.Equal(t, 0, len(manager.unusedTemplateMap))
	manager.lock.RUnlock()

	manager.lock.Lock()
	manager.unusedTemplateMap["test"] = &unusedTemplate{
		localTemplate: &templatetypes.LocalRunTemplate{
			DistributionReference: templatetypes.DistributionReference{
				TemplateID: "test",
			},
		},
		detectedTime: time.Now(),
	}
	manager.lock.Unlock()

	manager.lock.RLock()
	assert.Equal(t, 1, len(manager.unusedTemplateMap))
	manager.lock.RUnlock()
}

func TestUnusedTemplate_ExpiredDetection(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name          string
		detectedTime  time.Time
		maxDuration   time.Duration
		expectExpired bool
	}{
		{
			name:          "Not expired - just detected",
			detectedTime:  now,
			maxDuration:   48 * time.Hour,
			expectExpired: false,
		},
		{
			name:          "Not expired - 1 day old",
			detectedTime:  now.Add(-24 * time.Hour),
			maxDuration:   48 * time.Hour,
			expectExpired: false,
		},
		{
			name:          "Expired - 3 days old",
			detectedTime:  now.Add(-72 * time.Hour),
			maxDuration:   48 * time.Hour,
			expectExpired: true,
		},
		{
			name:          "Expired - exactly at threshold",
			detectedTime:  now.Add(-48*time.Hour - time.Second),
			maxDuration:   48 * time.Hour,
			expectExpired: true,
		},
		{
			name:          "Not expired - just before threshold",
			detectedTime:  now.Add(-48*time.Hour + time.Second),
			maxDuration:   48 * time.Hour,
			expectExpired: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unused := &unusedTemplate{
				localTemplate: &templatetypes.LocalRunTemplate{
					DistributionReference: templatetypes.DistributionReference{
						TemplateID: "template-001",
					},
				},
				detectedTime: tt.detectedTime,
			}

			isExpired := now.Sub(unused.detectedTime) > tt.maxDuration
			assert.Equal(t, tt.expectExpired, isExpired, "Expiration detection should be correct")
		})
	}
}

func TestUnusedTemplate_FirstDetection(t *testing.T) {
	now := time.Now()
	manager := &localCubeRunTemplateManager{
		unusedTemplateMap:         make(map[string]*unusedTemplate),
		maxUnusedTemplateDuration: 48 * time.Hour,
	}

	templateID := "template-001"
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID: templateID,
		},
	}

	manager.lock.Lock()
	if _, ok := manager.unusedTemplateMap[templateID]; !ok {
		manager.unusedTemplateMap[templateID] = &unusedTemplate{
			localTemplate: template,
			detectedTime:  now,
		}
	}
	manager.lock.Unlock()

	manager.lock.RLock()
	entry, exists := manager.unusedTemplateMap[templateID]
	manager.lock.RUnlock()

	assert.True(t, exists, "Template should be in unusedTemplateMap")
	assert.Equal(t, templateID, entry.localTemplate.TemplateID)
	assert.WithinDuration(t, now, entry.detectedTime, time.Second)
}

func TestUnusedTemplate_SecondDetection(t *testing.T) {
	now := time.Now()
	manager := &localCubeRunTemplateManager{
		unusedTemplateMap:         make(map[string]*unusedTemplate),
		maxUnusedTemplateDuration: 48 * time.Hour,
	}

	templateID := "template-001"
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID: templateID,
		},
	}

	firstDetectionTime := now.Add(-24 * time.Hour)
	manager.unusedTemplateMap[templateID] = &unusedTemplate{
		localTemplate: template,
		detectedTime:  firstDetectionTime,
	}

	shouldRemove := false
	manager.lock.Lock()
	if v, ok := manager.unusedTemplateMap[templateID]; ok {
		if now.Sub(v.detectedTime) > manager.maxUnusedTemplateDuration {
			shouldRemove = true
		}
	}
	manager.lock.Unlock()

	assert.False(t, shouldRemove, "Should not remove template after only 1 day")

	manager.lock.RLock()
	entry := manager.unusedTemplateMap[templateID]
	manager.lock.RUnlock()

	assert.Equal(t, firstDetectionTime, entry.detectedTime, "Detection time should not change")
}

func TestUnusedTemplate_ThirdDetection(t *testing.T) {
	now := time.Now()
	manager := &localCubeRunTemplateManager{
		unusedTemplateMap:         make(map[string]*unusedTemplate),
		maxUnusedTemplateDuration: 48 * time.Hour,
	}

	templateID := "template-001"
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID: templateID,
		},
	}

	firstDetectionTime := now.Add(-72 * time.Hour)
	manager.unusedTemplateMap[templateID] = &unusedTemplate{
		localTemplate: template,
		detectedTime:  firstDetectionTime,
	}

	shouldRemove := false
	manager.lock.Lock()
	if v, ok := manager.unusedTemplateMap[templateID]; ok {
		if now.Sub(v.detectedTime) > manager.maxUnusedTemplateDuration {
			shouldRemove = true
		}
	}
	manager.lock.Unlock()

	assert.True(t, shouldRemove, "Should remove template after 3 days")
}

func TestDeleteLocalTemplate_RemovesFromUnusedMap(t *testing.T) {
	ctx := context.Background()
	mockHook := &mockCDPHook{}
	cdp.RegisterDeleteProtectionHook(cdp.ResourceCubeRunTemplate, mockHook)

	tests := []struct {
		name           string
		preDeleteError error
		storeError     error
	}{
		{
			name:           "Success case",
			preDeleteError: nil,
			storeError:     nil,
		},
		{
			name:           "PreDelete fails",
			preDeleteError: errors.New("predel error"),
			storeError:     nil,
		},
		{
			name:           "Store delete fails",
			preDeleteError: nil,
			storeError:     errors.New("store error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockHook.Reset()
			mockHook.preDeleteErr = tt.preDeleteError

			store := newMockLocalTemplateStore()
			store.deleteErr = tt.storeError

			template := &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: "test-template",
				},
			}

			testManager := &testLocalCubeRunTemplateManager{
				mockStore:         store,
				unusedTemplateMap: make(map[string]*unusedTemplate),
			}

			testManager.unusedTemplateMap[template.TemplateID] = &unusedTemplate{
				localTemplate: template,
				detectedTime:  time.Now(),
			}

			_ = testManager.deleteLocalTemplate(ctx, template)

			testManager.lock.RLock()
			_, exists := testManager.unusedTemplateMap[template.TemplateID]
			testManager.lock.RUnlock()

			assert.False(t, exists, "Template should always be removed from unusedTemplateMap")
		})
	}
}
