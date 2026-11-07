// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package content implements the E2B volume-content HTTP protocol
// (/volumecontent/{volumeID}/{file,dir,path}) on a Cubelet node.
//
// A request is authorised by a volume-scoped token minted by CubeMaster, is
// served from the volume's host directory leased through the regular volume
// plugin path, and never starts a sandbox. All file access goes through
// os.Root, so the kernel-backed root confinement, not string checks, is what
// stops symlink and ".." escapes.
package content

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/volumecontent"
)

const (
	// DefaultMaxUpload bounds one PUT body so a token holder cannot fill a node.
	DefaultMaxUpload = 5 << 30
	maxPatchBody     = 4 << 10
)

// Options configures a Handler.
type Options struct {
	Tokens *volumecontent.Config
	Leaser Leaser
	// Refs registers each request's use of its volume with the control plane
	// before anything is attached, like a sandbox mount does.
	Refs      VolumeRefs
	MaxUpload int64
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

// Handler serves the volume-content routes.
type Handler struct {
	opts Options
	mux  *http.ServeMux
}

// NewHandler builds the handler. Tokens, Leaser and Refs are required.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Tokens == nil || opts.Leaser == nil || opts.Refs == nil {
		return nil, errors.New("volume content handler requires a token config, a leaser and a volume reference holder")
	}
	if opts.MaxUpload <= 0 {
		opts.MaxUpload = DefaultMaxUpload
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	h := &Handler{opts: opts, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /volumecontent/{volumeID}/file", h.route(false, h.getFile))
	h.mux.HandleFunc("PUT /volumecontent/{volumeID}/file", h.route(true, h.putFile))
	h.mux.HandleFunc("GET /volumecontent/{volumeID}/dir", h.route(false, h.getDir))
	h.mux.HandleFunc("POST /volumecontent/{volumeID}/dir", h.route(true, h.postDir))
	h.mux.HandleFunc("GET /volumecontent/{volumeID}/path", h.route(false, h.getPath))
	h.mux.HandleFunc("PATCH /volumecontent/{volumeID}/path", h.route(true, h.patchPath))
	h.mux.HandleFunc("DELETE /volumecontent/{volumeID}/path", h.route(true, h.deletePath))
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mux.ServeHTTP(w, r)
}

type request struct {
	w       http.ResponseWriter
	r       *http.Request
	h       *Handler
	root    *os.Root
	volPath string
	rel     string
	q       url.Values
}

func (h *Handler) route(write bool, fn func(*request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		volumeID := r.PathValue("volumeID")
		token, ok := bearerToken(r)
		if !ok {
			unauthorized(w)
			return
		}
		claims, err := h.opts.Tokens.Verify(token, h.opts.Now())
		if err != nil {
			unauthorized(w)
			return
		}
		if claims.VolumeID != volumeID {
			writeError(w, http.StatusForbidden, "forbidden", "token is not valid for this volume")
			return
		}
		if write && !claims.CanWrite() {
			writeError(w, http.StatusForbidden, "forbidden", "token does not allow modification")
			return
		}
		q := r.URL.Query()
		volPath, rel, err := cleanVolumePath(q.Get("path"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
			return
		}

		// A token outlives the volume it was issued for, and a request is a use
		// of the volume just like a sandbox mount. Take the control-plane
		// reference first: it fails for a deleted volume, so the plugin is never
		// asked to attach (which could recreate it), and while it is held the
		// volume cannot be deleted. If the answer is unknown, refuse. It is
		// released last, after the lease below has detached.
		releaseRef, err := h.opts.Refs.Hold(r.Context(), claims.VolumeID)
		switch {
		case err == nil:
		case errors.Is(err, ErrVolumeNotFound):
			writeError(w, http.StatusNotFound, "not_found", "volume does not exist")
			return
		default:
			h.opts.Logf("volume content: hold %q: %v", claims.VolumeID, err)
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "unavailable", "volume use could not be registered")
			return
		}
		defer releaseRef()

		hostPath, release, err := h.opts.Leaser.Acquire(r.Context(), claims.VolumeID, claims.Driver, claims.PrivateData)
		if err != nil {
			if errors.Is(err, ErrDriverUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "volume_unavailable", "volume is not available on this node")
				return
			}
			h.opts.Logf("volume content: acquire %q: %v", claims.VolumeID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		defer release()

		root, err := os.OpenRoot(hostPath)
		if err != nil {
			h.opts.Logf("volume content: open root for %q: %v", claims.VolumeID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		defer root.Close()

		fn(&request{w: w, r: r, h: h, root: root, volPath: volPath, rel: rel, q: q})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	v := r.Header.Get("Authorization")
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(v[len(prefix):]), true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "missing, invalid or expired token")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

// fsError maps a filesystem failure to a status without exposing host paths.
func (rq *request) fsError(err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(rq.w, http.StatusNotFound, "not_found", "path does not exist")
	case errors.Is(err, fs.ErrExist), isErrno(err, syscall.ENOTEMPTY):
		writeError(rq.w, http.StatusConflict, "conflict", "path already exists or directory is not empty")
	case errors.Is(err, fs.ErrPermission):
		writeError(rq.w, http.StatusForbidden, "forbidden", "permission denied")
	case isEscape(err) || isErrno(err, syscall.ELOOP):
		writeError(rq.w, http.StatusBadRequest, "invalid_path", "path leaves the volume")
	case isErrno(err, syscall.ENOTDIR, syscall.EISDIR):
		writeError(rq.w, http.StatusBadRequest, "invalid_path", "path has the wrong type for this operation")
	case isErrno(err, syscall.ENOSPC):
		writeError(rq.w, http.StatusInsufficientStorage, "no_space", "no space left on volume")
	default:
		rq.h.opts.Logf("volume content: %s %s: %v", rq.r.Method, rq.r.URL.Path, err)
		writeError(rq.w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (rq *request) bad(msg string) { writeError(rq.w, http.StatusBadRequest, "invalid_request", msg) }

func (rq *request) writeStat(status int, volPath, rel string) {
	info, err := rq.root.Lstat(rel)
	if err != nil {
		rq.fsError(err)
		return
	}
	writeJSON(rq.w, status, describe(rq.root, volPath, rel, info))
}

func optUint32(q url.Values, key string) (*uint32, error) {
	raw := q.Get(key)
	if !q.Has(key) || raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("%s must be an unsigned 32-bit integer", key)
	}
	v := uint32(n)
	return &v, nil
}

func optBool(q url.Values, key string) (*bool, error) {
	raw := q.Get(key)
	if !q.Has(key) || raw == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be a boolean", key)
	}
	return &b, nil
}

type metadata struct {
	uid, gid, mode *uint32
	force          *bool
}

func (rq *request) metadata() (metadata, bool) {
	var m metadata
	var err error
	if m.uid, err = optUint32(rq.q, "uid"); err == nil {
		if m.gid, err = optUint32(rq.q, "gid"); err == nil {
			if m.mode, err = optUint32(rq.q, "mode"); err == nil {
				m.force, err = optBool(rq.q, "force")
			}
		}
	}
	if err != nil {
		rq.bad(err.Error())
		return m, false
	}
	if m.mode != nil {
		if _, err := goMode(*m.mode); err != nil {
			rq.bad(err.Error())
			return m, false
		}
	}
	return m, true
}

func (m metadata) owner() (uid, gid int, set bool) {
	uid, gid = -1, -1
	if m.uid != nil {
		uid, set = int(*m.uid), true
	}
	if m.gid != nil {
		gid, set = int(*m.gid), true
	}
	return
}

// applyMetadata sets ownership and mode on rel; defaultMode is used when the
// client gave none (umask would otherwise decide).
func (rq *request) applyMetadata(rel string, m metadata, defaultMode fs.FileMode) error {
	mode := defaultMode
	if m.mode != nil {
		mode, _ = goMode(*m.mode)
	}
	if err := rq.root.Chmod(rel, mode); err != nil {
		return err
	}
	if uid, gid, set := m.owner(); set {
		return rq.root.Chown(rel, uid, gid)
	}
	return nil
}

func (h *Handler) getFile(rq *request) {
	// O_NONBLOCK keeps a FIFO planted in the volume from blocking the request.
	f, err := rq.root.OpenFile(rq.rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		rq.fsError(err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		rq.fsError(err)
		return
	}
	if !info.Mode().IsRegular() {
		rq.bad(errNotRegular.Error())
		return
	}
	rq.w.Header().Set("Content-Type", "application/octet-stream")
	rq.w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	rq.w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(rq.w, io.LimitReader(f, info.Size()))
}

func (h *Handler) putFile(rq *request) {
	if rq.rel == "." {
		rq.bad("cannot write the volume root")
		return
	}
	m, ok := rq.metadata()
	if !ok {
		return
	}
	parent := path.Dir(rq.rel)
	pinfo, err := rq.root.Stat(parent)
	if err != nil {
		rq.fsError(err)
		return
	}
	if !pinfo.IsDir() {
		rq.bad("parent is not a directory")
		return
	}
	switch existing, err := rq.root.Lstat(rq.rel); {
	case err == nil:
		if existing.IsDir() {
			writeError(rq.w, http.StatusConflict, "conflict", "path is a directory")
			return
		}
		if m.force != nil && !*m.force {
			writeError(rq.w, http.StatusConflict, "conflict", "file exists and force is false")
			return
		}
	case !errors.Is(err, fs.ErrNotExist):
		rq.fsError(err)
		return
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		rq.fsError(err)
		return
	}
	tmp := path.Join(parent, ".cube-upload-"+hex.EncodeToString(suffix[:]))
	f, err := rq.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		rq.fsError(err)
		return
	}
	cleanup := func() { _ = f.Close(); _ = rq.root.Remove(tmp) }

	body := http.MaxBytesReader(rq.w, rq.r.Body, h.opts.MaxUpload)
	if _, err := io.Copy(f, body); err != nil {
		cleanup()
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(rq.w, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds the size limit")
			return
		}
		rq.fsError(err)
		return
	}
	if err := f.Sync(); err != nil {
		cleanup()
		rq.fsError(err)
		return
	}
	if err := f.Close(); err != nil {
		_ = rq.root.Remove(tmp)
		rq.fsError(err)
		return
	}
	if err := rq.applyMetadata(tmp, m, 0o644); err != nil {
		_ = rq.root.Remove(tmp)
		rq.fsError(err)
		return
	}
	if err := rq.root.Rename(tmp, rq.rel); err != nil {
		_ = rq.root.Remove(tmp)
		rq.fsError(err)
		return
	}
	rq.writeStat(http.StatusCreated, rq.volPath, rq.rel)
}

func (h *Handler) getDir(rq *request) {
	depth := 1
	if raw := rq.q.Get("depth"); rq.q.Has("depth") && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxListDepth {
			rq.bad(fmt.Sprintf("depth must be between 0 and %d", maxListDepth))
			return
		}
		if n > 0 {
			depth = n // 0 means the default, as in E2B's filesystem listing
		}
	}
	info, err := rq.root.Stat(rq.rel)
	if err != nil {
		rq.fsError(err)
		return
	}
	if !info.IsDir() {
		rq.bad(errNotDir.Error())
		return
	}

	entries := make([]entryStat, 0, 16)
	prefix := ""
	if rq.rel != "." {
		prefix = rq.rel + "/"
	}
	err = fs.WalkDir(rq.root.FS(), rq.rel, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == rq.rel {
			return nil
		}
		if len(entries) >= maxListEntries {
			return errTooManyEntries
		}
		level := strings.Count(strings.TrimPrefix(p, prefix), "/") + 1
		fi, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, describe(rq.root, "/"+p, p, fi))
		if d.IsDir() && level >= depth {
			return fs.SkipDir
		}
		return nil
	})
	if errors.Is(err, errTooManyEntries) {
		rq.bad(fmt.Sprintf("listing exceeds %d entries; reduce depth", maxListEntries))
		return
	}
	if err != nil {
		rq.fsError(err)
		return
	}
	writeJSON(rq.w, http.StatusOK, entries)
}

var errTooManyEntries = errors.New("too many entries")

func (h *Handler) postDir(rq *request) {
	m, ok := rq.metadata()
	if !ok {
		return
	}
	if rq.rel == "." {
		writeError(rq.w, http.StatusConflict, "conflict", "volume root already exists")
		return
	}
	mode := fs.FileMode(0o755)
	if m.mode != nil {
		mode, _ = goMode(*m.mode)
	}
	force := m.force != nil && *m.force
	var err error
	if force {
		if info, lerr := rq.root.Lstat(rq.rel); lerr == nil && info.IsDir() {
			rq.writeStat(http.StatusCreated, rq.volPath, rq.rel)
			return
		}
		err = rq.root.MkdirAll(rq.rel, mode)
	} else {
		err = rq.root.Mkdir(rq.rel, mode)
	}
	if err != nil {
		rq.fsError(err)
		return
	}
	if err := rq.applyMetadata(rq.rel, m, 0o755); err != nil {
		rq.fsError(err)
		return
	}
	rq.writeStat(http.StatusCreated, rq.volPath, rq.rel)
}

func (h *Handler) getPath(rq *request) {
	rq.writeStat(http.StatusOK, rq.volPath, rq.rel)
}

func (h *Handler) patchPath(rq *request) {
	var body struct {
		UID  *uint32 `json:"uid"`
		GID  *uint32 `json:"gid"`
		Mode *uint32 `json:"mode"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(rq.w, rq.r.Body, maxPatchBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		rq.bad("body must be a JSON object with optional uid, gid and mode")
		return
	}
	info, err := rq.root.Lstat(rq.rel)
	if err != nil {
		rq.fsError(err)
		return
	}
	symlink := info.Mode()&fs.ModeSymlink != 0
	if body.Mode != nil {
		mode, err := goMode(*body.Mode)
		if err != nil {
			rq.bad(err.Error())
			return
		}
		if symlink {
			rq.bad("mode cannot be changed on a symlink")
			return
		}
		if err := rq.root.Chmod(rq.rel, mode); err != nil {
			rq.fsError(err)
			return
		}
	}
	if body.UID != nil || body.GID != nil {
		m := metadata{uid: body.UID, gid: body.GID}
		uid, gid, _ := m.owner()
		chown := rq.root.Chown
		if symlink {
			chown = rq.root.Lchown
		}
		if err := chown(rq.rel, uid, gid); err != nil {
			rq.fsError(err)
			return
		}
	}
	rq.writeStat(http.StatusOK, rq.volPath, rq.rel)
}

func (h *Handler) deletePath(rq *request) {
	if rq.rel == "." {
		rq.bad("the volume root cannot be removed")
		return
	}
	if _, err := rq.root.Lstat(rq.rel); err != nil {
		rq.fsError(err)
		return
	}
	if err := rq.root.RemoveAll(rq.rel); err != nil {
		rq.fsError(err)
		return
	}
	rq.w.WriteHeader(http.StatusNoContent)
}
