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
// Config file: volume-s3.conf next to this binary (or $CUBE_S3_CONFIG). It holds
// a plaintext secret, so keep it root-owned and chmod 600. See
// volume-s3.conf.example.
//
// Path overrides, so the plugin can be exercised without root (e.g. in CI):
//
//	CUBE_S3_CONFIG       config file path
//	CUBE_S3_PASSWD_FILE  s3fs credential file (default /etc/cube/.passwd-s3fs-volume-<bucket>)
//	CUBE_S3_LOCK_DIR     attach/detach locks  (default /run/cube-volume-s3)
//
// The control plane talks to the endpoint directly over HTTP, so no S3 command
// line tool is needed. The data plane still requires s3fs on every Cubelet node.
//
// Mount layout (one s3fs process per volume):
//
//	<volume-base-dir>/s3-<volume_id>/  ->  BUCKET:/volumes/<volume_id>/
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
		return doAttach(opts)
	case "detach":
		return doDetach(opts)
	default:
		return fmt.Errorf("unknown op: %s", opts.op)
	}
}

// doCreate provisions backend storage for a new volume: ensure the bucket
// exists, then write the volumes/<id>/ directory object.
//
// private_data is opaque create-to-attach state (max 1024 bytes). This plugin
// stores the object key prefix so attach can log it without hardcoding a layout.
func doCreate(ctx context.Context, opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("create: --volume-id is required")
	}
	log.Printf("create volumeID=%s name=%s", opts.volumeID, opts.name)

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	client, err := s3api.New(cfg)
	if err != nil {
		return err
	}
	if err := client.EnsureBucket(ctx); err != nil {
		return err
	}
	if err := client.CreateVolumeDir(ctx, opts.volumeID); err != nil {
		return err
	}

	privateData := config.VolumePrefix(opts.volumeID)
	log.Printf("create ready: private_data=%s", privateData)
	emit(createResponse{PrivateData: privateData})
	return nil
}

// doDestroy removes the volume prefix from the bucket. This is irreversible.
func doDestroy(ctx context.Context, opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("destroy: --volume-id is required")
	}
	log.Printf("destroy volumeID=%s", opts.volumeID)

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	client, err := s3api.New(cfg)
	if err != nil {
		return err
	}
	if err := client.RemoveVolumeDir(ctx, opts.volumeID); err != nil {
		return err
	}
	emit(statusResponse{})
	return nil
}

// doAttach makes volume data visible on this node and tells Cubelet where it is.
// Cubelet bind-mounts host_path into the sandbox at the user's chosen path.
func doAttach(opts *options) error {
	if opts.volumeID == "" {
		return fmt.Errorf("attach: --volume-id is required")
	}
	log.Printf("attach sandbox=%s volumeID=%s refcount_before=%d private_data=%s",
		opts.sandboxID, opts.volumeID, opts.refCount, opts.privateData)

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
	log.Printf("lock acquired for volume %s", opts.volumeID)

	// Mount is idempotent, so a repeat attach (refCount > 0) resolves to the
	// mountpoint that is already there instead of mounting twice.
	mnt, err := mounts.Mount(opts.volumeBaseDir, opts.volumeID)
	if err != nil {
		return err
	}

	log.Printf("attach ready: host_path=%s", mnt)
	emit(attachResponse{
		HostPath: mnt,
		Metadata: map[string]string{
			"mount_dir": mnt,
			"volume_id": opts.volumeID,
		},
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
