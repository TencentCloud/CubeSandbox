// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// cube-volume-s3 — CubeSandbox VolumePlugin for any S3-compatible object storage
// (AWS S3, Tencent Cloud COS, MinIO, Cloudflare R2, Ceph RGW, ...).
//
// One hook per line:
//
//	create  — make an empty folder for this volume in the bucket (control plane)
//	destroy — delete that folder from the bucket (control plane)
//	attach  — mount the bucket folder on the node with s3fs (data plane)
//	detach  — unmount s3fs when no sandbox uses the volume anymore
//
// CubeMaster calls create/destroy when users create/delete volumes via API.
// Cubelet calls attach/detach when sandboxes start/stop using a volume.
//
// Calling convention: one process per operation.
//
//	cube-volume-s3 --op <op> [--<key> <value> ...]
//
// A single JSON object goes to stdout; logs go to stderr. Exit 0 with an empty
// "error" field means success.
//
// Dynamic storage: Create may receive --private-data with a JSON object such
// as {"bucket":"customer-bucket","prefix":"workspaces/user-a/","cred_ref":"user-a"}.
// The plugin provisions that prefix in the caller's own bucket, returns the
// resolved binding as private_data, and Attach/Destroy use only the returned
// state. cred_ref names a file in CREDENTIALS_DIR; plaintext credentials never
// appear in private_data. A volume created without private_data keeps the
// legacy shape: BUCKET:/volumes/<volume_id>/ and a plain prefix string.
//
// Dynamic creates require an existing bucket and an empty prefix (apart from
// its zero-byte directory marker). Destroy recursively removes that prefix.
// The platform must therefore authorize each cred_ref/bucket/prefix binding,
// keep prefixes for different volumes non-overlapping, and avoid reusing a
// prefix until deletion has finished. The plugin does not provide distributed
// coordination or adopt pre-existing data: a failed Create may be rolled back
// with Destroy(B), which is indistinguishable from a user delete.
//
// Config file: volume-s3.conf next to this binary (or $CUBE_S3_CONFIG). It holds
// a plaintext secret, so keep it root-owned and chmod 600. See
// volume-s3.conf.example.
//
// Path overrides, so the plugin can be exercised without root (e.g. in CI):
//
//	CUBE_S3_CONFIG           config file path
//	CUBE_S3_CREDENTIALS_DIR  cred_ref files (default /etc/cube/s3-credentials)
//	CUBE_S3_PASSWD_FILE      legacy s3fs credential file
//	CUBE_S3_PASSWD_DIR       per-volume s3fs credential files (default /etc/cube)
//	CUBE_S3_LOCK_DIR         attach/detach locks (default /run/cube-volume-s3)
//
// The control plane talks to the endpoint directly over HTTP, so no S3 command
// line tool is needed. The data plane still requires s3fs on every Cubelet node.
//
// Mount layout (one s3fs process per volume):
//
//	<volume-base-dir>/s3-<volume_id>/  ->  BUCKET:/<binding prefix>
//
// where <volume-base-dir> is passed by Cubelet via --volume-base-dir (default
// /data/cube-shared/volume). host_path MUST live inside it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/lockfile"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3api"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3fsmnt"
)

type options struct {
	op            string
	volumeID      string
	name          string
	sandboxID     string
	namespace     string
	refCount      int64
	volumeBaseDir string
	privateData   string
	metadata      string
}

// createResponse is the Controller create hook reply.
type createResponse struct {
	Token       string `json:"token"`
	PrivateData string `json:"private_data"`
	Error       string `json:"error"`
}

// attachResponse is the Node attach hook reply.
type attachResponse struct {
	HostPath string            `json:"host_path"`
	Metadata map[string]string `json:"metadata"`
	Error    string            `json:"error"`
}

// statusResponse is the reply for destroy / detach and for every failure.
type statusResponse struct {
	Error string `json:"error"`
}

func main() {
	log.SetPrefix("[cube-volume-s3] ")
	log.SetFlags(0)

	opts := parseFlags()

	if err := run(context.Background(), opts); err != nil {
		log.Printf("ERROR: %v", err)
		emit(statusResponse{Error: err.Error()})
		os.Exit(1)
	}
}

func parseFlags() *options {
	opts := &options{metadata: "{}"}

	fs := flag.NewFlagSet("cube-volume-s3", flag.ExitOnError)
	fs.StringVar(&opts.op, "op", "", "operation: create|destroy|attach|detach")
	fs.StringVar(&opts.volumeID, "volume-id", "", "volume ID")
	fs.StringVar(&opts.name, "name", "", "human-readable volume label")
	fs.StringVar(&opts.sandboxID, "sandbox-id", "", "sandbox ID")
	fs.StringVar(&opts.namespace, "namespace", "", "sandbox namespace")
	fs.Int64Var(&opts.refCount, "ref-count", 0, "reference count reported by Cubelet")
	fs.StringVar(&opts.volumeBaseDir, "volume-base-dir", "", "parent dir host_path must live under")
	fs.StringVar(&opts.privateData, "private-data", "", "opaque state from create")
	fs.StringVar(&opts.metadata, "metadata", "{}", "opaque map from the matching attach")

	// CubeMaster and Cubelet always pass flags, never positional arguments.
	_ = fs.Parse(os.Args[1:])
	return opts
}

func run(ctx context.Context, opts *options) error {
	switch opts.op {
	case "":
		return fmt.Errorf("--op is required")
	case "create":
		return doCreate(ctx, opts)
	case "destroy":
		return doDestroy(ctx, opts)
	case "attach":
		return doAttach(ctx, opts)
	case "detach":
		return doDetach(opts)
	default:
		return fmt.Errorf("unknown op: %s", opts.op)
	}
}

// doCreate provisions backend storage for a new volume: ensure the target
// bucket exists, then write the directory object for the volume prefix.
//
// private_data is opaque create-to-attach state (max 1024 bytes). This plugin
// accepts an optional JSON descriptor from the caller and returns the resolved
// binding through this field. A volume created without the descriptor keeps
// the deployment-configured bucket and volumes/<id>/ layout.
//
// A dynamic create only proceeds on an empty prefix. CubeMaster may roll a
// failed create back with Destroy(B), and that delete cannot be distinguished
// from a user delete, so adopting existing objects would make rollback
// destructive. The platform coordinates non-overlapping prefixes across
// volumes and delays reuse until deletion has completed.
func doCreate(ctx context.Context, opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("create: --volume-id is required")
	}

	req, err := binding.ParseCreate(opts.privateData, opts.volumeID)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	log.Printf("create volumeID=%s name=%s dynamic=%t", opts.volumeID, opts.name, req.Dynamic)

	cfg, err := config.Load("")
	if err != nil {
		return err
	}

	volume, err := resolveVolume(cfg, binding.Binding{
		Bucket: req.Bucket, Prefix: req.Prefix, CredRef: req.CredRef, Dynamic: req.Dynamic,
	})
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	client, err := volume.client(ctx)
	if err != nil {
		return err
	}

	// Resolve state B before touching the bucket. Encoding can fail even
	// though the caller's descriptor passed the size check (JSON escaping and
	// the resolved bucket add bytes), and a failure here must not roll back
	// data that already lived under the prefix.
	var state string
	if req.Dynamic {
		state, err = binding.EncodeState(volume.state)
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
	}

	if req.Dynamic {
		if err := createDynamicPrefix(ctx, client, volume.state.Bucket, volume.state.Prefix); err != nil {
			return err
		}
	} else {
		if err := client.EnsureBucket(ctx); err != nil {
			return err
		}
		if err := client.CreatePrefix(ctx, volume.state.Prefix); err != nil {
			return err
		}
	}

	if !req.Dynamic {
		log.Printf("create ready: volumeID=%s bucket=%s prefix=%s", opts.volumeID, volume.state.Bucket, volume.state.Prefix)
		emit(createResponse{PrivateData: volume.state.Prefix})
		return nil
	}

	log.Printf("create ready: volumeID=%s bucket=%s prefix=%s cred_ref=%s", opts.volumeID, volume.state.Bucket, volume.state.Prefix, req.CredRef)
	emit(createResponse{PrivateData: state})
	return nil
}

type createStore interface {
	BucketExists(context.Context) (bool, error)
	PrefixHasData(context.Context, string) (bool, error)
	CreatePrefix(context.Context, string) error
}

// createDynamicPrefix provisions only an empty prefix in an existing bucket.
// State B must already have passed encoding and size validation before this is
// called. Prefix ownership and concurrent lifecycle coordination belong to the
// platform.
func createDynamicPrefix(ctx context.Context, store createStore, bucket, prefix string) error {
	exists, err := store.BucketExists(ctx)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if !exists {
		return fmt.Errorf("create: bucket %q does not exist; create it before creating the volume (user buckets are never auto-created)", bucket)
	}
	hasData, err := store.PrefixHasData(ctx, prefix)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if hasData {
		return fmt.Errorf("create: prefix %q already contains objects; dynamic volumes require an empty prefix (existing data is never adopted, because a rollback delete would destroy it)", prefix)
	}
	return store.CreatePrefix(ctx, prefix)
}

// doDestroy removes the volume prefix from the bucket. This is irreversible.
//
// ParseState and resolveVolume validate the binding before the recursive
// delete. The platform must ensure that prefixes do not overlap and are not
// reused until this operation completes.
func doDestroy(ctx context.Context, opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("destroy: --volume-id is required")
	}

	b, err := binding.ParseState(opts.privateData, opts.volumeID)
	if err != nil {
		return fmt.Errorf("destroy: %w", err)
	}
	log.Printf("destroy volumeID=%s prefix=%s dynamic=%t", opts.volumeID, b.Prefix, b.Dynamic)

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	volume, err := resolveVolume(cfg, b)
	if err != nil {
		return fmt.Errorf("destroy: %w", err)
	}
	client, err := volume.client(ctx)
	if err != nil {
		return err
	}

	if err := client.RemovePrefix(ctx, volume.state.Prefix); err != nil {
		return err
	}
	emit(statusResponse{})
	return nil
}

// doAttach makes volume data visible on this node and tells Cubelet where it is.
// Cubelet bind-mounts host_path into the sandbox at the user's chosen path.
func doAttach(ctx context.Context, opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("attach: --volume-id is required")
	}

	b, err := binding.ParseState(opts.privateData, opts.volumeID)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	log.Printf("attach sandbox=%s volumeID=%s refcount_before=%d prefix=%s dynamic=%t",
		opts.sandboxID, opts.volumeID, opts.refCount, b.Prefix, b.Dynamic)

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	volume, err := resolveVolume(cfg, b)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	spec := volume.mountSpec(cfg, opts.volumeID)

	mounts := s3fsmnt.New(cfg)
	// s3fs asks IMDS itself; checking first turns a missing role into a clear
	// error instead of an opaque mount failure. Only on a real mount, so a
	// repeat attach of a mounted volume does not depend on IMDS.
	if volume.credentials.InstanceRole {
		mounts.BeforeMount = func() error { return s3api.CheckInstanceRole(ctx, cfg) }
	}

	lock, err := lockfile.Acquire(cfg.LockDir, opts.volumeID)
	if err != nil {
		return err
	}
	defer lock.Release()
	log.Printf("lock acquired for volume %s", opts.volumeID)

	// Mount is idempotent, so a repeat attach (refCount > 0) resolves to the
	// mountpoint that is already there instead of mounting twice.
	mnt, err := mounts.MountWithSpec(opts.volumeBaseDir, opts.volumeID, spec)
	if err != nil {
		return err
	}

	log.Printf("attach ready: host_path=%s", mnt)
	metadata := map[string]string{
		"mount_dir": mnt,
		"volume_id": opts.volumeID,
	}
	if b.Dynamic && !volume.credentials.InstanceRole {
		metadata["passwd_file"] = spec.PasswdFile
	}
	emit(attachResponse{
		HostPath: mnt,
		Metadata: metadata,
	})
	return nil
}

// doDetach stops exposing volume data on this node once nobody uses it.
//
// refCount is how many sandboxes on this node still use the volume after this
// detach, so only refCount 0 unmounts. Bucket data is kept until destroy.
func doDetach(opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("detach: --volume-id is required")
	}
	log.Printf("detach sandbox=%s volumeID=%s refcount_after=%d",
		opts.sandboxID, opts.volumeID, opts.refCount)

	if opts.refCount > 0 {
		log.Printf("skipping unmount: volume still in use (refcount_after=%d)", opts.refCount)
		emit(statusResponse{})
		return nil
	}
	if err := binding.ValidateVolumeID(opts.volumeID); err != nil {
		return fmt.Errorf("detach: %w", err)
	}

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	mounts := s3fsmnt.New(cfg)

	lock, err := lockfile.Acquire(cfg.LockDir, opts.volumeID)
	if err != nil {
		return err
	}
	defer lock.Release()

	mnt := mountDirFromMetadata(opts.metadata)
	if mnt == "" {
		mnt = mounts.MountPoint("", opts.volumeID)
	}

	if err := mounts.Unmount(mnt); err != nil {
		return err
	}

	if err := mounts.RemoveVolumePasswdFile(opts.volumeID, passwdFileFromMetadata(opts.metadata)); err != nil {
		log.Printf("warning: remove dynamic passwd file for volume %s: %v", opts.volumeID, err)
	}

	log.Printf("detach done volumeID=%s (bucket data preserved; delete volume to remove backend data)",
		opts.volumeID)
	emit(statusResponse{})
	return nil
}

// mountDirFromMetadata reads the mount path recorded at attach time. An
// unparsable or absent value falls back to the default layout.
func mountDirFromMetadata(metadata string) string {
	if metadata == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return ""
	}
	return m["mount_dir"]
}

// passwdFileFromMetadata reads the dynamic s3fs credential path recorded by
// Attach. The caller validates it against the configured path for volumeID.
func passwdFileFromMetadata(metadata string) string {
	if metadata == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return ""
	}
	return m["passwd_file"]
}

// emit writes the single JSON line CubeMaster / Cubelet read from stdout.
func emit(resp any) {
	b, err := json.Marshal(resp)
	if err != nil {
		// Marshalling these fixed structs cannot fail; degrade to a valid
		// error object rather than printing nothing at all.
		fmt.Println(`{"error":"marshal response failed"}`)
		return
	}
	fmt.Println(string(b))
}
