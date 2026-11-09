// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConf writes a volume-s3.conf and points Load at it.
func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return path
}

const minimalConf = `
ACCESS_KEY_ID=ak
SECRET_ACCESS_KEY=sk
BUCKET=cube-volumes
ENDPOINT=https://s3.example.com
`

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != DefaultRegion {
		t.Errorf("Region = %q, want %q", cfg.Region, DefaultRegion)
	}
	if cfg.LockDir != DefaultLockDir {
		t.Errorf("LockDir = %q, want %q", cfg.LockDir, DefaultLockDir)
	}
	if cfg.Addressing != AddressingAuto {
		t.Errorf("Addressing = %v, want auto without ADDRESSING_STYLE or s3fs opts", cfg.Addressing)
	}
	if want := "/etc/cube/.passwd-s3fs-volume-cube-volumes"; cfg.PasswdFile != want {
		t.Errorf("PasswdFile = %q, want %q", cfg.PasswdFile, want)
	}
	if len(cfg.S3FSExtraOpts) != 0 {
		t.Errorf("S3FSExtraOpts = %v, want empty", cfg.S3FSExtraOpts)
	}
}

// The config file is shared with shell tooling that quotes multi-option values,
// so quotes must not leak into the s3fs argument list.
func TestLoadStripsQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{"single", `S3FS_EXTRA_OPTS='-ouse_path_request_style'`, []string{"-ouse_path_request_style"}},
		{"double", `S3FS_EXTRA_OPTS="-ouse_path_request_style -odbglevel=info"`,
			[]string{"-ouse_path_request_style", "-odbglevel=info"}},
		{"bare", `S3FS_EXTRA_OPTS=-onomultipart`, []string{"-onomultipart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConf(t, minimalConf+tc.line+"\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.S3FSExtraOpts) != len(tc.want) {
				t.Fatalf("S3FSExtraOpts = %v, want %v", cfg.S3FSExtraOpts, tc.want)
			}
			for i := range tc.want {
				if cfg.S3FSExtraOpts[i] != tc.want[i] {
					t.Errorf("S3FSExtraOpts[%d] = %q, want %q", i, cfg.S3FSExtraOpts[i], tc.want[i])
				}
			}
		})
	}
}

// The control-plane client must address the endpoint the same way s3fs does,
// otherwise MinIO and dotted bucket names break on one side only.
func TestLoadAddressing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Addressing
	}{
		{"explicit path", minimalConf + "ADDRESSING_STYLE=path\n", AddressingPath},
		{"from s3fs opts", minimalConf + "S3FS_EXTRA_OPTS='-ouse_path_request_style'\n", AddressingPath},
		{"virtual host", minimalConf + "ADDRESSING_STYLE=virtual\n", AddressingVirtual},
		{"explicit auto", minimalConf + "ADDRESSING_STYLE=auto\n", AddressingAuto},
		{"unset", minimalConf, AddressingAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConf(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Addressing != tc.want {
				t.Errorf("Addressing = %v, want %v", cfg.Addressing, tc.want)
			}
		})
	}
}

// An explicit virtual style in the plugin config cannot be silently overridden
// by a path-style s3fs flag. Unknown values are rejected instead of guessed.
func TestLoadAddressingRejectsConflictsAndUnknownValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"virtual with path flag",
			minimalConf + "ADDRESSING_STYLE=virtual\nS3FS_EXTRA_OPTS='-ouse_path_request_style'\n",
			"conflicts",
		},
		{
			"explicit auto with path flag",
			minimalConf + "ADDRESSING_STYLE=auto\nS3FS_EXTRA_OPTS='-ouse_path_request_style'\n",
			"conflicts",
		},
		{"unknown style", minimalConf + "ADDRESSING_STYLE=host\n", "must be auto, path or virtual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConf(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadRegionOverride(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"REGION=ap-guangzhou\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != "ap-guangzhou" {
		t.Errorf("Region = %q, want ap-guangzhou", cfg.Region)
	}
}

// An empty REGION must not override the default, since the shell config often
// carries a commented-out or blank assignment.
func TestLoadEmptyRegionKeepsDefault(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"REGION=\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != DefaultRegion {
		t.Errorf("Region = %q, want %q", cfg.Region, DefaultRegion)
	}
}

func TestLoadIgnoresCommentsAndExport(t *testing.T) {
	body := `
# ACCESS_KEY_ID=ignored
export ACCESS_KEY_ID=ak
SECRET_ACCESS_KEY=sk
BUCKET=b
ENDPOINT=http://minio:9000

# trailing comment
`
	cfg, err := Load(writeConf(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AccessKeyID != "ak" {
		t.Errorf("AccessKeyID = %q, want ak", cfg.AccessKeyID)
	}
}

// An inline trailing `# comment` after a value must be dropped, matching the
// shell `source` semantics the old plugin relied on. Without this,
// `S3FS_EXTRA_OPTS=-ouse_path_request_style  # MinIO` would pass `#` and
// `MinIO` as spurious s3fs arguments.
func TestLoadStripsInlineComment(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"S3FS_EXTRA_OPTS=-ouse_path_request_style  # MinIO\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"-ouse_path_request_style"}
	if len(cfg.S3FSExtraOpts) != len(want) {
		t.Fatalf("S3FSExtraOpts = %v, want %v", cfg.S3FSExtraOpts, want)
	}
	if cfg.S3FSExtraOpts[0] != want[0] {
		t.Errorf("S3FSExtraOpts[0] = %q, want %q", cfg.S3FSExtraOpts[0], want[0])
	}
}

func TestLoadMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no access key", "SECRET_ACCESS_KEY=sk\nBUCKET=b\nENDPOINT=http://e\n"},
		{"no secret", "ACCESS_KEY_ID=ak\nBUCKET=b\nENDPOINT=http://e\n"},
		{"no endpoint", "ACCESS_KEY_ID=ak\nSECRET_ACCESS_KEY=sk\nBUCKET=b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConf(t, tc.body)); err == nil {
				t.Fatal("Load succeeded, want error")
			}
		})
	}
}

// A deployment that hands every volume its own cred_ref does not need a
// deployment-wide key pair or bucket. Both can be supplied per volume.
func TestLoadDynamicOnlyDeployment(t *testing.T) {
	cfg, err := Load(writeConf(t, "ENDPOINT=http://minio:9000\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bucket != "" {
		t.Errorf("Bucket = %q, want empty", cfg.Bucket)
	}
	if cfg.PasswdFile != "" {
		t.Errorf("PasswdFile = %q, want empty without a default bucket", cfg.PasswdFile)
	}
	if cfg.CredentialsDir != DefaultCredentialsDir {
		t.Errorf("CredentialsDir = %q, want %q", cfg.CredentialsDir, DefaultCredentialsDir)
	}
}

// The instance role is an explicit opt-in: CREDENTIALS=instance_role with no
// keys and an AWS endpoint.
func TestLoadInstanceRole(t *testing.T) {
	for _, endpoint := range []string{
		"https://s3.ap-northeast-1.amazonaws.com",
		"s3.cn-north-1.amazonaws.com.cn",
		"https://bucket.vpce-0123-abcd.s3.ap-northeast-1.vpce.amazonaws.com",
	} {
		cfg, err := Load(writeConf(t, "CREDENTIALS=instance_role\nBUCKET=b\nENDPOINT="+endpoint+"\n"))
		if err != nil {
			t.Fatalf("Load(%s): %v", endpoint, err)
		}
		if !cfg.UseInstanceRole() {
			t.Errorf("UseInstanceRole = false for %s, want true with CREDENTIALS=instance_role", endpoint)
		}
	}

	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UseInstanceRole() {
		t.Error("UseInstanceRole = true, want false when static keys are configured")
	}
}

// Empty keys alone must not select the instance role: a missing, overwritten
// or misspelled key has to fail at load time, not fall through to IMDS. In
// static mode both-empty is allowed (dynamic-only deployments), one key alone
// is always a misconfiguration.
func TestLoadInstanceRoleRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"misspelled key", "ACCESS_KEY=ak\nSECRET_ACCESS_KEY=sk\nBUCKET=b\nENDPOINT=https://s3.ap-northeast-1.amazonaws.com\n", "set both ACCESS_KEY_ID and SECRET_ACCESS_KEY"},
		{"opt-in with keys", "CREDENTIALS=instance_role\n" + minimalConf, "takes no ACCESS_KEY_ID"},
		{"opt-in with secret only", "CREDENTIALS=instance_role\nSECRET_ACCESS_KEY=sk\nBUCKET=b\nENDPOINT=https://s3.us-east-1.amazonaws.com\n", "takes no ACCESS_KEY_ID"},
		{"opt-in on COS", "CREDENTIALS=instance_role\nBUCKET=b\nENDPOINT=https://cos.ap-guangzhou.myqcloud.com\n", "needs an AWS S3 endpoint"},
		{"opt-in on MinIO", "CREDENTIALS=instance_role\nBUCKET=b\nENDPOINT=http://minio:9000\n", "needs an AWS S3 endpoint"},
		{"look-alike host", "CREDENTIALS=instance_role\nBUCKET=b\nENDPOINT=https://s3.amazonaws.com.evil.example\n", "needs an AWS S3 endpoint"},
		{"unknown mode", "CREDENTIALS=iam\nBUCKET=b\nENDPOINT=https://s3.us-east-1.amazonaws.com\n", "is not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConf(t, tc.body))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// UseInstanceRole must not trust validate() alone: a config built without Load
// that carries any key is not instance-role.
func TestUseInstanceRoleNeedsBothKeysEmpty(t *testing.T) {
	for _, c := range []Config{
		{Credentials: CredentialsInstanceRole, SecretAccessKey: "sk"},
		{Credentials: CredentialsInstanceRole, AccessKeyID: "ak"},
		{Credentials: CredentialsStatic},
		{},
	} {
		if c.UseInstanceRole() {
			t.Errorf("UseInstanceRole() = true for %+v, want false", c)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.conf")); err == nil {
		t.Fatal("Load succeeded on missing file, want error")
	}
}

func TestLoadHonoursEnvOverrides(t *testing.T) {
	path := writeConf(t, minimalConf)
	t.Setenv("CUBE_S3_CONFIG", path)
	t.Setenv("CUBE_S3_LOCK_DIR", "/tmp/locks")
	t.Setenv("CUBE_S3_PASSWD_FILE", "/tmp/passwd")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LockDir != "/tmp/locks" {
		t.Errorf("LockDir = %q, want /tmp/locks", cfg.LockDir)
	}
	if cfg.PasswdFile != "/tmp/passwd" {
		t.Errorf("PasswdFile = %q, want /tmp/passwd", cfg.PasswdFile)
	}
}

// s3fs 1.91+ stats the trailing-slash directory object on mount, so the prefix
// create writes and the subdir s3fs mounts must not drift apart.
func TestVolumeKeys(t *testing.T) {
	if got, want := VolumePrefix("vol-1"), "volumes/vol-1/"; got != want {
		t.Errorf("VolumePrefix = %q, want %q", got, want)
	}
	if got, want := VolumeSubdir("vol-1"), "volumes/vol-1"; got != want {
		t.Errorf("VolumeSubdir = %q, want %q", got, want)
	}
}

func TestMountPointUnder(t *testing.T) {
	if got, want := MountPointUnder("/data/cube-shared/volume", "v1"), "/data/cube-shared/volume/s3-v1"; got != want {
		t.Errorf("MountPointUnder = %q, want %q", got, want)
	}
	// Detach records from an older Cubelet carry no base dir.
	if got, want := MountPointUnder("", "v1"), DefaultVolumeBaseDir+"/s3-v1"; got != want {
		t.Errorf("MountPointUnder with empty base = %q, want %q", got, want)
	}
}

// writeCred writes one credential reference file and returns a config that
// looks for references in that directory.
func writeCred(t *testing.T, ref, body string) *Config {
	t.Helper()
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dir := t.TempDir()
	cfg.CredentialsDir = dir
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, ref+CredentialFileSuffix), []byte(body), 0o600); err != nil {
			t.Fatalf("write credential file: %v", err)
		}
	}
	return cfg
}

// Reference files select credentials and optional endpoint/region/addressing.
// Mount options stay global; a reference cannot merge another option layer.
func TestResolveCredentials(t *testing.T) {
	const globalPath = "S3FS_EXTRA_OPTS='-ouse_path_request_style -odbglevel=info'\n"
	for _, tc := range []struct {
		name, global, ref, endpoint, region, options, wantError string
		style                                                   Addressing
		defaults                                                bool
	}{
		{name: "config defaults", defaults: true},
		{name: "reference inherits defaults"},
		{name: "reference overrides connection", ref: "ENDPOINT=https://cos.ap-guangzhou.myqcloud.com\nREGION=ap-guangzhou\nADDRESSING_STYLE=path\n",
			endpoint: "https://cos.ap-guangzhou.myqcloud.com", region: "ap-guangzhou", style: AddressingPath},
		{name: "reference inherits global path", global: globalPath, style: AddressingPath,
			options: "-ouse_path_request_style -odbglevel=info"},
		{name: "virtual overrides global path", global: globalPath, ref: "ADDRESSING_STYLE=virtual\n",
			style: AddressingVirtual, options: "-odbglevel=info"},
		{name: "auto resets global path", global: globalPath, ref: "ADDRESSING_STYLE=auto\n", options: "-odbglevel=info"},
		{name: "path overrides global virtual", global: "ADDRESSING_STYLE=virtual\n", ref: "ADDRESSING_STYLE=path\n", style: AddressingPath},
		{name: "virtual retains bundled global options", global: "S3FS_EXTRA_OPTS='-o use_path_request_style,multipart_size=50'\n",
			ref: "ADDRESSING_STYLE=virtual\n", style: AddressingVirtual, options: "-o multipart_size=50"},
		{name: "unknown style", ref: "ADDRESSING_STYLE=host\n", wantError: "must be auto, path or virtual"},
		{name: "reference mount options unsupported", ref: "S3FS_EXTRA_OPTS='-odbglevel=info'\n", wantError: "cannot set S3FS_EXTRA_OPTS"},
		{name: "reference path flag unsupported", global: "ADDRESSING_STYLE=virtual\n",
			ref: "S3FS_EXTRA_OPTS='-ouse_path_request_style'\n", wantError: "cannot set S3FS_EXTRA_OPTS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConf(t, minimalConf+tc.global))
			if err != nil {
				t.Fatal(err)
			}
			originalOptions := strings.Join(cfg.S3FSExtraOpts, " ")
			ref := "team-a"
			if tc.defaults {
				ref = ""
			} else {
				cfg.CredentialsDir = t.TempDir()
				body := "ACCESS_KEY_ID=AKA\nSECRET_ACCESS_KEY=SKA\n" + tc.ref
				if err := os.WriteFile(filepath.Join(cfg.CredentialsDir, ref+CredentialFileSuffix), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			creds, err := cfg.ResolveCredentials(ref)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			key, secret := "AKA", "SKA"
			if tc.defaults {
				key, secret = "ak", "sk"
			}
			endpoint, region := tc.endpoint, tc.region
			if endpoint == "" {
				endpoint = "https://s3.example.com"
			}
			if region == "" {
				region = DefaultRegion
			}
			if creds.AccessKeyID != key || creds.SecretAccessKey != secret || creds.InstanceRole ||
				creds.Endpoint != endpoint || creds.Region != region || creds.Addressing != tc.style ||
				strings.Join(creds.ExtraOpts, " ") != tc.options {
				t.Errorf("resolved credentials do not match case %q", tc.name)
			}
			if strings.Join(cfg.S3FSExtraOpts, " ") != originalOptions {
				t.Error("reference resolution mutated global options")
			}
		})
	}
}

func TestWithoutPathRequestStyle(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"joined flag", []string{"-ouse_path_request_style", "-odbglevel=info"}, []string{"-odbglevel=info"}},
		{"split flag", []string{"-o", "use_path_request_style", "-odbglevel=info"}, []string{"-odbglevel=info"}},
		{"bundled split flag", []string{"-o", "use_path_request_style,multipart_size=50", "-odbglevel=info"},
			[]string{"-o", "multipart_size=50", "-odbglevel=info"}},
		{"bundled joined flag", []string{"-ouse_path_request_style,retries=5"},
			[]string{"-oretries=5"}},
		{"bundled unrelated options", []string{"-o", "multipart_size=50,retries=5"},
			[]string{"-o", "multipart_size=50,retries=5"}},
		{"unrelated options", []string{"-onomultipart", "-odbglevel=info"}, []string{"-onomultipart", "-odbglevel=info"}},
		{"empty", nil, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WithoutPathRequestStyle(tc.in)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("WithoutPathRequestStyle(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveCredentialsRejects(t *testing.T) {
	cfg := writeCred(t, "incomplete", "ACCESS_KEY_ID=AK\n")

	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{"missing file", "absent", "unreadable"},
		{"incomplete file", "incomplete", "must set ACCESS_KEY_ID"},
		{"path traversal", "../escape", "not a safe file name"},
		{"nested path", "a/b", "not a safe file name"},
		{"dot", ".", "not a safe file name"},
		{"dotdot", "..", "not a safe file name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cfg.ResolveCredentials(tc.ref)
			if err == nil {
				t.Fatal("ResolveCredentials succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A dynamic-only deployment has no fallback keys; a volume without cred_ref
// must fail with an actionable error instead of an empty passwd file.
func TestResolveCredentialsWithoutAnyKeys(t *testing.T) {
	cfg, err := Load(writeConf(t, "ENDPOINT=http://minio:9000\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = cfg.ResolveCredentials("")
	if err == nil || !strings.Contains(err.Error(), "legacy volumes require") || !strings.Contains(err.Error(), "dynamic volumes require cred_ref") {
		t.Fatalf("ResolveCredentials error = %v, want legacy/global credential guidance", err)
	}
}

func TestPasswdFileForVolume(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.PasswdDir = "/run/cube-creds"
	got := cfg.PasswdFileForVolume("vol-abc_1")
	want := "/run/cube-creds/.passwd-s3fs-dynamic-vol-abc_1"
	if got != want {
		t.Errorf("PasswdFileForVolume = %q, want %q", got, want)
	}
}

func TestIsDynamicPasswdFileRequiresExactVolumePath(t *testing.T) {
	cfg := &Config{PasswdDir: filepath.Join(t.TempDir(), "passwd")}
	want := cfg.PasswdFileForVolume("vol-1")
	for _, tc := range []struct {
		name string
		id   string
		path string
		want bool
	}{
		{"exact", "vol-1", want, true},
		{"normalized exact", "vol-1", filepath.Join(filepath.Dir(want), "..", filepath.Base(filepath.Dir(want)), filepath.Base(want)), true},
		{"different volume", "vol-1", cfg.PasswdFileForVolume("vol-2"), false},
		{"legacy shared file", "vol-1", filepath.Join(filepath.Dir(want), ".passwd-s3fs-volume-bucket"), false},
		{"outside directory", "vol-1", filepath.Join(t.TempDir(), filepath.Base(want)), false},
		{"prefix only", "vol-1", filepath.Join(filepath.Dir(want), filepath.Base(want)+"-extra"), false},
		{"invalid volume id", "../vol-1", want, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.IsDynamicPasswdFile(tc.id, tc.path); got != tc.want {
				t.Errorf("IsDynamicPasswdFile(%q, %q) = %v, want %v", tc.id, tc.path, got, tc.want)
			}
		})
	}
}

// A volume ID may equal the default bucket name; the dynamic passwd file must
// still not collide with the legacy per-bucket file.
func TestPasswdFileForVolumeDoesNotCollideWithLegacy(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.PasswdDir = "/etc/cube"
	if dyn, legacy := cfg.PasswdFileForVolume(cfg.Bucket), cfg.PasswdFile; dyn == legacy {
		t.Fatalf("dynamic and legacy passwd files share %q", dyn)
	}
}

// An install that only overrides the legacy passwd file keeps working: the
// per-volume files are written next to it.
func TestPasswdFileForVolumeFollowsLegacyOverride(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.PasswdDir = ""
	cfg.PasswdFile = "/tmp/cube-creds/legacy-passwd"
	got := cfg.PasswdFileForVolume("v1")
	want := "/tmp/cube-creds/.passwd-s3fs-dynamic-v1"
	if got != want {
		t.Errorf("PasswdFileForVolume = %q, want %q", got, want)
	}
}

func TestLoadHonoursCredentialsDirEnv(t *testing.T) {
	t.Setenv("CUBE_S3_CREDENTIALS_DIR", "/etc/cube/refs")
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CredentialsDir != "/etc/cube/refs" {
		t.Errorf("CredentialsDir = %q, want /etc/cube/refs", cfg.CredentialsDir)
	}
}

func TestLoadHonoursCredentialAndPasswdDirKeys(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"CREDENTIALS_DIR=/srv/refs\nPASSWD_DIR=/srv/passwd\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CredentialsDir != "/srv/refs" {
		t.Errorf("CredentialsDir = %q, want /srv/refs", cfg.CredentialsDir)
	}
	if cfg.PasswdDir != "/srv/passwd" {
		t.Errorf("PasswdDir = %q, want /srv/passwd", cfg.PasswdDir)
	}
}

// An environment override wins over the config file, as with the other paths.
func TestCredentialsDirEnvWinsOverFile(t *testing.T) {
	t.Setenv("CUBE_S3_CREDENTIALS_DIR", "/env/refs")
	cfg, err := Load(writeConf(t, minimalConf+"CREDENTIALS_DIR=/srv/refs\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CredentialsDir != "/env/refs" {
		t.Errorf("CredentialsDir = %q, want /env/refs", cfg.CredentialsDir)
	}
}

// PASSWD_DIR wins over the directory of the legacy passwd file, which is only
// a fallback for installs that never set it.
func TestPasswdDirEnvWinsOverLegacyFileDir(t *testing.T) {
	t.Setenv("CUBE_S3_PASSWD_DIR", "/env/passwd")
	cfg, err := Load(writeConf(t, minimalConf+"PASSWD_DIR=/srv/passwd\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PasswdDir != "/env/passwd" {
		t.Errorf("PasswdDir = %q, want /env/passwd", cfg.PasswdDir)
	}
}
