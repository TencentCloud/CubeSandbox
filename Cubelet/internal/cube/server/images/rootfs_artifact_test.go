package images

import (
	"bytes"
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/nodedistribution/distribution"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

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
