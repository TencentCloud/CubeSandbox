package templatecenter

import (
	"encoding/json"
	"testing"
)

func TestRemoteBuildResultJSONCompatibility(t *testing.T) {
	for _, metadata := range []string{
		`"ext4_path":"/old.ext4","ext4_sha256":"abc","ext4_size_bytes":9007199254740993`,
		`"rootfs_format":"ext4","rootfs_path":"/old.ext4","rootfs_sha256":"abc","rootfs_size_bytes":9007199254740993`,
		`"ext4_path":" /old.ext4 \t","ext4_sha256":" abc \n","ext4_size_bytes":9007199254740993`,
		`"rootfs_format":"ext4","rootfs_path":" /old.ext4 \t","rootfs_sha256":" abc \n","rootfs_size_bytes":9007199254740993`,
		`"rootfs_path":" /old.ext4 ","rootfs_sha256":"abc","rootfs_size_bytes":9007199254740993,"ext4_path":"/old.ext4","ext4_sha256":" abc ","ext4_size_bytes":9007199254740993`,
	} {
		var result RemoteBuildResult
		payload := `{"artifact_id":" rfs-1 ","object_key":"artifacts/custom/image.data","cube_egress_ca_baked":true,` + metadata + `}`
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			t.Fatal(err)
		}
		if result.ArtifactID != "rfs-1" || result.ObjectKey != "artifacts/custom/image.data" || !result.CubeEgressCABaked || result.RootfsFormat != "ext4" || result.RootfsPath != "/old.ext4" || result.RootfsSHA256 != "abc" || result.RootfsSizeBytes != 9007199254740993 {
			t.Fatalf("decoded result=%+v", result)
		}
	}
	for _, payload := range []string{
		`{"rootfs_path":"/new.ext4","ext4_path":"/old.ext4"}`,
		`{"rootfs_path":" /new.ext4 ","ext4_path":" /old.ext4 "}`,
		`{"rootfs_format":"unknown"}`,
		`{"rootfs_size_bytes":"invalid"}`,
	} {
		var result RemoteBuildResult
		if err := json.Unmarshal([]byte(payload), &result); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}
