// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package config loads the S3 Volume plugin configuration file and the
// per-volume credential reference files.
//
// The file uses shell-style KEY=VALUE lines so a single volume-s3.conf can be
// shared with tooling that sources it, but this package parses it rather than
// executing it.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
)

// DefaultVolumeBaseDir mirrors Cubelet's default parent directory, used when
// --volume-base-dir is empty (older Cubelet).
const DefaultVolumeBaseDir = "/data/cube-shared/volume"

// DefaultRegion is the SigV4 signing region used when REGION is unset.
const DefaultRegion = "us-east-1"

// DefaultLockDir holds the per-volume attach/detach lock files.
const DefaultLockDir = "/run/cube-volume-s3"

// DefaultCredentialsDir holds referenced credential files, one per cred_ref.
const DefaultCredentialsDir = "/etc/cube/s3-credentials"

// DefaultPasswdDir holds the s3fs credential files written for dynamic volumes.
const DefaultPasswdDir = "/etc/cube"

// ConfigFileName is looked up next to the plugin executable.
const ConfigFileName = "volume-s3.conf"

// CredentialFileSuffix is appended to a cred_ref to find its file.
const CredentialFileSuffix = ".conf"

// Values of CREDENTIALS, which selects where S3 credentials come from.
const (
	// CredentialsStatic uses ACCESS_KEY_ID / SECRET_ACCESS_KEY. The default.
	CredentialsStatic = "static"
	// CredentialsInstanceRole uses the EC2 instance role through IMDS; both
	// keys must be left empty.
	CredentialsInstanceRole = "instance_role"
)

// Config holds S3 credentials and mount settings for the plugin.
type Config struct {
	// Credentials is CredentialsStatic or CredentialsInstanceRole.
	Credentials string

	// The access key pair is optional: a deployment that always uses
	// per-volume cred_ref files may leave both empty.
	AccessKeyID     string
	SecretAccessKey string

	// Bucket is the legacy/default bucket. Optional: dynamic volumes may
	// name their own bucket, but volumes created without private_data, and
	// dynamic volumes that omit bucket, fall back to this value.
	Bucket   string
	Endpoint string
	Region   string

	// S3FSExtraOpts are extra s3fs flags, whitespace-separated in the file.
	S3FSExtraOpts []string

	// Addressing is how the bucket is placed in the request URL. It is
	// derived from ADDRESSING_STYLE, or from a path-style s3fs option when
	// ADDRESSING_STYLE is unset.
	Addressing Addressing

	// PasswdFile is the s3fs credential file path for legacy volumes.
	PasswdFile string

	// PasswdDir holds the per-volume s3fs credential files written for
	// dynamic volumes.
	PasswdDir string

	// CredentialsDir holds <cred_ref>.conf files.
	CredentialsDir string

	// LockDir holds per-volume flock files.
	LockDir string
}

// Addressing selects how the bucket name is placed in the request URL.
type Addressing int

const (
	// AddressingAuto lets the client and endpoint negotiate. This is the
	// default and preserves the behaviour of existing deployments.
	AddressingAuto Addressing = iota
	// AddressingPath uses https://endpoint/bucket/key (path-style).
	AddressingPath
	// AddressingVirtual uses https://bucket.endpoint/key (virtual-hosted).
	AddressingVirtual
)

func (a Addressing) String() string {
	switch a {
	case AddressingPath:
		return "path"
	case AddressingVirtual:
		return "virtual"
	default:
		return "auto"
	}
}

// Credentials is the resolved credential set for one volume. Exactly one of
// the static key pair or InstanceRole is used.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string
	Region          string
	Addressing      Addressing
	ExtraOpts       []string
	InstanceRole    bool
}

// fileValues is the raw KEY=VALUE content shared by volume-s3.conf and
// credential reference files.
type fileValues struct {
	Credentials     string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Endpoint        string
	Region          string
	S3FSExtraOpts   []string
	AddressingStyle string
	CredentialsDir  string
	PasswdDir       string
}

// Load reads KEY=VALUE lines from path. When path is empty it falls back to
// $CUBE_S3_CONFIG, then to volume-s3.conf next to the executable.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("CUBE_S3_CONFIG")
	}
	if path == "" {
		dir, err := executableDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, ConfigFileName)
	}

	values, err := parseFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Credentials:     CredentialsStatic,
		AccessKeyID:     values.AccessKeyID,
		SecretAccessKey: values.SecretAccessKey,
		Bucket:          values.Bucket,
		Endpoint:        values.Endpoint,
		Region:          DefaultRegion,
		S3FSExtraOpts:   values.S3FSExtraOpts,
		LockDir:         DefaultLockDir,
		CredentialsDir:  DefaultCredentialsDir,
		PasswdDir:       DefaultPasswdDir,
	}
	if values.Credentials != "" {
		cfg.Credentials = values.Credentials
	}
	if values.Region != "" {
		cfg.Region = values.Region
	}

	if values.CredentialsDir != "" {
		cfg.CredentialsDir = values.CredentialsDir
	}
	// An explicit PASSWD_DIR (file or env) must win over the legacy passwd
	// file's directory, so remember that it was set.
	passwdDirSet := values.PasswdDir != ""
	if passwdDirSet {
		cfg.PasswdDir = values.PasswdDir
	}
	if v := os.Getenv("CUBE_S3_LOCK_DIR"); v != "" {
		cfg.LockDir = v
	}
	if v := os.Getenv("CUBE_S3_CREDENTIALS_DIR"); v != "" {
		cfg.CredentialsDir = v
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// MinIO and other path-style endpoints break virtual-hosted-style
	// addressing, as do bucket names containing dots. s3fs is told via
	// S3FS_EXTRA_OPTS; mirror that choice for the control-plane client so both
	// sides address the same way. A path-style flag is only a fallback: an
	// explicit virtual or auto style cannot coexist with one, because s3fs would
	// still be forced to path-style while the operator asked for something else.
	addressing, explicit, err := parseAddressing(values.AddressingStyle)
	if err != nil {
		return nil, err
	}
	if HasPathRequestStyle(cfg.S3FSExtraOpts) {
		switch {
		case addressing == AddressingVirtual:
			return nil, errors.New("config: ADDRESSING_STYLE=virtual conflicts with S3FS_EXTRA_OPTS use_path_request_style; remove the s3fs flag")
		case addressing == AddressingAuto && explicit:
			return nil, errors.New("config: ADDRESSING_STYLE=auto conflicts with S3FS_EXTRA_OPTS use_path_request_style; set ADDRESSING_STYLE=path or remove the s3fs flag")
		case !explicit:
			addressing = AddressingPath
		}
	}
	cfg.Addressing = addressing

	if v := os.Getenv("CUBE_S3_PASSWD_FILE"); v != "" {
		cfg.PasswdFile = v
	} else if cfg.Bucket != "" {
		cfg.PasswdFile = filepath.Join(DefaultPasswdDir, ".passwd-s3fs-volume-"+cfg.Bucket)
	}
	if v := os.Getenv("CUBE_S3_PASSWD_DIR"); v != "" {
		cfg.PasswdDir = v
		passwdDirSet = true
	} else if !passwdDirSet && cfg.PasswdFile != "" {
		// Keep tests and custom installs that only override the legacy path
		// working: dynamic passwd files belong next to it.
		cfg.PasswdDir = filepath.Dir(cfg.PasswdFile)
	}
	return cfg, nil
}

// parseFile reads the shell-style KEY=VALUE file.
func parseFile(path string) (fileValues, error) {
	var values fileValues

	f, err := os.Open(path)
	if err != nil {
		return values, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// Strip an unquoted inline comment: volume-s3.conf values (keys,
		// endpoints, bucket names) never contain '#', so cutting at the first
		// '#' is safe and matches the shell `source` behaviour the old plugin
		// relied on. A quoted '#' inside a value is not supported — keep the
		// config simple.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		val = unquote(strings.TrimSpace(val))

		switch key {
		case "CREDENTIALS":
			values.Credentials = val
		case "ACCESS_KEY_ID":
			values.AccessKeyID = val
		case "SECRET_ACCESS_KEY":
			values.SecretAccessKey = val
		case "BUCKET":
			values.Bucket = val
		case "ENDPOINT":
			values.Endpoint = val
		case "REGION":
			values.Region = val
		case "S3FS_EXTRA_OPTS":
			values.S3FSExtraOpts = strings.Fields(val)
		case "ADDRESSING_STYLE":
			values.AddressingStyle = val
		case "CREDENTIALS_DIR":
			values.CredentialsDir = val
		case "PASSWD_DIR":
			values.PasswdDir = val
		}
	}
	if err := sc.Err(); err != nil {
		return values, fmt.Errorf("read config %q: %w", path, err)
	}
	return values, nil
}

// UseInstanceRole reports whether credentials come from the EC2 instance role
// (IMDS). It needs the explicit opt-in and no static keys at all, so a config
// that skipped validate() still cannot reach IMDS by accident.
func (c *Config) UseInstanceRole() bool {
	return c.Credentials == CredentialsInstanceRole && c.AccessKeyID == "" && c.SecretAccessKey == ""
}

// ResolveCredentials returns the credential set for a volume. An empty ref
// uses the plugin configuration; otherwise <CredentialsDir>/<ref>.conf is
// read, which lets the platform rotate per-user secrets without restarting
// CubeMaster or Cubelet.
func (c *Config) ResolveCredentials(ref string) (*Credentials, error) {
	creds := &Credentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		Endpoint:        c.Endpoint,
		Region:          c.Region,
		Addressing:      c.Addressing,
		ExtraOpts:       append([]string(nil), c.S3FSExtraOpts...),
		InstanceRole:    c.UseInstanceRole(),
	}
	if ref == "" {
		if !creds.InstanceRole && (creds.AccessKeyID == "" || creds.SecretAccessKey == "") {
			return nil, errors.New("legacy volumes require ACCESS_KEY_ID and SECRET_ACCESS_KEY or CREDENTIALS=instance_role; dynamic volumes require cred_ref")
		}
		return creds, nil
	}
	path, err := c.credentialsPath(ref)
	if err != nil {
		return nil, err
	}
	values, err := parseFile(path)
	if err != nil {
		return nil, fmt.Errorf("credential reference %q is unreadable", ref)
	}
	if values.AccessKeyID == "" || values.SecretAccessKey == "" {
		return nil, fmt.Errorf("credential reference %q must set ACCESS_KEY_ID and SECRET_ACCESS_KEY", ref)
	}
	if len(values.S3FSExtraOpts) != 0 {
		return nil, fmt.Errorf("credential reference %q cannot set S3FS_EXTRA_OPTS; configure mount options globally", ref)
	}
	creds.AccessKeyID, creds.SecretAccessKey = values.AccessKeyID, values.SecretAccessKey
	creds.InstanceRole = false
	if values.Endpoint != "" {
		creds.Endpoint = values.Endpoint
	}
	if creds.Endpoint == "" {
		return nil, fmt.Errorf("credential reference %q has no ENDPOINT and the plugin config sets none", ref)
	}
	if values.Region != "" {
		creds.Region = values.Region
	}
	addressing, explicit, err := parseAddressing(values.AddressingStyle)
	if err != nil {
		return nil, fmt.Errorf("credential reference %q: %w", ref, err)
	}
	if explicit {
		creds.Addressing = addressing
		if addressing != AddressingPath {
			creds.ExtraOpts = WithoutPathRequestStyle(creds.ExtraOpts)
		}
	}
	return creds, nil
}

// parseAddressing validates an ADDRESSING_STYLE value. The second result says
// whether the value was set explicitly: "auto" and an empty value behave the
// same as a mode, but an explicit auto must not be overridden by a path-style
// flag, so callers need to tell them apart.
func parseAddressing(raw string) (Addressing, bool, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "":
		return AddressingAuto, false, nil
	case "auto":
		return AddressingAuto, true, nil
	case "path":
		return AddressingPath, true, nil
	case "virtual":
		return AddressingVirtual, true, nil
	default:
		return AddressingAuto, false, fmt.Errorf("config: ADDRESSING_STYLE %q must be auto, path or virtual", raw)
	}
}

// WithoutPathRequestStyle removes s3fs path-style flags from opts. It handles
// both "-ouse_path_request_style" and the two-token "-o use_path_request_style"
// form, and removes only the path-style element from a comma-separated -o
// value so unrelated options in the same value survive.
func WithoutPathRequestStyle(opts []string) []string {
	out := make([]string, 0, len(opts))
	for i := 0; i < len(opts); i++ {
		original := []string{opts[i]}
		value := strings.TrimPrefix(opts[i], "-o")
		split := opts[i] == "-o" && i+1 < len(opts)
		if split {
			i++
			value = opts[i]
			original = append(original, value)
		} else if !strings.HasPrefix(opts[i], "-o") || opts[i] == "-o" {
			out = append(out, opts[i])
			continue
		}
		var kept []string
		removed := false
		for _, part := range strings.Split(value, ",") {
			if strings.TrimSpace(part) == "use_path_request_style" {
				removed = true
			} else {
				kept = append(kept, part)
			}
		}
		if !removed {
			out = append(out, original...)
		} else if value = strings.Join(kept, ","); value != "" {
			if split {
				out = append(out, "-o", value)
			} else {
				out = append(out, "-o"+value)
			}
		}
	}
	return out
}

// credentialsPath resolves a reference to a file inside CredentialsDir and
// refuses anything that could escape it.
func (c *Config) credentialsPath(ref string) (string, error) {
	if err := binding.ValidateCredRef(ref); err != nil {
		return "", fmt.Errorf("credential reference %q is not a safe file name", ref)
	}
	dir := c.CredentialsDir
	if dir == "" {
		dir = DefaultCredentialsDir
	}
	return filepath.Join(dir, ref+CredentialFileSuffix), nil
}

// DynamicPasswdFilePrefix names the per-volume credential files written for
// dynamic volumes. It deliberately differs from the legacy per-bucket name
// (.passwd-s3fs-volume-<bucket>): a volume ID can equal a bucket name, and the
// two files must never overwrite each other.
const DynamicPasswdFilePrefix = ".passwd-s3fs-dynamic-"

// PasswdFileForVolume is the s3fs credential file for one dynamic volume.
// volumeID must already have passed binding.ValidateVolumeID.
// Per-volume files avoid two credentials for the same bucket racing on one
// path. The file is root-owned, mode 0600, and outside the mounted tree.
func (c *Config) PasswdFileForVolume(volumeID string) string {
	dir := c.PasswdDir
	if dir == "" {
		if c.PasswdFile != "" {
			dir = filepath.Dir(c.PasswdFile)
		} else {
			dir = DefaultPasswdDir
		}
	}
	return filepath.Join(dir, DynamicPasswdFilePrefix+volumeID)
}

// IsDynamicPasswdFile reports whether path is exactly the dynamic credential
// file derived for volumeID under this configuration. It intentionally does
// not accept prefix matches, sibling volumes, legacy shared files, or paths
// outside PasswdDir.
func (c *Config) IsDynamicPasswdFile(volumeID, path string) bool {
	if binding.ValidateVolumeID(volumeID) != nil || path == "" {
		return false
	}
	want := filepath.Clean(c.PasswdFileForVolume(volumeID))
	return filepath.Clean(path) == want
}

func (c *Config) validate() error {
	switch c.Credentials {
	case CredentialsStatic:
		// Both empty is allowed for deployments that only use cred_ref files;
		// a volume without credentials then fails with a clear error when it
		// is created or attached. One key alone is always a misconfiguration.
		if (c.AccessKeyID == "") != (c.SecretAccessKey == "") {
			return errors.New("config: set both ACCESS_KEY_ID and SECRET_ACCESS_KEY, or neither when every volume uses cred_ref")
		}
	case CredentialsInstanceRole:
		if c.AccessKeyID != "" || c.SecretAccessKey != "" {
			return fmt.Errorf("config: CREDENTIALS=%s takes no ACCESS_KEY_ID / SECRET_ACCESS_KEY; remove the keys, or drop CREDENTIALS to use them", CredentialsInstanceRole)
		}
	default:
		return fmt.Errorf("config: CREDENTIALS=%q is not supported (use %s or %s)", c.Credentials, CredentialsStatic, CredentialsInstanceRole)
	}

	switch {
	case c.Endpoint == "":
		return fmt.Errorf("config: ENDPOINT is empty (set your S3-compatible endpoint URL)")
	case c.Credentials == CredentialsInstanceRole && !isAWSEndpoint(c.Endpoint):
		// COS, R2 and MinIO do not accept AWS role credentials; fail here
		// rather than on the first create after an IMDS round trip.
		return fmt.Errorf("config: CREDENTIALS=%s needs an AWS S3 endpoint (*.amazonaws.com), got %q; other backends need ACCESS_KEY_ID / SECRET_ACCESS_KEY", CredentialsInstanceRole, c.Endpoint)
	case c.Bucket != "" && !validBucketName(c.Bucket):
		return fmt.Errorf("config: BUCKET %q contains an unsupported character", c.Bucket)
	default:
		return nil
	}
}

func validBucketName(bucket string) bool {
	for _, r := range bucket {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// isAWSEndpoint reports whether endpoint is an AWS S3 host, including China
// regions and VPC endpoints. A bare host without scheme is accepted, as in
// s3api.ParseEndpoint.
func isAWSEndpoint(endpoint string) bool {
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn")
}

// HasPathRequestStyle reports whether s3fs was told to use path-style
// addressing (-ouse_path_request_style). Every caller that decides between
// path and virtual addressing must use this one predicate, or the control plane
// and the mount can disagree about what counts as a path-style flag.
func HasPathRequestStyle(opts []string) bool {
	for _, o := range opts {
		if strings.Contains(o, "use_path_request_style") {
			return true
		}
	}
	return false
}

// unquote strips one layer of matching single or double quotes, so values
// written for `source` compatibility (S3FS_EXTRA_OPTS='-o...') parse the same.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// VolumePrefix returns the object key prefix for a legacy volume (trailing
// slash). This is also the key of the directory object s3fs stats when
// mounting.
func VolumePrefix(volumeID string) string {
	return "volumes/" + volumeID + "/"
}

// VolumeSubdir returns the path segment passed to s3fs (no trailing slash).
func VolumeSubdir(volumeID string) string {
	return "volumes/" + volumeID
}

// MountPointUnder returns the per-volume FUSE mount path under baseDir. The
// path MUST live inside baseDir so it satisfies Cubelet's host_path check.
func MountPointUnder(baseDir, volumeID string) string {
	if baseDir == "" {
		baseDir = DefaultVolumeBaseDir
	}
	return filepath.Join(baseDir, "s3-"+volumeID)
}
