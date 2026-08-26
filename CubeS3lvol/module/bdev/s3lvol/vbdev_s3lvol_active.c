/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_active -- the /data/cubelet/rcow/active_lvols registry
 *
 *   Records which lvol or snapshot is exposed through which NVMf subsystem and
 *   namespace ID, so that a restart can put the host-side layout back exactly as
 *   it was.
 *
 *   Format:
 *
 *     {
 *       "vol0":  {"uuid":"d39dab5f-...","subsys":3,"nsid":7},
 *       "snap0": {"uuid":"520e77a7-...","subsys":3,"nsid":42}
 *     }
 *
 *   === What is deliberately *not* in here ===
 *
 *   The device path. Measured on a live target: nsid 7 came up as /dev/nvme0n1
 *   and nsid 42 as /dev/nvme0n2 -- the host numbers namespaces in the order it
 *   discovers them, not by nsid. Replay the same two namespaces in the other
 *   order and the two paths swap. A recorded path would therefore be a lie
 *   after the first recovery, and a lie of the worst kind: the upper layer would
 *   mount the wrong volume with no error anywhere.
 *
 *   So the path is never stored and never predicted. It is looked up when asked
 *   for, by matching the lvol uuid against the namespace uuid the host exposes
 *   in sysfs -- which is the same value, because vbdev_s3lvol_lvol.c sets
 *   bdev->uuid = lvol->uuid and NVMf passes it through.
 *
 *   Read-only-ness, for a different reason: nothing consumes it. There is no
 *   read-only field in struct spdk_nvmf_ns_opts and no write-protected
 *   namespace anywhere in lib/nvmf, so a snapshot is handed to the host as a
 *   writable device regardless; writes come down and are refused by the lvol
 *   layer, which the host sees as an I/O error. A flag here would suggest a
 *   protection that does not exist.
 *
 *   === Why uuid is stored alongside the name ===
 *
 *   The name is the key, but a name can be reused: delete vol0 and create vol0
 *   again and it is a different volume with a different uuid. Replaying an old
 *   (name -> nsid) mapping onto it would hand the host a namespace that looks
 *   like the one it had before and contains something else. The uuid is what
 *   makes that detectable, so recovery compares it and refuses on a mismatch.
 *
 *   === Threading: every access must stay on the one RPC thread ===
 *
 *   g_active is an unlocked global list, and that is safe only because every
 *   reader and writer runs on the thread that serves the RPCs (the master
 *   thread). This is not an accident that the code happens to satisfy, it is a
 *   contract that several unrelated decisions hold up:
 *
 *   - the RPC server's poller runs on the master thread, so every handler --
 *     and with it every load / find / first / next / alloc_nsid call -- is
 *     serialised there;
 *   - the registry is written from completion callbacks that SPDK bounces back
 *     to the thread which started the operation: nvmf state changes via
 *     spdk_thread_exec_msg on the thread recorded at submission (subsystem.c),
 *     and bdev unregister completions on the io_device's registration thread,
 *     which is the same RPC thread for every operation in this module;
 *   - rcow_get_bdev's wait runs on a spawner thread, but that thread never
 *     touches this list -- it is handed copies of the entries, taken by value
 *     on the RPC thread, and only touches sysfs and /dev.
 *
 *   The first two are also the fragile ones. Adding a second RPC server, or
 *   starting a registry operation from a poller or the spawner, breaks the
 *   single-thread assumption silently: concurrent finds while another call
 *   frees an entry are a use-after-free, not a race that happens to work. If a
 *   new caller cannot guarantee this thread, it must bounce through
 *   spdk_thread_send_msg first or take the registry lock -- one or the other,
 *   never neither.
 */

#include "spdk/stdinc.h"
#include "spdk/crc32.h"
#include "spdk/json.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/string.h"
#include "spdk/util.h"

#include "vbdev_s3lvol.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_active)

#define ACTIVE_PATH_DEFAULT "/data/cubelet/rcow/active_lvols"
#define ACTIVE_PATH_ENV     "S3LVOL_ACTIVE_FILE"

/* Resolved through the environment rather than fixed at compile time, so that a
 * test suite can keep its own registry instead of sharing the one a production
 * instance on the same host is using. Two suites used to remove this file in
 * cleanup, which took a live instance's registry with it. */
#define ACTIVE_PATH (s3lvol_statefile_path(ACTIVE_PATH_ENV, ACTIVE_PATH_DEFAULT))

struct active_entry {
	struct s3lvol_active_entry pub;
	TAILQ_ENTRY(active_entry) link;
};

static TAILQ_HEAD(, active_entry) g_active = TAILQ_HEAD_INITIALIZER(g_active);

/* Set once the file has been read, so a load is not repeated on every lookup
 * and an empty registry is not mistaken for "not loaded yet". */
static bool g_loaded;

/* --------------------------------------------------------------------------
 * Placement
 * -------------------------------------------------------------------------- */

uint32_t
s3lvol_active_hash_subsys(const char *name)
{
	uint32_t crc;

	if (!name) {
		return 0;
	}

	/* Hashed from the name rather than the uuid on purpose. The uuid changes
	 * when a volume of the same name is recreated, whereas the name is what
	 * the caller and the orchestration layer both know -- so if this registry
	 * is ever lost, the placement can still be recomputed from names alone.
	 * The initial value is the customary all-ones. */
	crc = spdk_crc32c_update(name, strlen(name), ~0u);
	return crc % RCOW_NUM_SUBSYS;
}

uint32_t
s3lvol_active_alloc_nsid(uint32_t subsys)
{
	struct active_entry *e;
	bool taken[RCOW_NS_PER_SUBSYS + 1] = {};
	uint32_t nsid;

	TAILQ_FOREACH(e, &g_active, link) {
		if (e->pub.subsys == subsys && e->pub.nsid >= 1 &&
		    e->pub.nsid <= RCOW_NS_PER_SUBSYS) {
			taken[e->pub.nsid] = true;
		}
	}

	/* nsid is 1-based in NVMe. */
	for (nsid = 1; nsid <= RCOW_NS_PER_SUBSYS; nsid++) {
		if (!taken[nsid]) {
			return nsid;
		}
	}

	return 0;	/* subsystem full */
}

/* --------------------------------------------------------------------------
 * Serialisation
 *
 * Written through spdk_json_write rather than assembled with snprintf: a volume
 * name is caller-supplied, and a name containing a quote or a backslash would
 * turn hand-built JSON into a file that no longer parses -- discovered, as
 * always with this file, only by whoever is trying to recover.
 * -------------------------------------------------------------------------- */

struct json_buf {
	char   *data;
	size_t  len;
	size_t  cap;
};

static int
json_buf_append(void *cb_ctx, const void *data, size_t size)
{
	struct json_buf *b = cb_ctx;

	if (b->len + size + 1 > b->cap) {
		size_t cap = b->cap ? b->cap * 2 : 1024;
		char *grown;

		while (cap < b->len + size + 1) {
			cap *= 2;
		}
		grown = realloc(b->data, cap);
		if (!grown) {
			return -1;
		}
		b->data = grown;
		b->cap = cap;
	}

	memcpy(b->data + b->len, data, size);
	b->len += size;
	b->data[b->len] = '\0';
	return 0;
}

static int
active_flush(void)
{
	struct json_buf buf = {};
	struct spdk_json_write_ctx *w;
	struct active_entry *e;
	int rc;

	/* An empty registry is an empty file rather than "{}": nothing is active,
	 * and a recovery that finds no file behaves the same as one that finds no
	 * entries. */
	if (TAILQ_EMPTY(&g_active)) {
		return s3lvol_statefile_remove(ACTIVE_PATH);
	}

	w = spdk_json_write_begin(json_buf_append, &buf, SPDK_JSON_WRITE_FLAG_FORMATTED);
	if (!w) {
		free(buf.data);
		return -ENOMEM;
	}

	spdk_json_write_object_begin(w);
	TAILQ_FOREACH(e, &g_active, link) {
		spdk_json_write_named_object_begin(w, e->pub.name);
		spdk_json_write_named_string(w, "uuid", e->pub.uuid);
		spdk_json_write_named_uint32(w, "subsys", e->pub.subsys);
		spdk_json_write_named_uint32(w, "nsid", e->pub.nsid);
		spdk_json_write_object_end(w);
	}
	spdk_json_write_object_end(w);

	rc = spdk_json_write_end(w);
	if (rc != 0 || !buf.data) {
		SPDK_ERRLOG("active: failed to serialise the registry\n");
		free(buf.data);
		return rc ? rc : -ENOMEM;
	}

	rc = s3lvol_statefile_write(ACTIVE_PATH, buf.data);
	free(buf.data);
	return rc;
}

/* --------------------------------------------------------------------------
 * Parsing
 * -------------------------------------------------------------------------- */

struct entry_fields {
	char     *uuid;
	uint32_t  subsys;
	uint32_t  nsid;
};

static const struct spdk_json_object_decoder entry_decoders[] = {
	{"uuid",   offsetof(struct entry_fields, uuid),   spdk_json_decode_string, false},
	{"subsys", offsetof(struct entry_fields, subsys), spdk_json_decode_uint32, false},
	{"nsid",   offsetof(struct entry_fields, nsid),   spdk_json_decode_uint32, false},
};

/* Append without touching the file: used by the loader, which must not rewrite
 * what it is in the middle of reading. */
static int
active_insert(const char *name, const char *uuid, uint32_t subsys, uint32_t nsid)
{
	struct active_entry *e;

	e = calloc(1, sizeof(*e));
	if (!e) {
		return -ENOMEM;
	}

	snprintf(e->pub.name, sizeof(e->pub.name), "%s", name);
	snprintf(e->pub.uuid, sizeof(e->pub.uuid), "%s", uuid ? uuid : "");
	e->pub.subsys = subsys;
	e->pub.nsid   = nsid;

	TAILQ_INSERT_TAIL(&g_active, e, link);
	return 0;
}

int
s3lvol_active_load(void)
{
	char *text;
	struct spdk_json_val *values = NULL;
	ssize_t rc, num_values;
	struct spdk_json_val *it, *end;
	uint32_t loaded = 0, skipped = 0;

	if (g_loaded) {
		return 0;
	}

	text = s3lvol_statefile_read(ACTIVE_PATH);
	if (!text) {
		/* Absent or empty: nothing was active. */
		g_loaded = true;
		return 0;
	}

	/* First pass counts, second pass fills. */
	num_values = spdk_json_parse(text, strlen(text), NULL, 0, NULL, 0);
	if (num_values <= 0) {
		SPDK_ERRLOG("active: %s does not parse; refusing to guess. Move it "
			    "aside to continue without it\n", ACTIVE_PATH);
		free(text);
		return -EINVAL;
	}

	values = calloc((size_t)num_values, sizeof(*values));
	if (!values) {
		free(text);
		return -ENOMEM;
	}

	rc = spdk_json_parse(text, strlen(text), values, (size_t)num_values, NULL,
			     SPDK_JSON_PARSE_FLAG_DECODE_IN_PLACE);
	if (rc != num_values || values[0].type != SPDK_JSON_VAL_OBJECT_BEGIN) {
		SPDK_ERRLOG("active: %s is not a JSON object\n", ACTIVE_PATH);
		free(values);
		free(text);
		return -EINVAL;
	}

	/* Walk the top-level names. The keys are volume names, so they cannot be
	 * expressed as a decoder schema; each value is decoded individually.
	 *
	 * The NULL test is the whole reason this loop is written out rather than
	 * bounded by `end` alone: spdk_json_next() returns NULL, not a pointer to
	 * the closing brace, once the value it would step to is the enclosing
	 * OBJECT_END (lib/json/json_util.c:722). So after the last entry `it` is
	 * NULL, and `it < end` is true for it -- which dereferenced NULL at
	 * offset 12, the `type` field, and killed the target.
	 *
	 * It went unnoticed because nothing reached this line: the registry is
	 * read lazily, on the first activation RPC, and every test either started
	 * with no file or (like the recovery replay) removed it first, so the
	 * "file exists and parses" branch only ran the first time a restarted
	 * target was asked about a volume it had inherited -- which is precisely
	 * the case the file exists for.
	 */
	end = values + num_values;
	it = &values[1];
	while (it != NULL && it < end && it->type == SPDK_JSON_VAL_NAME) {
		struct spdk_json_val *obj = it + 1;
		struct entry_fields f = {};
		char name[SPDK_LVOL_NAME_MAX];

		if (it->len >= sizeof(name)) {
			SPDK_WARNLOG("active: entry name too long, skipped\n");
			skipped++;
			it = spdk_json_next(obj);
			continue;
		}
		memcpy(name, it->start, it->len);
		name[it->len] = '\0';

		if (spdk_json_decode_object(obj, entry_decoders,
					    SPDK_COUNTOF(entry_decoders), &f) != 0) {
			SPDK_WARNLOG("active: entry '%s' is malformed, skipped\n", name);
			skipped++;
			free(f.uuid);
			it = spdk_json_next(obj);
			continue;
		}

		if (active_insert(name, f.uuid, f.subsys, f.nsid) != 0) {
			SPDK_ERRLOG("active: out of memory loading '%s'\n", name);
			free(f.uuid);
			free(values);
			free(text);
			return -ENOMEM;
		}
		loaded++;
		free(f.uuid);
		it = spdk_json_next(obj);
	}

	free(values);
	free(text);
	g_loaded = true;

	SPDK_NOTICELOG("active: loaded %" PRIu32 " entries from %s%s\n", loaded,
		       ACTIVE_PATH,
		       skipped ? " (some entries were skipped, see above)" : "");
	return 0;
}

/* --------------------------------------------------------------------------
 * Lookup and mutation
 * -------------------------------------------------------------------------- */

const struct s3lvol_active_entry *
s3lvol_active_find(const char *name)
{
	struct active_entry *e;

	if (!name) {
		return NULL;
	}

	TAILQ_FOREACH(e, &g_active, link) {
		if (strcmp(e->pub.name, name) == 0) {
			return &e->pub;
		}
	}
	return NULL;
}

const struct s3lvol_active_entry *
s3lvol_active_find_by_nsid(uint32_t subsys, uint32_t nsid)
{
	struct active_entry *e;

	TAILQ_FOREACH(e, &g_active, link) {
		if (e->pub.subsys == subsys && e->pub.nsid == nsid) {
			return &e->pub;
		}
	}
	return NULL;
}

int
s3lvol_active_add(const char *name, const char *uuid, uint32_t subsys,
		  uint32_t nsid)
{
	struct active_entry *e;
	int rc;

	if (!name || !uuid) {
		return -EINVAL;
	}

	/* Update in place when it is already there, so that a re-activation does
	 * not accumulate duplicates. */
	TAILQ_FOREACH(e, &g_active, link) {
		if (strcmp(e->pub.name, name) == 0) {
			snprintf(e->pub.uuid, sizeof(e->pub.uuid), "%s", uuid);
			e->pub.subsys = subsys;
			e->pub.nsid   = nsid;
			return active_flush();
		}
	}

	rc = active_insert(name, uuid, subsys, nsid);
	if (rc != 0) {
		return rc;
	}

	rc = active_flush();
	if (rc != 0) {
		/* The file is the contract with recovery; an entry that only exists
		 * in memory would be forgotten by the next restart while the
		 * namespace stays exposed. Undo so the two agree. */
		TAILQ_FOREACH(e, &g_active, link) {
			if (strcmp(e->pub.name, name) == 0) {
				TAILQ_REMOVE(&g_active, e, link);
				free(e);
				break;
			}
		}
	}
	return rc;
}

int
s3lvol_active_remove(const char *name)
{
	struct active_entry *e;

	if (!name) {
		return -EINVAL;
	}

	TAILQ_FOREACH(e, &g_active, link) {
		if (strcmp(e->pub.name, name) == 0) {
			TAILQ_REMOVE(&g_active, e, link);
			free(e);
			return active_flush();
		}
	}
	return -ENOENT;
}

const struct s3lvol_active_entry *
s3lvol_active_first(void)
{
	struct active_entry *e = TAILQ_FIRST(&g_active);

	return e ? &e->pub : NULL;
}

const struct s3lvol_active_entry *
s3lvol_active_next(const struct s3lvol_active_entry *prev)
{
	struct active_entry *e;

	if (!prev) {
		return NULL;
	}
	e = SPDK_CONTAINEROF(prev, struct active_entry, pub);
	e = TAILQ_NEXT(e, link);
	return e ? &e->pub : NULL;
}
