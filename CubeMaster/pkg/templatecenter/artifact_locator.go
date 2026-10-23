package templatecenter

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	images "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// ArtifactObjectKey resolves the durable object identity. A recorded key is
// authoritative; only historical records without one use the EXT4 convention.
func ArtifactObjectKey(artifact *models.RootfsArtifact) string {
	if artifact == nil {
		return ""
	}
	if artifact.ObjectKey != "" {
		return artifact.ObjectKey
	}
	filename, err := images.RootfsFilename(artifact.ArtifactID, artifact.RootfsFormat)
	if err != nil {
		return ""
	}
	return filename
}

// NewArtifactObjectKey computes a key only when publishing a new artifact.
func NewArtifactObjectKey(prefix, artifactID, format string) (string, error) {
	filename, err := images.RootfsFilename(artifactID, format)
	if err != nil {
		return "", err
	}
	return blobstore.JoinKey(prefix, filename), nil
}
