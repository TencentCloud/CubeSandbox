// SPDX-License-Identifier: Apache-2.0

package build

import (
	"os"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestLocalArtifactReferencesMySQL(t *testing.T) {
	dsn := os.Getenv("TEMPLATECENTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("requires isolated MySQL database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	var existing []string
	if err := db.Raw("SHOW TABLES").Scan(&existing).Error; err != nil || len(existing) != 0 {
		t.Fatalf("fixture requires an empty disposable database: tables=%d error=%v", len(existing), err)
	}
	for _, table := range []string{artifactTableName, constants.TemplateImageJobTableName, constants.TemplateDefinitionTableName, constants.TemplateReplicaTableName, constants.SnapshotTableName} {
		if err := db.Exec("CREATE TABLE " + table + " (artifact_id VARCHAR(128), rootfs_artifact_id VARCHAR(128), status VARCHAR(32), deleted_at DATETIME NULL)").Error; err != nil {
			t.Fatal(err)
		}
		createdTable := table
		t.Cleanup(func() {
			if err := db.Exec("DROP TABLE IF EXISTS " + createdTable).Error; err != nil {
				t.Errorf("clean up fixture table %s: %v", createdTable, err)
			}
		})
	}
	d := NewArtifactDeleter(db, nil)
	cases := []struct{ table, column, status string }{
		{artifactTableName, "artifact_id", "DELETING"},
		{constants.TemplateImageJobTableName, "artifact_id", "BUILT"},
		{constants.TemplateImageJobTableName, "artifact_id", "RUNNING"},
		{constants.TemplateDefinitionTableName, "rootfs_artifact_id", "READY"},
		{constants.TemplateReplicaTableName, "artifact_id", "READY"},
		{constants.SnapshotTableName, "rootfs_artifact_id", "READY"},
	}
	for _, c := range cases {
		if err := db.Table(c.table).Create(map[string]any{c.column: "rfs-live", "status": c.status}).Error; err != nil {
			t.Fatal(err)
		}
		live, err := d.localArtifactReferenced(t.Context(), "rfs-live")
		if err != nil || !live {
			t.Fatalf("%s %s not protected: %v", c.table, c.status, err)
		}
		if err := db.Exec("DELETE FROM " + c.table).Error; err != nil {
			t.Fatal(err)
		}
	}
	if live, err := d.localArtifactReferenced(t.Context(), "rfs-absent"); err != nil || live {
		t.Fatalf("absent: live=%v err=%v", live, err)
	}
	if err := db.Exec("DROP TABLE " + constants.SnapshotTableName).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := d.localArtifactReferenced(t.Context(), "rfs-absent"); err == nil {
		t.Fatal("schema/query error ignored")
	}
}
