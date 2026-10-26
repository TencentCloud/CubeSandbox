/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_statefile -- crash-safe read/write for the small JSON state
 *   files this module keeps outside S3 (/data/cubelet/rcow/bstore.json and
 *   /data/cubelet/rcow/active_lvols).
 *
 *   Both files exist for exactly one reader: recovery after a crash. That is
 *   what makes a torn write worse than it looks. Writing in place with
 *   fopen("w") truncates first, so a crash in the middle leaves a prefix of the
 *   new content -- and a prefix of a JSON object is not a JSON object with one
 *   entry missing, it is a syntax error. The parser then yields nothing at all,
 *   and the file that was supposed to describe what to recover describes
 *   nothing.
 *
 *   So: write a sibling temp file, fsync it, rename over the target, then fsync
 *   the directory. rename(2) within a directory is atomic, so a reader sees
 *   either the whole old file or the whole new one. The final directory fsync is
 *   what makes the rename itself survive a power cut -- without it the rename
 *   can still be in the page cache when the machine goes down, leaving the old
 *   content, which is at least intact.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"

#include "vbdev_s3lvol.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_statefile)

char *
s3lvol_statefile_read(const char *path)
{
	FILE *fp;
	long size;
	char *buf;
	int fd;

	/* O_NOFOLLOW: the state files live in /data/cubelet/rcow (not a sticky
	 * world-writable directory), but an attacker-controlled symlink is still
	 * the thing to defend against; the read must never follow one. */
	fd = open(path, O_RDONLY | O_NOFOLLOW);
	if (fd < 0) {
		return NULL;
	}

	fp = fdopen(fd, "r");
	if (!fp) {
		close(fd);
		return NULL;
	}

	if (fseek(fp, 0, SEEK_END) != 0) {
		fclose(fp);
		return NULL;
	}
	size = ftell(fp);

	/* "{}" is the smallest meaningful content; anything shorter is either
	 * empty or a leftover from a torn write by an older build. */
	if (size <= 2) {
		fclose(fp);
		return NULL;
	}
	rewind(fp);

	buf = calloc(1, (size_t)size + 1);
	if (!buf) {
		fclose(fp);
		return NULL;
	}

	if (fread(buf, 1, (size_t)size, fp) != (size_t)size) {
		SPDK_ERRLOG("statefile: short read from %s\n", path);
		free(buf);
		fclose(fp);
		return NULL;
	}

	fclose(fp);
	return buf;
}

/* Fsync the directory holding `path`, so the rename is durable and not merely
 * visible. Failure is logged but not fatal: the rename has already happened, so
 * the file is correct right now; only its survival across a power cut is in
 * question, and there is nothing better to do about it here. */
static void
statefile_sync_dir(const char *path)
{
	char *dup;
	const char *dir;
	int fd;

	dup = strdup(path);
	if (!dup) {
		return;
	}

	dir = dirname(dup);
	fd = open(dir, O_RDONLY | O_DIRECTORY);
	if (fd < 0) {
		SPDK_WARNLOG("statefile: cannot open %s to fsync: %s\n", dir,
			     strerror(errno));
		free(dup);
		return;
	}

	if (fsync(fd) != 0) {
		SPDK_WARNLOG("statefile: fsync of %s failed: %s\n", dir,
			     strerror(errno));
	}

	close(fd);
	free(dup);
}

int
s3lvol_statefile_write(const char *path, const char *content)
{
	char tmp[PATH_MAX];
	size_t len;
	int fd, rc = 0;
	ssize_t written;

	if (snprintf(tmp, sizeof(tmp), "%s.tmp", path) >= (int)sizeof(tmp)) {
		SPDK_ERRLOG("statefile: path too long: %s\n", path);
		return -ENAMETOOLONG;
	}

	/* Owner-only: the files hold no secrets (credentials are
	 * environment-only), but they are writable only by root anyway, and there
	 * is no reason for anyone else to read them. */
	fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC, 0600);
	if (fd < 0) {
		SPDK_ERRLOG("statefile: cannot create %s: %s\n", tmp,
			    strerror(errno));
		return -errno;
	}

	len = strlen(content);
	while (len > 0) {
		written = write(fd, content, len);
		if (written < 0) {
			if (errno == EINTR) {
				continue;
			}
			rc = -errno;
			SPDK_ERRLOG("statefile: write to %s failed: %s\n", tmp,
				    strerror(errno));
			goto fail;
		}
		content += written;
		len -= (size_t)written;
	}

	/* Before the rename, not after: the rename must not be able to publish a
	 * name that points at data still sitting in the page cache. */
	if (fsync(fd) != 0) {
		rc = -errno;
		SPDK_ERRLOG("statefile: fsync of %s failed: %s\n", tmp,
			    strerror(errno));
		goto fail;
	}

	if (close(fd) != 0) {
		rc = -errno;
		SPDK_ERRLOG("statefile: close of %s failed: %s\n", tmp,
			    strerror(errno));
		unlink(tmp);
		return rc;
	}

	if (rename(tmp, path) != 0) {
		rc = -errno;
		SPDK_ERRLOG("statefile: rename %s -> %s failed: %s\n", tmp, path,
			    strerror(errno));
		unlink(tmp);
		return rc;
	}

	statefile_sync_dir(path);
	return 0;

fail:
	close(fd);
	unlink(tmp);
	return rc;
}

int
s3lvol_statefile_remove(const char *path)
{
	if (unlink(path) != 0 && errno != ENOENT) {
		SPDK_ERRLOG("statefile: cannot remove %s: %s\n", path,
			    strerror(errno));
		return -errno;
	}
	statefile_sync_dir(path);
	return 0;
}
/* Small enough that a fixed table beats a list: there are exactly two state
 * files, and a third would be a design change rather than a configuration one. */
#define STATEFILE_PATH_SLOTS 4

struct statefile_path_slot {
	const char	*env_name;
	char		*resolved;
};

static struct statefile_path_slot g_statefile_paths[STATEFILE_PATH_SLOTS];

const char *
s3lvol_statefile_path(const char *env_name, const char *fallback)
{
	struct statefile_path_slot *slot = NULL;
	const char *env;
	int i;

	assert(env_name != NULL);
	assert(fallback != NULL);

	/* Comparing the pointer, not the contents: callers pass string literals,
	 * and every one of them passes the same literal every time. */
	for (i = 0; i < STATEFILE_PATH_SLOTS; i++) {
		if (g_statefile_paths[i].env_name == env_name) {
			return g_statefile_paths[i].resolved;
		}
		if (!slot && g_statefile_paths[i].env_name == NULL) {
			slot = &g_statefile_paths[i];
		}
	}

	if (!slot) {
		/* Out of slots: honour the override for correctness, but do not
		 * cache it. Cannot happen with two callers; if it ever does, the
		 * table is too small rather than the caller being wrong. */
		SPDK_ERRLOG("statefile: no cache slot left for %s; raise "
			    "STATEFILE_PATH_SLOTS\n", env_name);
		env = getenv(env_name);
		return (env && env[0] == '/') ? env : fallback;
	}

	env = getenv(env_name);
	if (env && env[0] != '\0' && env[0] != '/') {
		/* Rejected rather than resolved against the cwd: a state file that
		 * moves when the target is started from a different directory is
		 * worse than one that ignored the setting, because the old file
		 * still exists and still looks authoritative. */
		SPDK_ERRLOG("statefile: %s='%s' is not an absolute path; using "
			    "%s instead\n", env_name, env, fallback);
		env = NULL;
	}
	if (env && env[0] == '\0') {
		env = NULL;
	}

	/* strdup, because getenv() returns a pointer into an environment that
	 * setenv() is free to reallocate, and this pointer is kept for the life of
	 * the process. */
	slot->resolved = strdup(env ? env : fallback);
	if (!slot->resolved) {
		/* Never cached, so a later call retries. Returning the literal
		 * keeps the caller working rather than handing it NULL. */
		return fallback;
	}
	slot->env_name = env_name;

	if (env) {
		SPDK_NOTICELOG("statefile: %s -> %s (from %s)\n", env_name,
			       slot->resolved, env_name);
	}
	return slot->resolved;
}
