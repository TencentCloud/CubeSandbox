// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// cube-volume-nfs attaches NFS directories through the binary Volume hook.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	volumeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	exportPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*:/[a-zA-Z0-9/_.-]+$`)
	unescapeMount   = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, "\\")
)

type options struct {
	op, id, namespace, baseDir, metadata string
	privateData                          string
	refCount                             int64
}

type result struct {
	HostPath    string            `json:"host_path,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	PrivateData string            `json:"private_data,omitempty"`
	Error       string            `json:"error"`
	ErrorKind   string            `json:"error_kind,omitempty"`
}

func main() {
	var o options
	flag.StringVar(&o.op, "op", "", "create, destroy, attach or detach")
	flag.StringVar(&o.id, "volume-id", "", "CubeSandbox volume ID")
	flag.StringVar(&o.namespace, "namespace", "", "Cubelet namespace")
	flag.StringVar(&o.baseDir, "volume-base-dir", "", "Cubelet volume base directory")
	flag.StringVar(&o.metadata, "metadata", "", "metadata from Attach")
	flag.Int64Var(&o.refCount, "ref-count", 0, "Cubelet local reference count")
	flag.String("name", "", "volume name")
	flag.String("sandbox-id", "", "sandbox ID")
	flag.StringVar(&o.privateData, "private-data", "", "volume binding returned by Create")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		res = errorResult(o.op, err)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(res); encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) (result, error) {
	if !volumeIDPattern.MatchString(o.id) {
		return result{}, &invalidArgumentError{message: "invalid NFS volume ID"}
	}
	switch o.op {
	case "create":
		binding, err := claimVolume(os.Getenv("CUBE_NFS_CONTROL_ROOT"), o.id)
		if err != nil {
			return result{}, err
		}
		return result{PrivateData: binding}, nil
	case "destroy":
		// Leave the external data and its claim together. A new registration
		// must use an unclaimed directory, even after this record is removed.
		return result{}, nil
	case "attach":
		if !bindingPattern.MatchString(o.privateData) {
			return result{}, errors.New("NFS volume registration has no valid binding")
		}
		return attach(ctx, o)
	case "detach":
		return result{}, detach(ctx, o)
	default:
		return result{}, fmt.Errorf("unknown hook operation %q", o.op)
	}
}

func volumeSource(export, id string) (string, error) {
	_, exportPath, _ := strings.Cut(export, ":")
	if !exportPattern.MatchString(export) || path.Clean(exportPath) != exportPath {
		return "", errors.New("CUBE_NFS_EXPORT must be a canonical NFS server:/export path")
	}
	return export + "/" + id, nil
}

func mountPath(base, namespace, id string) (string, error) {
	if !filepath.IsAbs(base) || filepath.Clean(base) != base || namespace == "" {
		return "", errors.New("invalid volume base directory or namespace")
	}
	ns := sha256.Sum256([]byte(namespace))
	return filepath.Join(base, fmt.Sprintf("nfs-%x", ns[:8]), id), nil
}

func attachmentPaths(baseDir, namespace, id string) (base, target, hostPath string, err error) {
	logical, err := mountPath(filepath.Clean(baseDir), namespace, id)
	if err != nil {
		return "", "", "", err
	}
	base, err = filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve Cubelet volume base: %w", err)
	}
	target, err = mountPath(base, namespace, id)
	// mountinfo uses the resolved path. Cubelet checks the returned path
	// against the configured spelling of its volume base directory.
	return base, target, filepath.Join(logical, dataDirName), err
}

func attach(ctx context.Context, o options) (_ result, err error) {
	source, err := volumeSource(os.Getenv("CUBE_NFS_EXPORT"), o.id)
	if err != nil {
		return result{}, err
	}
	base, target, hostPath, err := attachmentPaths(o.baseDir, o.namespace, o.id)
	if err != nil {
		return result{}, err
	}
	mounted, err := verifyMount(target, source)
	if err != nil {
		return result{}, err
	}
	if !mounted {
		// The manager serializes Attach/Detach. Use namespace-specific
		// mountpoints because its persisted reference count is namespaced.
		for _, dir := range []string{filepath.Dir(target), target} {
			if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return result{}, err
			}
			if err := requireDirectory(dir); err != nil {
				return result{}, err
			}
		}
		// Run the NFS helper directly so a timeout stops the helper itself.
		// The node mount is shared. Cubelet sets read-only on the per-sandbox bind mount.
		if err := command(ctx, "mount.nfs", source, target, "-o", "rw,nosuid,nodev,vers=3,proto=tcp,hard,fg,retry=0"); err != nil {
			return result{}, err
		}
		defer func() {
			if err != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				err = errors.Join(err, command(cleanup, "umount", "-i", "--", target))
			}
		}()
		if ok, err := verifyMount(target, source); err != nil || !ok {
			return result{}, errors.Join(errors.New("NFS mount verification failed"), err)
		}
	}
	if err := validateBinding(target, o.privateData); err != nil {
		return result{}, err
	}
	if err := requireDirectory(filepath.Join(target, dataDirName)); err != nil {
		return result{}, err
	}
	return result{HostPath: hostPath, Metadata: map[string]string{"base_dir": base, "source": source}}, nil
}

func detach(ctx context.Context, o options) error {
	return detachWith(ctx, o, verifyMount, command)
}

func detachWith(ctx context.Context, o options, verify func(string, string) (bool, error), unmount func(context.Context, string, ...string) error) error {
	if o.refCount > 0 {
		return nil
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(o.metadata), &meta); err != nil {
		return fmt.Errorf("invalid NFS attachment metadata: %w", err)
	}
	target, err := mountPath(meta["base_dir"], o.namespace, o.id)
	if err != nil {
		return err
	}
	// A worker may still need to detach a mount made before bindings were
	// introduced. Those attachments recorded the data subdirectory as source.
	volumeDir := meta["source"]
	if strings.HasSuffix(volumeDir, "/"+o.id+"/"+dataDirName) {
		volumeDir = strings.TrimSuffix(volumeDir, "/"+dataDirName)
	}
	export := strings.TrimSuffix(volumeDir, "/"+o.id)
	source, err := volumeSource(export, o.id)
	if err != nil || source != volumeDir {
		return errors.New("invalid NFS attachment source")
	}
	mounted, err := verify(target, meta["source"])
	if err != nil {
		return err
	}
	if mounted {
		if err := unmount(ctx, "umount", "-i", "--", target); err != nil {
			return err
		}
	}
	// Remove only the empty local mountpoint. The published data stays in NFS.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty NFS mountpoint: %w", err)
	}
	return nil
}

func verifyMount(target, source string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	return matchMount(string(data), target, source)
}

func matchMount(data, target, source string) (bool, error) {
	found := false
	for line := range strings.Lines(data) {
		before, after, ok := strings.Cut(line, " - ")
		left, right := strings.Fields(before), strings.Fields(after)
		if !ok || len(left) < 6 || len(right) < 3 || unescapeMount.Replace(left[4]) != target {
			continue
		}
		if found || right[0] != "nfs" || unescapeMount.Replace(right[1]) != source ||
			!strings.Contains(","+left[5]+",", ",rw,") ||
			!strings.Contains(","+right[2]+",", ",rw,") {
			return false, errors.New("NFS mountpoint does not match the expected read-write mount")
		}
		found = true
	}
	return found, nil
}

func command(ctx context.Context, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w (%s)", filepath.Base(binary), err, strings.TrimSpace(string(out)))
	}
	return nil
}
