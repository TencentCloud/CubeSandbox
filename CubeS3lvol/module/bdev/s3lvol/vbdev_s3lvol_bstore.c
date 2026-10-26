/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_bstore -- /data/cubelet/rcow/bstore.json registry
 *
 *   Maps a user-visible lvstore name to the auto-generated blobstore name,
 *   namespace and WAL bdev, so that a recovery script can re-issue the correct
 *   attach call without the operator having to remember them.
 *
 *   Format:
 *
 *     {
 *       "src": {"bs_name":"bstore_A1B2C3D4","namespace":"bucket1","wal_bdev":"n1"},
 *       "dst": {"bs_name":"bstore_E5F6G7H8","namespace":"bucket2","wal_bdev":"n2"}
 *     }
 *
 *   Read as text and written out whole on every change. The write goes through
 *   s3lvol_statefile_write(), which renames a fsynced temp file into place --
 *   this file is only ever read by recovery, and an in-place write torn by a
 *   crash leaves a JSON prefix, which parses as nothing at all rather than as
 *   one entry fewer.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/uuid.h"

#include "vbdev_s3lvol.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_bstore)

#define BSTORE_PATH_DEFAULT "/data/cubelet/rcow/bstore.json"
#define BSTORE_PATH_ENV     "S3LVOL_BSTORE_FILE"

/* See the note on ACTIVE_PATH in vbdev_s3lvol_active.c: overridable so that
 * tests do not write into a production instance's registry. */
#define BSTORE_PATH (s3lvol_statefile_path(BSTORE_PATH_ENV, BSTORE_PATH_DEFAULT))
#define BSTORE_BS_NAME_PREFIX "bstore_"

/* --------------------------------------------------------------------------
 * Generate a blobstore name: bstore_ + 8 hex chars (first 4 bytes of a UUID).
 * -------------------------------------------------------------------------- */

void
bstore_generate_bs_name(char *out, size_t out_size)
{
	struct spdk_uuid u;
	char ustr[SPDK_UUID_STRING_LEN];

	spdk_uuid_generate(&u);
	spdk_uuid_fmt_lower(ustr, sizeof(ustr), &u);
	snprintf(out, out_size, BSTORE_BS_NAME_PREFIX "%.8s", ustr);
}

/* --------------------------------------------------------------------------
 * File access. Both delegate to the statefile helpers so that the atomicity
 * lives in one place -- rcow_active_lvols has the same requirement.
 * -------------------------------------------------------------------------- */

static char *
bstore_read_file(void)
{
	return s3lvol_statefile_read(BSTORE_PATH);
}

static int
bstore_write_file(const char *s)
{
	return s3lvol_statefile_write(BSTORE_PATH, s);
}

/* --------------------------------------------------------------------------
 * Rewrite the file, replacing or inserting one top-level entry.
 *
 * If `bs_name` is NULL the entry is removed; otherwise it is written or
 * updated.
 *
 * Parsing is intentionally minimal: each top-level key is a quoted string
 * followed by `:{` ... `}`. That is the entirety of the format.
 * -------------------------------------------------------------------------- */

static int
bstore_rewrite(const char *lvs_name, const char *bs_name,
	       const char *ns_name, const char *wal_bdev)
{
	char *old = bstore_read_file();
	char *out, *p;
	const char *rp, *end;
	int own_entry = (bs_name != NULL) ? 1 : 0;
	int found = 0;
	size_t out_size, avail;
	int rc;

	/* Sized from the actual strings rather than a round number: the entry is
	 * "  \"<lvs>\": {\"bs_name\":\"<bs>\",\"ns_name\":\"<ns>\",\"wal_bdev\":\"<wal>\"}"
	 * and each of those four names can be up to 63 characters, which together
	 * with the punctuation overruns any fixed slack that looks big enough. A
	 * snprintf() would then truncate mid-string and leave a file that does not
	 * parse -- silently, and only noticed by whoever tries to recover from it.
	 * The 128 covers the field names, braces, quotes and separators. */
	out_size = (old ? strlen(old) : 0)
		   + strlen(lvs_name)
		   + (bs_name  ? strlen(bs_name)  : 0)
		   + (ns_name  ? strlen(ns_name)  : 0)
		   + (wal_bdev ? strlen(wal_bdev) : 0)
		   + 128;

	out = calloc(1, out_size);
	if (!out) {
		free(old);
		return -ENOMEM;
	}

#define BSTORE_AVAIL() (out_size - (size_t)(p - out))

	p = out;
	p += snprintf(p, BSTORE_AVAIL(), "{\n");

	if (old) {
		rp = old;
		end = old + strlen(old);

		/* Skip past the opening '{'. */
		while (rp < end && *rp != '{') { rp++; }
		if (rp < end && *rp == '{') { rp++; }

		while (rp < end) {
			const char *k0, *k1;
			const char *v0, *v1;
			int depth;

			/* Skip whitespace, commas and '}'. */
			while (rp < end && (*rp == ' ' || *rp == '\n' ||
				    *rp == '\r' || *rp == '\t' ||
				    *rp == ',' || *rp == '}')) {
				rp++;
			}
			if (rp >= end) { break; }

			/* Key: "…" */
			k0 = rp + 1;
			k1 = k0;
			while (k1 < end && *k1 != '"') { k1++; }
			if (k1 >= end) { break; }
			rp = k1 + 1;

			/* Skip to '{' of the value. */
			while (rp < end && *rp != '{') { rp++; }
			if (rp >= end) { break; }

			/* Count braces to find the matching '}'. */
			v0 = rp;
			depth = 0;
			while (rp < end) {
				if (*rp == '{') { depth++; }
				if (*rp == '}') { depth--; }
				rp++;
				if (depth == 0) { break; }
			}
			v1 = rp;

			if ((size_t)(k1 - k0) == strlen(lvs_name) &&
			    strncmp(k0, lvs_name, k1 - k0) == 0) {
				found = 1;
				if (!own_entry) {
					/* Removal: just skip this one. */
					continue;
				}
				/* Replace: write our own below, skip the old. */
			} else {
				/* Carry this entry forward unchanged. */
				if (p > out + 2) {
					p += snprintf(p, BSTORE_AVAIL(), ",\n");
				}
				p += snprintf(p, BSTORE_AVAIL(),
					"  \"%.*s\": %.*s",
					(int)(k1 - k0), k0,
					(int)(v1 - v0), v0);
			}
		}
	}

	/* Write our entry (if not removing). */
	if (own_entry) {
		if (p > out + 2) {
			p += snprintf(p, BSTORE_AVAIL(), ",\n");
		}
		p += snprintf(p, BSTORE_AVAIL(),
			      "  \"%s\": {\"bs_name\":\"%s\"",
			      lvs_name, bs_name);
		if (ns_name) {
			p += snprintf(p, BSTORE_AVAIL(),
				      ",\"ns_name\":\"%s\"", ns_name);
		}
		if (wal_bdev) {
			p += snprintf(p, BSTORE_AVAIL(),
				      ",\"wal_bdev\":\"%s\"", wal_bdev);
		}
		p += snprintf(p, BSTORE_AVAIL(), "}");
	}

	p += snprintf(p, BSTORE_AVAIL(), "\n}\n");

	/* Sanity check on the sizing above: a truncated write means the file no
	 * longer parses, and the only thing that reads it is recovery. */
	avail = BSTORE_AVAIL();
	if (avail == 0) {
		SPDK_ERRLOG("bstore: entry for '%s' did not fit; refusing to write "
			    "a truncated file\n", lvs_name);
		free(out);
		free(old);
		return -ENAMETOOLONG;
	}

#undef BSTORE_AVAIL

	/* Report what the write actually did. Returning 0 on a failed write would
	 * tell the caller the entry is on disk when it is not. */
	rc = bstore_write_file(out);
	if (rc == 0) {
		SPDK_NOTICELOG("bstore: %s entry for '%s'\n",
			       own_entry ? (found ? "updated" : "saved") : "removed",
			       lvs_name);
	}

	free(out);
	free(old);
	return rc;
}

int
bstore_save_entry(const char *lvs_name, const char *bs_name,
		  const char *ns_name, const char *wal_bdev)
{
	if (!lvs_name || !bs_name) {
		return -EINVAL;
	}
	return bstore_rewrite(lvs_name, bs_name, ns_name, wal_bdev);
}

int
bstore_remove_entry(const char *lvs_name)
{
	if (!lvs_name) {
		return -EINVAL;
	}
	return bstore_rewrite(lvs_name, NULL, NULL, NULL);
}
