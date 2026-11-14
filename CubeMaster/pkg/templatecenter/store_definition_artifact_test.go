package templatecenter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newDefinitionArtifactTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

// requestOnArtifact mimics the stored create request generateTemplateCreateRequest
// produces: the artifact id is recorded on the request and on the image.
func requestOnArtifact(artifactID string) *sandboxtypes.CreateCubeSandboxReq {
	return &sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{constants.CubeAnnotationRootfsArtifactID: artifactID},
		Containers: []*sandboxtypes.Container{{
			Image: &sandboxtypes.ImageSpec{
				Image: artifactID,
				Annotations: map[string]string{
					constants.CubeAnnotationRootfsArtifactID:     artifactID,
					constants.CubeAnnotationRootfsArtifactSHA256: "sha256-" + artifactID,
				},
			},
		}},
	}
}

func seedDefinition(t *testing.T, db *gorm.DB, templateID, rootfsArtifactID string, req *sandboxtypes.CreateCubeSandboxReq) {
	t.Helper()
	payload, err := json.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:       templateID,
		Status:           StatusFailed,
		Kind:             TemplateKindTemplate,
		RootfsArtifactID: rootfsArtifactID,
		RequestJSON:      string(payload),
	}).Error)
}

func storedDefinitionRequest(t *testing.T, templateID string) (*models.TemplateDefinition, *sandboxtypes.CreateCubeSandboxReq) {
	t.Helper()
	def, err := GetDefinition(context.Background(), templateID)
	require.NoError(t, err)
	req := &sandboxtypes.CreateCubeSandboxReq{}
	require.NoError(t, json.Unmarshal([]byte(def.RequestJSON), req))
	return def, req
}

// Regression for #2063: the first create fails after writing the definition on
// artifact A, a redo rebuilds the template on artifact B. The definition must
// follow B, or sandboxes are created from the reclaimed artifact A.
func TestEnsureTemplateDefinitionMovesExistingDefinitionToRebuiltArtifact(t *testing.T) {
	db := newDefinitionArtifactTestDB(t)
	templateID := "tpl-redo-rebuild"
	seedDefinition(t, db, templateID, "rfs-old", requestOnArtifact("rfs-old"))
	require.NoError(t, setTemplateRequestCache(templateID, requestOnArtifact("rfs-old")))

	created, err := ensureTemplateDefinitionWithOptions(context.Background(), templateID, requestOnArtifact("rfs-new"),
		"cubebox", "v2", definitionCreateOptions{})
	require.NoError(t, err)
	require.False(t, created)

	def, req := storedDefinitionRequest(t, templateID)
	require.Equal(t, "rfs-new", def.RootfsArtifactID)
	require.Equal(t, "rfs-new", rootfsArtifactIDFromCreateRequest(req))
	require.Equal(t, "sha256-rfs-new", req.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactSHA256])
	_, cached := templateDefinitionCache.Get(templateID)
	require.False(t, cached, "stale template request cache must be dropped")
}

// A build that reuses the recorded artifact must not rewrite the definition,
// so annotations added after creation (e.g. the converged envd version) stay.
func TestEnsureTemplateDefinitionKeepsDefinitionOnSameArtifact(t *testing.T) {
	db := newDefinitionArtifactTestDB(t)
	templateID := "tpl-redo-reuse"
	stored := requestOnArtifact("rfs-same")
	stored.Annotations[constants.CubeAnnotationComponentEnvdVersion] = "v1.2.3"
	seedDefinition(t, db, templateID, "rfs-same", stored)

	created, err := ensureTemplateDefinitionWithOptions(context.Background(), templateID, requestOnArtifact("rfs-same"),
		"cubebox", "v2", definitionCreateOptions{})
	require.NoError(t, err)
	require.False(t, created)

	_, req := storedDefinitionRequest(t, templateID)
	require.Equal(t, "v1.2.3", req.Annotations[constants.CubeAnnotationComponentEnvdVersion])
}

// Rows written before rootfs_artifact_id existed only carry the artifact inside
// request_json; that is enough to recognise the same artifact.
func TestEnsureTemplateDefinitionFallsBackToRequestJSONArtifact(t *testing.T) {
	db := newDefinitionArtifactTestDB(t)
	templateID := "tpl-legacy-row"
	stored := requestOnArtifact("rfs-legacy")
	stored.Annotations[constants.CubeAnnotationComponentEnvdVersion] = "v1.2.3"
	seedDefinition(t, db, templateID, "", stored)

	_, err := ensureTemplateDefinitionWithOptions(context.Background(), templateID, requestOnArtifact("rfs-legacy"),
		"cubebox", "v2", definitionCreateOptions{})
	require.NoError(t, err)

	def, req := storedDefinitionRequest(t, templateID)
	require.Equal(t, "", def.RootfsArtifactID)
	require.Equal(t, "v1.2.3", req.Annotations[constants.CubeAnnotationComponentEnvdVersion])
}
