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
#include "spdk/json.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/util.h"
#include "spdk/uuid.h"

#include "vbdev_s3lvol.h"
#include "vbdev_s3lvol_json.h"

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

static int
bstore_read_file(char **out)
{
	struct stat st;
	char *buf;
	size_t off = 0;
	ssize_t n;
	int fd, rc = 0;

	*out = NULL;
	fd = open(BSTORE_PATH, O_RDONLY | O_NOFOLLOW);
	if (fd < 0) {
		return errno == ENOENT ? 0 : -errno;
	}
	if (fstat(fd, &st) != 0) {
		rc = -errno;
		goto done;
	}
	if (st.st_size < 0 || (uint64_t)st.st_size > SIZE_MAX - 1) {
		rc = -EOVERFLOW;
		goto done;
	}
	buf = calloc(1, (size_t)st.st_size + 1);
	if (!buf) {
		rc = -ENOMEM;
		goto done;
	}
	while (off < (size_t)st.st_size) {
		n = read(fd, buf + off, (size_t)st.st_size - off);
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			rc = -errno;
			free(buf);
			goto done;
		}
		if (n == 0) {
			rc = -EIO;
			free(buf);
			goto done;
		}
		off += (size_t)n;
	}
	*out = buf;

done:
	close(fd);
	return rc;
}

static int
bstore_write_file(const char *s)
{
	return s3lvol_statefile_write(BSTORE_PATH, s);
}

struct bstore_entry {
	char *lvs_name;
	char *bs_name;
	char *ns_name;
	char *wal_bdev;
};

static const struct spdk_json_object_decoder bstore_entry_decoders[] = {
	{"bs_name", offsetof(struct bstore_entry, bs_name), spdk_json_decode_string, false},
	{"ns_name", offsetof(struct bstore_entry, ns_name), spdk_json_decode_string, true},
	{"wal_bdev", offsetof(struct bstore_entry, wal_bdev), spdk_json_decode_string, true},
};

static void
bstore_entries_free(struct bstore_entry *entries, size_t count)
{
	size_t i;

	for (i = 0; i < count; i++) {
		free(entries[i].lvs_name);
		free(entries[i].bs_name);
		free(entries[i].ns_name);
		free(entries[i].wal_bdev);
	}
	free(entries);
}

static int
bstore_parse(char *text, struct bstore_entry **entries_out, size_t *count_out)
{
	struct spdk_json_val *values = NULL, *it, *end;
	struct bstore_entry *entries = NULL;
	ssize_t num_values, rc;
	size_t count = 0;
	int status = 0;

	*entries_out = NULL;
	*count_out = 0;
	if (!text) {
		return 0;
	}

	num_values = spdk_json_parse(text, strlen(text), NULL, 0, NULL, 0);
	if (num_values <= 0) {
		return -EINVAL;
	}
	values = calloc((size_t)num_values, sizeof(*values));
	if (!values) {
		return -ENOMEM;
	}
	rc = spdk_json_parse(text, strlen(text), values, (size_t)num_values, NULL,
			     SPDK_JSON_PARSE_FLAG_DECODE_IN_PLACE);
	if (rc != num_values || values[0].type != SPDK_JSON_VAL_OBJECT_BEGIN) {
		status = -EINVAL;
		goto out;
	}

	end = values + num_values;
	it = &values[1];
	while (it != NULL && it < end && it->type == SPDK_JSON_VAL_NAME) {
		struct bstore_entry *grown;
		struct spdk_json_val *obj = it + 1;

		grown = realloc(entries, (count + 1) * sizeof(*entries));
		if (!grown) {
			status = -ENOMEM;
			goto out;
		}
		entries = grown;
		memset(&entries[count], 0, sizeof(entries[count]));
		count++;
		entries[count - 1].lvs_name = strndup(it->start, it->len);
		if (!entries[count - 1].lvs_name) {
			status = -ENOMEM;
			goto out;
		}
		if (spdk_json_decode_object(obj, bstore_entry_decoders,
					    SPDK_COUNTOF(bstore_entry_decoders),
					    &entries[count - 1]) != 0) {
			status = -EINVAL;
			goto out;
		}
		it = spdk_json_next(obj);
	}
	if ((it != NULL && (it >= end || it->type != SPDK_JSON_VAL_OBJECT_END)) ||
	    values[num_values - 1].type != SPDK_JSON_VAL_OBJECT_END) {
		status = -EINVAL;
	}

out:
	free(values);
	if (status != 0) {
		bstore_entries_free(entries, count);
		return status;
	}
	*entries_out = entries;
	*count_out = count;
	return 0;
}

static int
bstore_serialize(const struct bstore_entry *entries, size_t count, char **out)
{
	struct s3lvol_json_buf buf = {0};
	struct spdk_json_write_ctx *w;
	size_t i;
	int rc;

	w = spdk_json_write_begin(s3lvol_json_buf_append, &buf, 0);
	if (!w) {
		return -ENOMEM;
	}
	spdk_json_write_object_begin(w);
	for (i = 0; i < count; i++) {
		spdk_json_write_named_object_begin(w, entries[i].lvs_name);
		spdk_json_write_named_string(w, "bs_name", entries[i].bs_name);
		if (entries[i].ns_name) {
			spdk_json_write_named_string(w, "ns_name", entries[i].ns_name);
		}
		if (entries[i].wal_bdev) {
			spdk_json_write_named_string(w, "wal_bdev", entries[i].wal_bdev);
		}
		spdk_json_write_object_end(w);
	}
	spdk_json_write_object_end(w);
	rc = spdk_json_write_end(w);
	if (rc != 0 || !buf.data) {
		free(buf.data);
		return rc != 0 ? rc : -ENOMEM;
	}
	*out = buf.data;
	return 0;
}

static int
bstore_rewrite(const char *lvs_name, const char *bs_name,
	       const char *ns_name, const char *wal_bdev)
{
	char *old = NULL;
	struct bstore_entry *entries = NULL;
	char *out = NULL;
	size_t count = 0, i;
	bool found = false;
	int rc = bstore_read_file(&old);

	if (rc != 0) {
		SPDK_ERRLOG("bstore: cannot read %s: %s\n", BSTORE_PATH,
			    spdk_strerror(-rc));
		goto out;
	}
	rc = bstore_parse(old, &entries, &count);
	if (rc != 0) {
		SPDK_ERRLOG("bstore: %s is corrupt; refusing to rewrite it\n",
			    BSTORE_PATH);
		goto out;
	}
	for (i = 0; i < count; i++) {
		if (strcmp(entries[i].lvs_name, lvs_name) != 0) {
			continue;
		}
		found = true;
		if (!bs_name) {
			free(entries[i].lvs_name);
			free(entries[i].bs_name);
			free(entries[i].ns_name);
			free(entries[i].wal_bdev);
			memmove(&entries[i], &entries[i + 1],
				(count - i - 1) * sizeof(entries[0]));
			count--;
		} else {
			char *new_bs = strdup(bs_name);
			char *new_ns = ns_name ? strdup(ns_name) : NULL;
			char *new_wal = wal_bdev ? strdup(wal_bdev) : NULL;

			if (!new_bs || (ns_name && !new_ns) || (wal_bdev && !new_wal)) {
				free(new_bs);
				free(new_ns);
				free(new_wal);
				rc = -ENOMEM;
				goto out;
			}
			free(entries[i].bs_name);
			free(entries[i].ns_name);
			free(entries[i].wal_bdev);
			entries[i].bs_name = new_bs;
			entries[i].ns_name = new_ns;
			entries[i].wal_bdev = new_wal;
		}
		break;
	}
	if (bs_name && !found) {
		struct bstore_entry *grown = realloc(entries, (count + 1) * sizeof(*entries));

		if (!grown) {
			rc = -ENOMEM;
			goto out;
		}
		entries = grown;
		memset(&entries[count], 0, sizeof(entries[count]));
		count++;
		entries[count - 1].lvs_name = strdup(lvs_name);
		entries[count - 1].bs_name = strdup(bs_name);
		entries[count - 1].ns_name = ns_name ? strdup(ns_name) : NULL;
		entries[count - 1].wal_bdev = wal_bdev ? strdup(wal_bdev) : NULL;
		if (!entries[count - 1].lvs_name || !entries[count - 1].bs_name ||
		    (ns_name && !entries[count - 1].ns_name) ||
		    (wal_bdev && !entries[count - 1].wal_bdev)) {
			rc = -ENOMEM;
			goto out;
		}
	}

	rc = bstore_serialize(entries, count, &out);
	if (rc != 0) {
		goto out;
	}
	rc = bstore_write_file(out);
	if (rc == 0) {
		SPDK_NOTICELOG("bstore: %s entry for '%s'\n",
			       bs_name ? (found ? "updated" : "saved") : "removed",
			       lvs_name);
	}

out:
	free(out);
	free(old);
	bstore_entries_free(entries, count);
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
