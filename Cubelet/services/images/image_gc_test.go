// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	criimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	criimagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func makeCubeBox(image string) *cubebox.CubeSandbox {
	return &cubebox.CubeSandbox{
		Id:        uuid.New().String(),
		Namespace: "test",
		Containers: []*cubebox.Container{
			{
				Id:    "c1",
				Image: image,
			},
			{
				Id:    "c2",
				Image: image,
			},
		},
	}
}

func makeImage(id, ref string, size int64, created time.Time) criimagestore.Image {
	img := criimagestore.Image{
		ID:         id,
		References: []string{ref},
		Size:       size,
		ImageSpec: imagespec.Image{
			Created: &created,
		},
	}
	return img
}

const (
	testImageId1 = "sha256:984b8977727b8942903c11d40d7a4a30dc00bf2be15a4c7203973b4513662057"
	testImageId2 = "sha256:1717c713d3b2161db30cd026ceffdb9c238fe876f6959bf62caff9c768fb47ef"
	testImageId3 = "sha256:f17acd1eb3df19311600ac389db57fd0347f200a5cd71b0f1aa526f8ebfba27d"

	testImage1 = "cube.com/test:1"
	testImage2 = "cube.com/test:2"
	testImage3 = "cube.com/test:3"
)

func makeDeviceIdleFunc(idle uint) func(path string) (uint64, uint64, error) {
	return func(path string) (uint64, uint64, error) {
		return uint64(idle), 0, nil
	}
}
func TestUnusedImageNotTooOld(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	now := time.Now().Add(-time.Hour)
	testImages := []criimagestore.Image{
		makeImage(testImageId1, testImage1, 1024, now),
		makeImage(testImageId2, testImage2, 1024, now),
	}
	mockNamespaces := &mockNamespaces{namespaces: []string{"test", "default"}}

	fakeService := &cubelet.FakeService{}
	client := cubelet.NewFromFakeService(fakeService)

	policy := ImageGCPolicy{
		LeastUnusedTimeInterval: 2 * time.Hour,
	}
	store, err := criimagestore.NewFakeStore(ctx, testImages)
	require.NoError(t, err)
	ctrl := gomock.NewController(t)
	cirimage, _ := criimages.NewTestCRIServiceWithImageStore(store, ctrl)
	m := NewImageGCManager(client, mockNamespaces, cirimage, policy, nil)
	m.getDeviceIdleRatioFunc = makeDeviceIdleFunc(100)

	err = m.GarbageCollect(context.Background())
	assert.NoError(t, err)
	assert.Len(t, fakeService.ImageDeleteEvent, 0)
}

type mockNamespaces struct {
	namespaces []string
}

func (m *mockNamespaces) ListNamespaces(ctx context.Context) ([]string, error) {
	return m.namespaces, nil
}

var _ NamespaceLister = &mockNamespaces{}

func TestDeleteUnusedImageBySize(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	now := time.Now().Add(-time.Hour)
	testImages := []criimagestore.Image{
		makeImage(testImageId1, testImage1, 4096, now),
		makeImage(testImageId2, testImage2, 1024, now),
		makeImage(testImageId3, testImage3, 2048, now),
	}
	mockNamespaces := &mockNamespaces{namespaces: []string{"test", "default"}}

	fakeService := &cubelet.FakeService{}
	client := cubelet.NewFromFakeService(fakeService)
	policy := ImageGCPolicy{
		LeastUnusedTimeInterval: 1 * time.Second,
		MaxDeletionPerCycle:     10,
	}
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	store, err := criimagestore.NewFakeStore(ctx, testImages)
	require.NoError(t, err)

	cirimage, _ := criimages.NewTestCRIServiceWithImageStore(store, ctrl)
	m := NewImageGCManager(client, mockNamespaces, cirimage, policy, nil)
	m.getDeviceIdleRatioFunc = makeDeviceIdleFunc(100)

	err = m.GarbageCollect(context.Background())
	assert.NoError(t, err)
	fmt.Println(fakeService.ImageDeleteEvent)
	assert.Nil(t, fakeService.ImageDeleteEvent)
}

func TestDeleteUnusedImageLimitedByMaxDeletion(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	now := time.Now().Add(-time.Hour)
	testImages := []criimagestore.Image{
		makeImage(testImageId1, testImage1, 4096, now),
		makeImage(testImageId2, testImage2, 1024, now),
		makeImage(testImageId3, testImage3, 2048, now),
	}
	mockNamespaces := &mockNamespaces{namespaces: []string{"test", "default"}}

	fakeService := &cubelet.FakeService{}
	client := cubelet.NewFromFakeService(fakeService)

	policy := ImageGCPolicy{
		LeastUnusedTimeInterval: 1 * time.Second,
		MaxDeletionPerCycle:     1,
	}
	store, err := criimagestore.NewFakeStore(ctx, testImages)
	require.NoError(t, err)
	ctrl := gomock.NewController(t)
	cirimage, _ := criimages.NewTestCRIServiceWithImageStore(store, ctrl)
	m := NewImageGCManager(client, mockNamespaces, cirimage, policy, nil)
	m.getDeviceIdleRatioFunc = makeDeviceIdleFunc(100)

	err = m.GarbageCollect(context.Background())
	assert.NoError(t, err)
	fmt.Println(fakeService.ImageDeleteEvent)
	assert.Nil(t, fakeService.ImageDeleteEvent)
}

func TestDeleteUnusedImageByRecentlyUsed(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	now := time.Now().Add(-time.Hour)
	testImages := []criimagestore.Image{
		makeImage(testImageId1, testImage1, 4096, now),
		makeImage(testImageId2, testImage2, 1024, now),
		makeImage(testImageId3, testImage3, 2048, now),
	}

	fakeService := &cubelet.FakeService{}
	client := cubelet.NewFromFakeService(fakeService)
	mockNamespaces := &mockNamespaces{namespaces: []string{"test", "default"}}

	policy := ImageGCPolicy{
		LeastUnusedTimeInterval:  1 * time.Nanosecond,
		MaxDeletionPerCycle:      10,
		FreeDiskThresholdPercent: 50,
	}

	store, err := criimagestore.NewFakeStore(ctx, testImages)
	require.NoError(t, err)
	ctrl := gomock.NewController(t)
	cirimage, _ := criimages.NewTestCRIServiceWithImageStore(store, ctrl)
	spy := &removeSpyImageSvc{CubeImageSvcInterface: cirimage}
	m := NewImageGCManager(client, mockNamespaces, spy, policy, nil)
	m.getDeviceIdleRatioFunc = makeDeviceIdleFunc(0)

	err = m.GarbageCollect(context.Background())
	assert.NoError(t, err)

	assert.ElementsMatch(t, []string{testImageId1, testImageId2, testImageId3}, spy.removed)
}

type removeSpyImageSvc struct {
	criimages.CubeImageSvcInterface
	removed []string
}

func (s *removeSpyImageSvc) RemoveImage(ctx context.Context, imageSpec *runtime.ImageSpec) error {
	s.removed = append(s.removed, imageSpec.GetImage())
	return s.CubeImageSvcInterface.RemoveImage(ctx, imageSpec)
}

func TestImageUsed(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	now := time.Now().Add(-time.Hour)
	testImages := []criimagestore.Image{
		makeImage(testImageId1, testImage1, 4096, now),
		makeImage(testImageId2, testImage2, 1024, now),
	}
	mockNamespaces := &mockNamespaces{namespaces: []string{"test", "default"}}

	fakeService := &cubelet.FakeService{
		CubeBoxes: []*cubebox.CubeSandbox{
			makeCubeBox(testImageId1),
		},
	}
	client := cubelet.NewFromFakeService(fakeService)

	policy := ImageGCPolicy{
		LeastUnusedTimeInterval: 1 * time.Second,
		MaxDeletionPerCycle:     10,
	}
	store, err := criimagestore.NewFakeStore(ctx, testImages)
	require.NoError(t, err)
	ctrl := gomock.NewController(t)
	cirimage, _ := criimages.NewTestCRIServiceWithImageStore(store, ctrl)
	m := NewImageGCManager(client, mockNamespaces, cirimage, policy, nil)
	m.getDeviceIdleRatioFunc = makeDeviceIdleFunc(100)

	err = m.GarbageCollect(context.Background())
	assert.NoError(t, err)

	assert.Nil(t, fakeService.ImageDeleteEvent)
}

func TestNoDeleteImageCreatedLongAgoButRefRecently(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	now := time.Now().Add(-time.Hour)
	testImages := []criimagestore.Image{
		makeImage(testImageId1, testImage1, 4096, now.Add(-24*time.Hour)),
	}
	mockNamespaces := &mockNamespaces{namespaces: []string{"test", "default"}}

	fakeService := &cubelet.FakeService{
		CubeBoxes: []*cubebox.CubeSandbox{
			makeCubeBox(testImageId1),
		},
	}
	client := cubelet.NewFromFakeService(fakeService)

	policy := ImageGCPolicy{
		LeastUnusedTimeInterval: 1 * time.Second,
		MaxDeletionPerCycle:     10,
	}
	store, err := criimagestore.NewFakeStore(ctx, testImages)
	require.NoError(t, err)
	ctrl := gomock.NewController(t)
	cirimage, _ := criimages.NewTestCRIServiceWithImageStore(store, ctrl)
	m := NewImageGCManager(client, mockNamespaces, cirimage, policy, nil)
	m.getDeviceIdleRatioFunc = makeDeviceIdleFunc(100)

	err = m.GarbageCollect(context.Background())
	assert.NoError(t, err)

	assert.Len(t, fakeService.ImageDeleteEvent, 0)
}
