package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/nodedistribution/distribution"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestPullImageDownloadsArtifactFromSandboxAnnotation(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	content := bytes.Repeat([]byte("rootfs"), 1024)
	sum := sha256.Sum256(content)
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if r.Header.Get("X-Cube-Artifact-Token") != "test-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()
	writeImageTestFile(t, pmem.GetSharedKernelFilePath(), bytes.Repeat([]byte("kernel"), 1024))
	spec := &cubeimages.ImageSpec{Image: "rfs-annotation", StorageMedia: "ext4", Annotations: map[string]string{
		constants.MasterAnnotationInstanceType:         "cubebox",
		constants.MasterAnnotationRootfsArtifactURL:    server.URL,
		constants.MasterAnnotationRootfsArtifactToken:  "test-token",
		constants.MasterAnnotationRootfsArtifactSHA256: hex.EncodeToString(sum[:]),
	}}
	wire, err := json.Marshal(spec)
	require.NoError(t, err)
	service := &CubeImageService{client: &containerd.Client{}, imageRemoveLock: &sync.RWMutex{}}
	// An existing lease avoids a daemon dependency; the ImageSpec is only in the annotation.
	ctx := leases.WithLease(context.Background(), "test-lease")
	name, err := service.PullImage(ctx, spec.Image, nil, &runtime.PodSandboxConfig{Annotations: map[string]string{
		constants.LabelContainerCubeImageSpec: string(wire),
	}})
	require.ErrorIs(t, err, ErrSkip)
	require.Equal(t, spec.Image, name)
	path, err := pmem.GetRootfsFilePath("cubebox", spec.Image, "ext4")
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, got)
	require.Equal(t, int32(1), downloads.Load())
}

func TestImageDistributionHandlerUsesArtifactMetadata(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	template := &templatetypes.TemplateImage{Name: "test", Image: "rfs-distribution", StorageMedia: "ext4"}
	path, err := pmem.GetRootfsFilePath("cubebox", template.Image, template.StorageMedia)
	require.NoError(t, err)
	content := bytes.Repeat([]byte("rootfs"), 1024)
	writeImageTestFile(t, path, content)
	writeImageTestFile(t, pmem.GetSharedKernelFilePath(), bytes.Repeat([]byte("kernel"), 1024))
	ctx := namespaces.WithNamespace(context.Background(), "artifact-tests")
	task := distribution.NewSubTaskDefine(ctx, "task-1", template.Name, distribution.ResourceTaskTypeImage).ToImageTask(template)
	// A filesystem artifact must finish pre-distribution without any containerd client.
	handler := &imageDistributionHandler{}
	status, err := handler.Handle(ctx, task)
	require.NoError(t, err)
	require.Equal(t, distribution.TaskStatus_SUCCESS, status.GetStatus())
	local, ok := status.GetExternObj().(*templatetypes.LocalDistributionImage)
	require.True(t, ok)
	require.Equal(t, template.Image, local.Image.ID)
	require.Equal(t, int64(len(content)), local.Image.Size)
	require.Equal(t, "ext4", local.Image.MediaType)
	require.Equal(t, "artifact-tests", local.Image.Namespace)
}
