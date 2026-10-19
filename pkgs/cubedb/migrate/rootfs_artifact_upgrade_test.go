package migrate_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
)

// Start from the actual migrations before rootfs metadata was generalized.
// This exercises the upgrade against both engines, including soft-deleted data
// and a partially applied (nontransactional) migration.
func TestRootfsArtifactUpgrade(t *testing.T) {
	for _, dialect := range []string{"mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var env *dbTestEnv
			var db *sql.DB
			if dialect == "mysql" {
				env = newMySQL(t)
				db = openDB(t, env.dsn)
			} else {
				env = newPostgres(t)
				db = openPGDB(t, env.dsn)
			}
			defer env.teardown()
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			files, err := os.ReadDir(filepath.Join("migrations", dialect))
			if err != nil {
				t.Fatal(err)
			}
			old := fstest.MapFS{}
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".sql") || strings.HasPrefix(file.Name(), "20261015120000_") {
					continue
				}
				data, err := os.ReadFile(filepath.Join("migrations", dialect, file.Name()))
				if err != nil {
					t.Fatal(err)
				}
				old[file.Name()] = &fstest.MapFile{Data: data}
			}
			provider, err := goose.NewProvider(goose.Dialect(dialect), db, old, goose.WithAllowOutofOrder(true))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Up(ctx); err != nil {
				t.Fatal(err)
			}
			mustExec(t, ctx, db, "seed live artifact", `INSERT INTO t_cube_rootfs_artifact (artifact_id,template_spec_fingerprint,ext4_path,ext4_sha256,ext4_size_bytes) VALUES ('rfs-live','live','/live.ext4','live-sha',4096)`)
			mustExec(t, ctx, db, "seed deleted artifact", `INSERT INTO t_cube_rootfs_artifact (artifact_id,template_spec_fingerprint,ext4_path,ext4_sha256,ext4_size_bytes,deleted_at) VALUES ('rfs-deleted','deleted','/deleted.ext4','deleted-sha',8192,CURRENT_TIMESTAMP)`)
			mustExec(t, ctx, db, "seed old phase", `INSERT INTO t_cube_template_image_job (job_id,template_id,phase,resume_phase) VALUES ('job-old','tpl-old','BUILDING_EXT4','BUILDING_EXT4')`)
			// Simulate a previous attempt adding one column before interruption.
			mustExec(t, ctx, db, "partial column", `ALTER TABLE t_cube_rootfs_artifact ADD COLUMN rootfs_path varchar(2048) NOT NULL DEFAULT ''`)
			if err := migrate.Run(ctx, db, dialect, nil); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"rfs-live", "rfs-deleted"} {
				var format, path, sha string
				var size int64
				query := `SELECT rootfs_format,rootfs_path,rootfs_sha256,rootfs_size_bytes FROM t_cube_rootfs_artifact WHERE artifact_id = '` + id + `'`
				if err := db.QueryRowContext(ctx, query).Scan(&format, &path, &sha, &size); err != nil {
					t.Fatal(err)
				}
				suffix := "live"
				wantSize := int64(4096)
				if id == "rfs-deleted" {
					suffix = "deleted"
					wantSize = 8192
				}
				if format != "ext4" || path != "/"+suffix+".ext4" || sha != suffix+"-sha" || size != wantSize {
					t.Fatalf("%s: %s %s %s %d", id, format, path, sha, size)
				}
			}
			var phase, resume string
			if err := db.QueryRowContext(ctx, `SELECT phase,resume_phase FROM t_cube_template_image_job WHERE job_id='job-old'`).Scan(&phase, &resume); err != nil {
				t.Fatal(err)
			}
			if phase != "BUILDING_ROOTFS" || resume != "BUILDING_ROOTFS" {
				t.Fatalf("phase=%s resume=%s", phase, resume)
			}
			mustExec(t, ctx, db, "change canonical path", `UPDATE t_cube_rootfs_artifact SET rootfs_path='/canonical.ext4' WHERE artifact_id='rfs-live'`)
			if err := migrate.Run(ctx, db, dialect, nil); err != nil {
				t.Fatal(err)
			}
			var path string
			if err := db.QueryRowContext(ctx, `SELECT rootfs_path FROM t_cube_rootfs_artifact WHERE artifact_id='rfs-live'`).Scan(&path); err != nil || path != "/canonical.ext4" {
				t.Fatalf("repeat migration path=%q err=%v", path, err)
			}
		})
	}
}
