// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// MaxVolumeNameLen mirrors CubeAPI's `MAX_VOLUME_NAME_LEN` enforced in
// `CubeAPI/src/handlers/volumes.rs`. Validating client-side turns an opaque
// HTTP 400 into a clean error at the call site.
const MaxVolumeNameLen = 128

// volumeNameRe mirrors the `^[a-zA-Z0-9_-]+$` rule enforced by CubeAPI for
// volume names. Volume IDs use the same character class (named volumes have
// volume_id == name; unnamed ones get a UUID, which also matches).
var volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// VolumeInfo describes a CubeSandbox persistent volume.
type VolumeInfo struct {
	// VolumeID is the stable identifier of the volume. It equals the
	// user-provided name, or an auto-generated UUID for unnamed volumes.
	VolumeID string `json:"volumeID"`
	// Name is the human-readable display name.
	Name string `json:"name"`
	// Token is the optional auth token issued by the volume plugin. It is
	// empty when the plugin does not issue one, and always empty for entries
	// returned by ListVolumes (tokens are only surfaced on create/get-single).
	// It is a data-plane credential: String/GoString mask it, so read the
	// field directly when the actual value is needed.
	Token string `json:"token"`
}

// String masks Token so the credential never leaks into logs, %v/%+v
// formatting, or error text — mirroring the Python SDK's VolumeInfo repr.
func (v VolumeInfo) String() string {
	token := ""
	if v.Token != "" {
		token = "***"
	}
	return fmt.Sprintf("VolumeInfo{VolumeID:%q, Name:%q, Token:%q}", v.VolumeID, v.Name, token)
}

// GoString masks Token for %#v as well, which bypasses String.
func (v VolumeInfo) GoString() string {
	return v.String()
}

// CreateVolumeOptions contains options for CreateVolume.
type CreateVolumeOptions struct {
	// Name is an optional volume identifier. When provided it must match
	// ^[a-zA-Z0-9_-]+$ and be at most MaxVolumeNameLen characters. When empty
	// the server generates a UUID and uses it as both name and volume ID.
	Name string
	// Driver optionally pins a configured volume plugin (matches a
	// volume_plugins[].name entry in the CubeMaster config, e.g. "cos" or
	// "nfs"). When empty the field is not sent and the backend falls back to
	// its first configured plugin (e2b-compatible behavior).
	Driver string
}

// CreateVolume creates a new persistent volume via POST /volumes.
func (c *Client) CreateVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeInfo, error) {
	if err := validateVolumeName(opts.Name); err != nil {
		return nil, err
	}
	payload := map[string]any{"name": opts.Name}
	if opts.Driver != "" {
		payload["driver"] = opts.Driver
	}
	var volume VolumeInfo
	if err := c.doJSON(ctx, http.MethodPost, "/volumes", payload, &volume, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}
	return &volume, nil
}

// ListVolumes lists all volumes via GET /volumes. Tokens are only surfaced on
// create/get-single, so VolumeInfo.Token is always empty in list results.
func (c *Client) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	var volumes []VolumeInfo
	if err := c.doJSON(ctx, http.MethodGet, "/volumes", nil, &volumes, http.StatusOK); err != nil {
		return nil, err
	}
	return volumes, nil
}

// GetVolume fetches a single volume, including its token, via
// GET /volumes/{volumeID}. A missing volume matches ErrVolumeNotFound.
func (c *Client) GetVolume(ctx context.Context, volumeID string) (*VolumeInfo, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	var volume VolumeInfo
	if err := c.doJSON(ctx, http.MethodGet, "/volumes/"+url.PathEscape(volumeID), nil, &volume, http.StatusOK); err != nil {
		return nil, err
	}
	return &volume, nil
}

// DeleteVolume permanently deletes a volume via DELETE /volumes/{volumeID}.
//
// Deleting a volume does not auto-detach it from running sandboxes: while any
// sandbox still mounts it the server refuses with a conflict that matches
// ErrVolumeInUse. A missing volume matches ErrVolumeNotFound, so callers that
// want e2b-style idempotent deletes can treat that case as "already gone".
func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	if err := validateVolumeID(volumeID); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(volumeID), nil, nil, http.StatusOK, http.StatusNoContent)
}

func validateVolumeName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > MaxVolumeNameLen {
		return fmt.Errorf("volume name must be at most %d characters, got %d", MaxVolumeNameLen, len(name))
	}
	if !volumeNameRe.MatchString(name) {
		return fmt.Errorf("volume name must match ^[a-zA-Z0-9_-]+$ (letters, digits, '_' and '-'), got %q", name)
	}
	return nil
}

// validateVolumeID rejects IDs that are unsafe to embed in a URL path.
// Defense-in-depth against path traversal: a malicious volumeID such as
// "../other" must not be interpolated into the request URL. The accepted
// character class covers both named volumes and auto-generated UUIDs.
func validateVolumeID(volumeID string) error {
	if volumeID == "" {
		return fmt.Errorf("volumeID must be a non-empty string")
	}
	if !volumeNameRe.MatchString(volumeID) {
		return fmt.Errorf("volumeID must match ^[a-zA-Z0-9_-]+$ (letters, digits, '_' and '-'), got %q", volumeID)
	}
	return nil
}

// validateVolumeMounts checks CreateOptions.VolumeMounts before the request is
// sent. Mount paths are user input forwarded to the backend, so we require a
// clean absolute POSIX path and reject any "."/".." segment; the backend is
// expected to validate as well.
func validateVolumeMounts(mounts []VolumeMount) error {
	for i, mount := range mounts {
		if err := validateVolumeID(mount.Name); err != nil {
			return fmt.Errorf("volumeMounts[%d]: %w", i, err)
		}
		if err := validateVolumeMountPath(mount.Path); err != nil {
			return fmt.Errorf("volumeMounts[%d]: %w", i, err)
		}
	}
	return nil
}

func validateVolumeMountPath(path string) error {
	if path == "" {
		return fmt.Errorf("volume mount path must be a non-empty string")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("volume mount path must be absolute (start with '/'), got %q", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("volume mount path must not contain '.' or '..' segments, got %q", path)
		}
	}
	return nil
}
