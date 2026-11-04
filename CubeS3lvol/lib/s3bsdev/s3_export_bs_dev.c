/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_export_bs_dev -- the read-only bs_dev behind an imported export 
 *
 *   This is what makes an lvol on node B a clone of a snapshot on node A. It is
 *   handed to blobstore as an esnap parent: blobstore reads through it for any
 *   cluster the clone has not written yet, and copies out of it on the first
 *   write to such a cluster.
 *
 *   === What it is not ===
 *
 *   No WAL, no overlay, no flusher, no chunk map, no journal, no local device.
 *   The data it serves is already durable and immutable in S3, so all of that
 *   machinery has nothing to do. A first partial read fetches its whole object
 *   into a small RAM working set; concurrent reads of that object share the GET,
 *   and everything on the write side reports -EROFS.
 *
 *   === Threading ===
 *
 *   Unlike s3_bs_dev, reads are not bounced to an owner thread. The manifest is
 *   immutable once parsed. The bounded whole-object working set is shared by
 *   nvmf poll groups and protected by fill_lock; a waiter is delivered back to
 *   its submitting thread before its I/O aggregation state is touched.
 *
 *   === Lifetime ===
 *
 *   destroy() is called by blobstore, when it decides the back device is no
 *   longer referenced. That is why both the manifest and the S3 client are held
 *   by reference here: the import RPC that created this is long gone, and a
 *   release_export may have happened in between.
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/thread.h"
#include "spdk/util.h"

#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_cache.h"
#include "s3lvol/s3_export.h"

#define EXPORT_FILL_MAX_GETTING 64
#define EXPORT_FILL_MAX_READY   128
#define EXPORT_FILL_MAX_LIVE    (EXPORT_FILL_MAX_GETTING + EXPORT_FILL_MAX_READY)
/* Bound one synchronous manifest walk so a sparse template cannot monopolise
 * its SPDK reactor. A poller resumes the walk after this many examined chunks. */
#define EXPORT_PREHEAT_SCAN_BUDGET 1024

struct s3_export_fill;
struct s3_export_io;

struct s3_export_fill_waiter {
	struct s3_export_fill      *fill;
	struct s3_export_io        *io;
	void                       *dst;
	uint32_t                    offset;
	uint32_t                    length;
	struct spdk_thread         *origin;
	TAILQ_ENTRY(s3_export_fill_waiter) link;
};

TAILQ_HEAD(s3_export_fill_waiters, s3_export_fill_waiter);

enum s3_export_fill_state {
	EXPORT_FILL_QUEUED = 0,
	EXPORT_FILL_GETTING,
	EXPORT_FILL_READY,
};

struct s3_export_fill {
	struct s3_export_dev       *dev;
	char                        key[S3_EXPORT_KEY_MAX];
	uint64_t                    chunk_index;
	uint32_t                    object_len;
	void                       *buf;
	/* A sole full-object demand waiter can lend its payload to the GET. If a
	 * second waiter joins, completion first turns this back into owned staging
	 * so cross-thread delivery never reads a payload whose callback released
	 * it. */
	struct s3_export_fill_waiter *direct_waiter;
	bool                        buf_owned;
	enum s3_export_fill_state   state;
	int                         status;
	uint32_t                    users;
	bool                        listed;
	bool                        on_lru;
	bool                        on_pending;
	bool                        token_held;
	bool                        lifetime_ref;
	/* Template preheat, not a blobstore read. A demand that joins this GET
	 * still completes normally; with no waiter the buffer stays listed only
	 * until the object cache has accepted it, so a demand read arriving in
	 * that disk-fill window copies the completed GET instead of re-fetching
	 * the object, while post-preheat reads still come from the object cache. */
	bool                        preheat;
	bool                        populate_wait;
	bool                        preheat_detach;
	bool                        preheat_had_waiters;
	bool                        preheat_ready_hit;
	int                         populate_status;
	uint64_t                    completion_bytes;
	int                         completion_status;
	bool                        preheat_read_done_published;
	bool                        preheat_finish_deferred;
	struct s3_cache_object_reservation *cache_reservation;
	struct s3_export_fill_waiters waiters;
	TAILQ_ENTRY(s3_export_fill) link;
	TAILQ_ENTRY(s3_export_fill) lru_link;
	TAILQ_ENTRY(s3_export_fill) pending_link;
};

TAILQ_HEAD(s3_export_fills, s3_export_fill);

struct s3_export_channel {
	struct spdk_io_channel *cache_ch;
};

struct s3_export_dev {
	/* Must be first: blobstore only ever holds &dev->bs_dev, and the cast
	 * back relies on the addresses being the same. */
	struct spdk_bs_dev         bs_dev;

	struct s3_client          *client;
	struct s3_export_manifest *m;
	struct s3_cache           *shared_cache;
	char                       cache_endpoint[S3_EXPORT_ENDPOINT_MAX];
	char                       cache_bucket[S3_EXPORT_BUCKET_MAX];

	s3_export_bs_dev_on_swap_fn on_swap;
	void                      *on_swap_arg;

	uint32_t                   chunk_shift;
	uint32_t                   blocks_per_chunk;
	uint64_t                   current_generation;

	/* io_device name, which spdk_io_device_register() keeps a pointer to. */
	char                       name[SPDK_UUID_STRING_LEN + 16];

	/* Where the refetch machinery runs, and the only thread that touches the
	 * refetch queues and poller below.
	 *
	 * The manifest and callbacks are otherwise read-only after construction.
	 * (The separate fill table has its own short-held mutex.) A refetch is
	 * different: several threads can hit 404 at the same moment on the same
	 * export, and they must produce one refetch between them rather than one
	 * each. Rather than use that mutex for network state, the refetch lives on
	 * one thread and the others send it a message.
	 *
	 * The thread that created the device, since that is the one that will still
	 * be there: blobstore's channels come and go with the poll groups. */
	struct spdk_thread        *home_thread;
	bool                       refetch_in_flight;
	TAILQ_HEAD(, s3_export_io) refetch_waiters;
	TAILQ_HEAD(, s3_export_io) refetch_delivery_waiters;
	struct spdk_poller        *refetch_delivery_poller;
	bool                       refetch_delivery_lifetime_ref;

	/* Whole-object GET single-flight and its small post-GET working set.
	 * The key, rather than chunk_index, is the identity: a manifest replacement
	 * can make one logical chunk name a different immutable object. */
	pthread_mutex_t            fill_lock;
	struct s3_export_fills     fills;
	struct s3_export_fills     fill_lru;
	struct s3_export_fills     fill_pending;
	uint32_t                   fills_getting;
	uint32_t                   fills_ready;
	uint32_t                   fills_live;

	_Atomic bool               destroying;
	bool                       unregister_started;
	uint32_t                   async_refs;

	/* Incremented from nvmf I/O threads as well as the home thread. */
	uint64_t                   reads;
	uint64_t                   bytes_read;
	uint64_t                   zero_fills;
	uint64_t                   refetches;
	uint64_t                   whole_gets;
	uint64_t                   coalesced_reads;
	uint64_t                   ready_hits;
	uint64_t                   exact_fallbacks;
	uint64_t                   shared_cache_hits;
	uint64_t                   shared_cache_misses;
	uint64_t                   shared_cache_fallbacks;

	/* One walk of this device's manifest into the lvstore object cache.
	 * Started at most once. In-flight GETs count toward preheat_inflight
	 * until the cache accepts or refuses them. */
	uint64_t                   preheat_cursor;
	uint64_t                   preheat_retry;
	uint32_t                   preheat_inflight;
	bool                       preheat_started;
	bool                       preheat_idle_logged;
	_Atomic bool               preheat_stop;
	bool                       preheat_has_retry;
	bool                       preheat_pumping;
	bool                       preheat_lifetime_ref;
	struct spdk_poller        *preheat_poller;
};

/* One bs_dev read, possibly spanning several chunks. */
struct s3_export_io {
	struct s3_export_dev       *dev;
	struct s3_export_manifest  *m;
	struct spdk_bs_dev_cb_args *cb_args;

	uint32_t                    num_pending;
	int                         status;

	/* One device-lifetime reference belongs to every blobstore read, from
	 * admission until its completion callback returns. A whole-object fill has
	 * a separate reference for its GET lifetime: that one can end
	 * after merely queueing a cross-thread waiter, while blobstore still owns
	 * the read and may still be using the external parent. */
	bool                        lifetime_ref;

	/* Stops the first sub-read from completing the whole I/O while the split
	 * loop is still adding to it. */
	bool                        submit_done;

	/* Enough to run the read again after the manifest has been replaced.
	 *
	 * The payload belongs to blobstore and stays valid until cb_fn is called,
	 * which is exactly what a retry postpones -- so holding it across the
	 * refetch is allowed by the same contract that makes the ordinary
	 * asynchronous completion legal. */
	void                       *payload;
	uint64_t                    lba;
	uint32_t                    lba_count;

	/* The thread this I/O was submitted on. The retry has to go back to it:
	 * cb_args->cb_fn is blobstore's, and blobstore expects it on the thread
	 * that issued the read. */
	struct spdk_thread         *origin;

	/* Set when a sub-read returned 404. It means the objects this manifest
	 * names are gone, which is what the source materialising the export looks
	 * like from here -- so the I/O waits for a newer manifest instead of
	 * failing. Once retried it is cleared, and a second 404 is a real one. */
	bool                        saw_missing;
	bool                        retried;

	TAILQ_ENTRY(s3_export_io)   waiter_link;
};

struct s3_export_cache_io {
	struct s3_export_io *io;
	char                 key[S3_EXPORT_KEY_MAX];
	uint64_t             chunk_index;
	uint32_t             object_len;
	uint32_t             offset;
	uint32_t             get_len;
	void                *dst;
};

struct s3_export_chunk_io {
	struct s3_export_io *io;
	uint64_t             chunk_index;

	/* How many bytes this sub-read asked for. Kept so the completion can tell a
	 * short read from a complete one: the buffer it was given is only filled as
	 * far as the object store went, and the rest keeps whatever was there. */
	uint32_t             expected;
	uint32_t             object_len;
	uint32_t             offset;
	void                *dst;

	char                 key[S3_EXPORT_KEY_MAX];
};

static void export_io_device_unregistered(void *io_device);
static void export_chunk_read_done(void *cb_arg, uint64_t bytes_read, int status);

static bool
export_async_get_live(struct s3_export_dev *dev)
{
	bool live;

	pthread_mutex_lock(&dev->fill_lock);
	live = !dev->destroying;
	if (live) {
		dev->async_refs++;
	}
	pthread_mutex_unlock(&dev->fill_lock);
	return live;
}

static void
export_async_put(struct s3_export_dev *dev)
{
	bool unregister = false;

	pthread_mutex_lock(&dev->fill_lock);
	assert(dev->async_refs > 0);
	dev->async_refs--;
	if (dev->destroying && dev->async_refs == 0 && !dev->unregister_started) {
		dev->unregister_started = true;
		unregister = true;
	}
	pthread_mutex_unlock(&dev->fill_lock);

	if (unregister) {
		spdk_io_device_unregister(dev, export_io_device_unregistered);
	}
}

/* ==========================================================================
 * Key derivation
 *
 * The one place the two layouts differ on the read side. Everything else --
 * splitting, holes, channels, refusing writes -- is the same for both.
 * ========================================================================== */

static void
export_chunk_key(const struct s3_export_manifest *m, uint64_t chunk_index,
		 char *out, size_t out_len)
{
	const struct s3_export_ref *ref;

	if (m->layout == S3_EXPORT_LAYOUT_REF) {
		const char *prefix;

		/* A live object under whichever lvstore's prefix owns it. Built with the
		 * chunk map's own key function rather than a copy of the format, because
		 * reading a *different* key than the source writes would not fail -- it
		 * would either 404 or, worse, find something else.
		 *
		 * The prefix is per chunk, not per manifest: a snapshot taken on an
		 * imported volume owns some chunks and inherited the rest, so they live
		 * under different prefixes. chunk_prefix() answers src.prefix for every
		 * chunk of a single-source manifest, which is every version 2 one, so
		 * there is nothing to branch on here.
		 *
		 * An empty answer means the manifest cannot say -- an index outside its
		 * source table, which parse refuses, so only a manifest built in memory
		 * can be in that state. Treated as no key at all rather than falling
		 * back to src.prefix: a wrong prefix finds another chunk's object and
		 * returns bytes that look perfectly good. */
		ref = s3_export_manifest_get_ref(m, chunk_index);
		prefix = s3_export_manifest_chunk_prefix(m, chunk_index);
		if (ref && prefix[0] != '\0') {
			s3_chunk_data_key(prefix, &ref->uuid, out, out_len);
			return;
		}
		out[0] = '\0';
		return;
	}

	s3_export_chunk_key(m->src.prefix, m->uuid_str, chunk_index,
			    out, out_len);
}

/* ==========================================================================
 * Completion
 * ========================================================================== */

static void export_io_start_refetch(struct s3_export_io *io);
static void export_read_internal(struct spdk_bs_dev *bs_dev,
				 struct spdk_io_channel *channel, void *payload,
				 uint64_t lba, uint32_t lba_count,
				 struct spdk_bs_dev_cb_args *cb_args, bool is_retry);

static void
export_io_drop(struct s3_export_io *io)
{
	if (io->m) {
		s3_export_manifest_unref(io->m);
	}
	free(io);
}

static void
export_io_complete(struct s3_export_io *io)
{
	struct s3_export_dev *dev = io->dev;
	struct spdk_bs_dev_cb_args *cb_args = io->cb_args;
	int status = io->status;
	bool lifetime_ref = io->lifetime_ref;

	/* A chunk was missing and nothing worse went wrong: hold the I/O and go
	 * looking for a newer manifest instead of failing it. Checked here rather
	 * than per sub-read so that every sub-read of this I/O has finished first --
	 * retrying with some of them still in flight would have two of them writing
	 * into the same buffer.
	 *
	 * status is honoured first because a real error is a worse answer than a
	 * stale manifest, and refetching would not fix it. */
	if (status == 0 && io->saw_missing && !io->retried) {
		export_io_start_refetch(io);
		return;
	}

	/* Retried and still missing. The manifest is current, so the objects really
	 * are gone -- the source deleted the snapshot rather than materialising it. */
	if (status == 0 && io->saw_missing) {
		SPDK_ERRLOG("export %s: chunks are still missing after refetching the "
			    "manifest; the source has deleted them\n",
			    io->m ? io->m->uuid_str : io->dev->m->uuid_str);
		status = -ENOENT;
	}

	export_io_drop(io);
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, status);
	if (lifetime_ref) {
		/* The callback may synchronously hot-unplug this parent. Keep dev
		 * alive until it has returned, then let the last read finish
		 * unregistering it. */
		export_async_put(dev);
	}
}

static void
export_io_put(struct s3_export_io *io)
{
	assert(io->num_pending > 0);
	if (--io->num_pending == 0 && io->submit_done) {
		export_io_complete(io);
	}
}

/* ==========================================================================
 * Whole-object GET single-flight
 * ========================================================================== */

static struct s3_export_fill *
export_fill_find_locked(struct s3_export_dev *dev, const char *key,
			uint32_t object_len)
{
	struct s3_export_fill *fill;

	TAILQ_FOREACH(fill, &dev->fills, link) {
		if (fill->object_len == object_len && strcmp(fill->key, key) == 0) {
			return fill;
		}
	}
	return NULL;
}

static void
export_fill_free(struct s3_export_fill *fill)
{
	if (fill->buf_owned) {
		free(fill->buf);
	}
	free(fill);
}

/* Called with fill_lock held. A pinned entry is passed over; the limit is a
 * target rather than a reason to free bytes a waiter is about to copy. The last
 * waiter retries eviction when it drops its pin. */
static void
export_fill_evict_locked(struct s3_export_dev *dev)
{
	struct s3_export_fill *fill, *tmp;

	TAILQ_FOREACH_SAFE(fill, &dev->fill_lru, lru_link, tmp) {
		if (dev->fills_ready <= EXPORT_FILL_MAX_READY) {
			break;
		}
		if (fill->users != 0 || fill->populate_wait) {
			continue;
		}
		TAILQ_REMOVE(&dev->fill_lru, fill, lru_link);
		TAILQ_REMOVE(&dev->fills, fill, link);
		fill->on_lru = false;
		fill->listed = false;
		dev->fills_ready--;
		assert(dev->fills_live > 0);
		dev->fills_live--;
		export_fill_free(fill);
	}
}

static bool
export_fill_evict_one_locked(struct s3_export_dev *dev)
{
	struct s3_export_fill *fill;

	TAILQ_FOREACH(fill, &dev->fill_lru, lru_link) {
		if (fill->users != 0 || fill->populate_wait) {
			continue;
		}
		TAILQ_REMOVE(&dev->fill_lru, fill, lru_link);
		TAILQ_REMOVE(&dev->fills, fill, link);
		fill->on_lru = false;
		fill->listed = false;
		assert(dev->fills_ready > 0);
		dev->fills_ready--;
		assert(dev->fills_live > 0);
		dev->fills_live--;
		export_fill_free(fill);
		return true;
	}
	return false;
}

static void
export_fill_waiter_release_pin(struct s3_export_fill_waiter *waiter, bool put_io)
{
	struct s3_export_fill *fill = waiter->fill;
	struct s3_export_io *io = waiter->io;
	struct s3_export_dev *dev = fill->dev;

	pthread_mutex_lock(&dev->fill_lock);
	assert(fill->users > 0);
	fill->users--;
	if (!fill->listed && fill->users == 0) {
		assert(dev->fills_live > 0);
		dev->fills_live--;
		pthread_mutex_unlock(&dev->fill_lock);
		export_fill_free(fill);
	} else {
		export_fill_evict_locked(dev);
		pthread_mutex_unlock(&dev->fill_lock);
	}

	free(waiter);
	if (put_io) {
		export_io_put(io);
	}
}

static void
export_fill_waiter_deliver(void *arg)
{
	struct s3_export_fill_waiter *waiter = arg;
	struct s3_export_fill *fill = waiter->fill;
	struct s3_export_io *io = waiter->io;
	struct s3_export_dev *dev = fill->dev;
	void *src;
	int status;

	assert(waiter->origin == NULL || waiter->origin == spdk_get_thread());

	/* Besides protecting the entry, this acquire pairs with fill_done's
	 * publication of status and the completed staging buffer. users pins the
	 * entry after the lock is dropped for the potentially large memcpy. */
	pthread_mutex_lock(&dev->fill_lock);
	status = fill->status;
	src = fill->buf;
	pthread_mutex_unlock(&dev->fill_lock);

	if (status == -ENOENT && !io->retried) {
		io->saw_missing = true;
	} else if (status != 0) {
		SPDK_ERRLOG("Failed to read export chunk '%s': %s\n",
			    fill->key, spdk_strerror(-status));
		if (io->status == 0) {
			io->status = status;
		}
	} else if (waiter != fill->direct_waiter) {
		memcpy(waiter->dst, (uint8_t *)src + waiter->offset,
		       waiter->length);
	}

	export_fill_waiter_release_pin(waiter, true);
}

static void
export_fill_waiter_finish(void *arg)
{
	struct s3_export_fill_waiter *waiter = arg;
	struct s3_export_fill *fill = waiter->fill;
	struct s3_export_io *io = waiter->io;
	struct s3_export_chunk_io *cio;
	int status;
	int rc;

	assert(waiter->origin == NULL || waiter->origin == spdk_get_thread());

	pthread_mutex_lock(&fill->dev->fill_lock);
	status = fill->status;
	pthread_mutex_unlock(&fill->dev->fill_lock);

	/* Staging OOM after admission should not fail the read: fall back to
	 * the exact range GET. Short whole-object answers stay -EIO so they
	 * cannot be served as zeroes. 404 still goes through deliver so the
	 * I/O can refetch the manifest. */
	if (status != -ENOMEM) {
		export_fill_waiter_deliver(waiter);
		return;
	}

	cio = calloc(1, sizeof(*cio));
	if (!cio) {
		export_fill_waiter_deliver(waiter);
		return;
	}
	cio->io = io;
	cio->chunk_index = fill->chunk_index;
	cio->expected = waiter->length;
	cio->object_len = fill->object_len;
	cio->offset = waiter->offset;
	cio->dst = waiter->dst;
	snprintf(cio->key, sizeof(cio->key), "%s", fill->key);
	rc = s3_get_range(fill->dev->client, cio->key, waiter->offset,
			  waiter->length, waiter->dst, export_chunk_read_done, cio);
	if (rc != 0) {
		free(cio);
		export_fill_waiter_deliver(waiter);
		return;
	}
	__atomic_fetch_add(&fill->dev->exact_fallbacks, 1, __ATOMIC_RELAXED);
	export_fill_waiter_release_pin(waiter, false);
}

static void export_fill_request_token(struct s3_export_fill *fill);
static void export_preheat_cached(void *cb_arg, int status);
static void export_preheat_finish(void *arg);
static void export_preheat_pump(struct s3_export_dev *dev);
static void export_fill_read_done(void *cb_arg, uint64_t bytes_read, int status);

static void
export_preheat_read_done_msg(void *arg)
{
	struct s3_export_fill *fill = arg;

	export_fill_read_done(fill, fill->completion_bytes,
			      fill->completion_status);
}

/* A failed fill may complete after the cursor has advanced far beyond it.
 * Keep the earliest retry: rewinding the cursor there rechecks every later
 * object, so several concurrent failures cannot overwrite and lose one
 * another. Already-resident objects are skipped by cache reservation. */
static void
export_preheat_retry_from(struct s3_export_dev *dev, uint64_t chunk)
{
	if (!dev->preheat_has_retry || chunk < dev->preheat_retry) {
		dev->preheat_retry = chunk;
	}
	dev->preheat_has_retry = true;
}

static bool
export_preheat_get_live(struct s3_export_dev *dev)
{
	bool live;

	pthread_mutex_lock(&dev->fill_lock);
	live = !dev->destroying && !dev->preheat_lifetime_ref;
	if (live) {
		dev->preheat_lifetime_ref = true;
		dev->async_refs++;
	}
	pthread_mutex_unlock(&dev->fill_lock);
	return live;
}

/* Transfer the walk's device reference to the caller for a final async_put.
 * Completion, poller, and destroy paths may race here, so claiming it is part
 * of the same lock domain as async_refs. */
static bool
export_preheat_take_lifetime(struct s3_export_dev *dev)
{
	bool held;

	pthread_mutex_lock(&dev->fill_lock);
	held = dev->preheat_lifetime_ref;
	if (held) {
		dev->preheat_lifetime_ref = false;
	}
	pthread_mutex_unlock(&dev->fill_lock);
	return held;
}

static void
export_fill_read_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct s3_export_fill *fill = cb_arg;
	struct s3_export_dev *dev = fill->dev;
	struct s3_export_fill *next = NULL;
	struct s3_export_fill_waiters waiters;
	struct s3_export_fill_waiter *waiter;
	bool release_token;
	bool lifetime_ref;
	bool free_failed = false;
	bool preheat_detach = false;
	bool preheat_bounce_failed = false;
	bool fill_preheat;
	bool fill_populate_wait;
	bool finish_deferred;
	uint64_t fill_chunk_index;
	bool release_preheat_ref = false;
	int populate_rc = 0;

	/* Preheat cursor, retry, inflight, poller and walk-lifetime state all
	 * belong to the device's home thread. S3 clients may deliver a completion
	 * on another thread, so transfer the GET's existing lifetime reference
	 * with the message before touching any of that state. */
	if (fill->preheat && dev->home_thread != NULL &&
	    dev->home_thread != spdk_get_thread()) {
		fill->completion_bytes = bytes_read;
		fill->completion_status = status;
		if (spdk_thread_send_msg(dev->home_thread,
					 export_preheat_read_done_msg, fill) == 0) {
			return;
		}
		/* A stopped home thread cannot continue an optional walk. The
		 * fallback below still retires the GET and, during destroy, the
		 * walk reference. */
		SPDK_ERRLOG("Could not bounce export %s preheat completion to its "
			    "home thread; stopping the walk\n", dev->m->uuid_str);
		/* Do not start another asynchronous cache operation from the wrong
		 * thread. Treat this optional GET as cancelled and retire all of its
		 * state below under fill_lock. */
		preheat_bounce_failed = true;
		status = -ESHUTDOWN;
	}

	TAILQ_INIT(&waiters);
	if (status == 0 && bytes_read != fill->object_len) {
		SPDK_ERRLOG("Short read of export chunk '%s': asked for %u byte(s), "
			    "got %" PRIu64 ". Refusing to cache or serve it.\n",
			    fill->key, fill->object_len, bytes_read);
		status = -EIO;
	}
	if (fill->preheat && status != 0 && fill->cache_reservation) {
		s3_cache_object_reservation_cancel(fill->cache_reservation);
		fill->cache_reservation = NULL;
	}
	if (status == 0 && dev->shared_cache) {
		struct s3_cache_object_id id = {
			.endpoint = dev->cache_endpoint,
			.bucket = dev->cache_bucket,
			.key = fill->key,
		};

		if (fill->preheat) {
			populate_rc = s3_cache_object_populate_reserved_notify(
				fill->cache_reservation, fill->buf, fill->object_len,
				export_preheat_cached, fill);
			fill->cache_reservation = NULL;
			fill->populate_wait = populate_rc == 0;
			fill->populate_status = populate_rc;
		} else {
			s3_cache_object_populate(dev->shared_cache, &id, 0, fill->buf,
						 fill->object_len, fill->object_len);
		}
	}

	pthread_mutex_lock(&dev->fill_lock);
	/* Waiter delivery can run on different SPDK threads. A direct payload is
	 * safe only for its sole owner; otherwise preserve the old shared-staging
	 * lifetime before any callback can release that payload. */
	if (status == 0 && fill->direct_waiter && fill->users > 1) {
		void *staging = malloc(fill->object_len);

		if (staging) {
			memcpy(staging, fill->buf, fill->object_len);
			fill->buf = staging;
			fill->buf_owned = true;
			fill->direct_waiter = NULL;
		} else {
			status = -ENOMEM;
			fill->direct_waiter = NULL;
		}
	}
	assert(fill->state == EXPORT_FILL_GETTING);
	assert(dev->fills_getting > 0);
	dev->fills_getting--;
	release_token = fill->token_held;
	fill->token_held = false;
	fill->status = status;

	while ((waiter = TAILQ_FIRST(&fill->waiters)) != NULL) {
		TAILQ_REMOVE(&fill->waiters, waiter, link);
		TAILQ_INSERT_TAIL(&waiters, waiter, link);
	}
	/* A preheat with nobody waiting keeps its lifetime ref and its RAM
	 * entry until the cache callback. A demand read that arrives while the
	 * cache fill is queued or writing then hits READY instead of issuing a
	 * duplicate GET; once the cache fill finishes, an unused preheat entry
	 * is removed so later reads go through the object cache. */
	fill->preheat_had_waiters = !TAILQ_EMPTY(&waiters);
	preheat_detach = fill->preheat && fill->populate_wait &&
			 !fill->preheat_had_waiters;
	fill->preheat_detach = preheat_detach;
	if (fill->preheat && fill->populate_wait) {
		lifetime_ref = false;
	} else {
		lifetime_ref = fill->lifetime_ref;
		fill->lifetime_ref = false;
	}

	if (status == 0) {
		bool keep = fill->direct_waiter == NULL &&
			    (!fill->preheat || fill->preheat_had_waiters ||
			     fill->populate_wait);

		fill->state = EXPORT_FILL_READY;
		dev->whole_gets++;
		__atomic_fetch_add(&dev->bytes_read, bytes_read, __ATOMIC_RELAXED);
		if (keep) {
			fill->on_lru = true;
			TAILQ_INSERT_TAIL(&dev->fill_lru, fill, lru_link);
			dev->fills_ready++;
			export_fill_evict_locked(dev);
		} else {
			TAILQ_REMOVE(&dev->fills, fill, link);
			fill->listed = false;
			if (fill->users == 0) {
				assert(dev->fills_live > 0);
				dev->fills_live--;
				free_failed = true;
			}
		}
	} else {
		TAILQ_REMOVE(&dev->fills, fill, link);
		fill->listed = false;
		if (fill->users == 0) {
			assert(dev->fills_live > 0);
			dev->fills_live--;
			free_failed = true;
		}
	}
	fill->preheat_read_done_published = true;
	finish_deferred = fill->preheat_finish_deferred;
	fill->preheat_finish_deferred = false;
	fill_preheat = fill->preheat;
	fill_populate_wait = fill->populate_wait;
	fill_chunk_index = fill->chunk_index;
	if (dev->fills_getting < EXPORT_FILL_MAX_GETTING) {
		next = TAILQ_FIRST(&dev->fill_pending);
		if (next) {
			TAILQ_REMOVE(&dev->fill_pending, next, pending_link);
		}
		if (next) {
			next->on_pending = false;
			next->state = EXPORT_FILL_GETTING;
			dev->fills_getting++;
		}
	}
	pthread_mutex_unlock(&dev->fill_lock);

	if (release_token) {
		s3_whole_get_token_release();
	}
	if (next) {
		export_fill_request_token(next);
	}

	while ((waiter = TAILQ_FIRST(&waiters)) != NULL) {
		int rc;

		TAILQ_REMOVE(&waiters, waiter, link);
		if (waiter->origin == NULL || waiter->origin == spdk_get_thread()) {
			export_fill_waiter_finish(waiter);
			continue;
		}
		rc = spdk_thread_send_msg(waiter->origin,
					 export_fill_waiter_finish, waiter);
		if (rc != 0) {
			/* Same last-resort rule as ingest/refetch: losing the completion
			 * hangs blobstore forever, which is worse than delivering it on
			 * the current thread after the origin has stopped accepting work. */
			SPDK_ERRLOG("export fill completion bounce failed: %s\n",
				    spdk_strerror(-rc));
			waiter->origin = spdk_get_thread();
			export_fill_waiter_finish(waiter);
		}
	}
	if (fill_preheat && !fill_populate_wait) {
		if (preheat_bounce_failed) {
			pthread_mutex_lock(&dev->fill_lock);
			if (dev->preheat_inflight > 0) {
				dev->preheat_inflight--;
			}
			dev->preheat_stop = true;
			if (dev->preheat_inflight == 0 &&
			    dev->preheat_lifetime_ref) {
				dev->preheat_lifetime_ref = false;
				release_preheat_ref = true;
			}
			pthread_mutex_unlock(&dev->fill_lock);
		} else {
			if (dev->preheat_inflight > 0) {
				dev->preheat_inflight--;
			}
			if (populate_rc == -ENOSPC ||
			    populate_rc == -ESHUTDOWN) {
				dev->preheat_stop = true;
			} else if (populate_rc == -EAGAIN) {
				export_preheat_retry_from(dev, fill_chunk_index);
			}
		}
	}
	if (free_failed) {
		export_fill_free(fill);
		fill = NULL;
	}
	if (finish_deferred) {
		export_preheat_finish(fill);
		return;
	}
	/* Demand completions arrive on the thread that submitted them. The
	 * walk's cursor and token retry belong to the creating thread. */
	if (!preheat_bounce_failed) {
		if (dev->preheat_started && !dev->destroying &&
		    (dev->home_thread == NULL ||
		     dev->home_thread == spdk_get_thread())) {
			export_preheat_pump(dev);
		} else if ((dev->destroying || dev->preheat_stop) &&
			   dev->preheat_inflight == 0) {
			release_preheat_ref = export_preheat_take_lifetime(dev);
		}
	}
	/* A concurrent destroy can free dev here. */
	if (release_preheat_ref) {
		export_async_put(dev);
	}
	/* This GET reference can be the final device pin. Keep it until every
	 * completion-side access to dev has finished. */
	if (lifetime_ref) {
		export_async_put(dev);
	}
}

static void
export_preheat_finish_aborted(struct s3_export_fill *fill, int status)
{
	struct s3_export_dev *dev = fill->dev;
	bool detach;
	bool held = false;
	bool free_fill = false;
	bool release_preheat_ref = false;

	pthread_mutex_lock(&dev->fill_lock);
	detach = fill->preheat_detach;
	fill->populate_wait = false;
	if (fill->lifetime_ref) {
		fill->lifetime_ref = false;
		held = true;
	}
	if (detach && status == 0 && !fill->preheat_ready_hit &&
	    fill->users == 0) {
		if (fill->on_lru) {
			TAILQ_REMOVE(&dev->fill_lru, fill, lru_link);
			fill->on_lru = false;
			assert(dev->fills_ready > 0);
			dev->fills_ready--;
		}
		if (fill->listed) {
			TAILQ_REMOVE(&dev->fills, fill, link);
			fill->listed = false;
			assert(dev->fills_live > 0);
			dev->fills_live--;
			free_fill = true;
		}
	} else {
		export_fill_evict_locked(dev);
	}
	if (dev->preheat_inflight > 0) {
		dev->preheat_inflight--;
	}
	dev->preheat_stop = true;
	if (dev->preheat_inflight == 0 && dev->preheat_lifetime_ref) {
		dev->preheat_lifetime_ref = false;
		release_preheat_ref = true;
	}
	pthread_mutex_unlock(&dev->fill_lock);

	if (free_fill) {
		export_fill_free(fill);
	}
	if (held) {
		export_async_put(dev);
	}
	/* May complete destroy; no device access is allowed after this put. */
	if (release_preheat_ref) {
		export_async_put(dev);
	}
}

static void
export_preheat_cached(void *cb_arg, int status)
{
	struct s3_export_fill *fill = cb_arg;
	struct s3_export_dev *dev = fill->dev;
	bool published;
	int rc;

	fill->populate_status = status;
	pthread_mutex_lock(&dev->fill_lock);
	published = fill->preheat_read_done_published;
	if (!published) {
		fill->preheat_finish_deferred = true;
	}
	pthread_mutex_unlock(&dev->fill_lock);
	if (!published) {
		return;
	}
	if (!dev->home_thread || dev->home_thread == spdk_get_thread()) {
		export_preheat_finish(fill);
		return;
	}
	rc = spdk_thread_send_msg(dev->home_thread, export_preheat_finish, fill);
	if (rc != 0) {
		SPDK_ERRLOG("Could not bounce export %s cache completion to its "
			    "home thread; stopping the walk\n", dev->m->uuid_str);
		export_preheat_finish_aborted(fill, status);
	}
}

static void
export_preheat_finish(void *arg)
{
	struct s3_export_fill *fill = arg;
	struct s3_export_dev *dev = fill->dev;
	bool detach = fill->preheat_detach;
	int status = fill->populate_status;
	bool held = false;
	bool free_fill = false;
	bool release_preheat_ref = false;

	pthread_mutex_lock(&dev->fill_lock);
	fill->populate_wait = false;
	if (fill->lifetime_ref) {
		fill->lifetime_ref = false;
		held = true;
	}
	if (detach && status == 0 && !fill->preheat_ready_hit &&
	    fill->users == 0) {
		if (fill->on_lru) {
			TAILQ_REMOVE(&dev->fill_lru, fill, lru_link);
			fill->on_lru = false;
			assert(dev->fills_ready > 0);
			dev->fills_ready--;
		}
		if (fill->listed) {
			TAILQ_REMOVE(&dev->fills, fill, link);
			fill->listed = false;
			assert(dev->fills_live > 0);
			dev->fills_live--;
			free_fill = true;
		}
	} else {
		export_fill_evict_locked(dev);
	}
	pthread_mutex_unlock(&dev->fill_lock);

	if (dev->preheat_inflight > 0) {
		dev->preheat_inflight--;
	}
	if (status == -ENOSPC || status == -ESHUTDOWN) {
		dev->preheat_stop = true;
	} else if (status == -EAGAIN && !dev->preheat_stop) {
		export_preheat_retry_from(dev, fill->chunk_index);
	}
	if (free_fill) {
		export_fill_free(fill);
	}
	if (!dev->destroying) {
		export_preheat_pump(dev);
	} else if (dev->preheat_inflight == 0) {
		release_preheat_ref = export_preheat_take_lifetime(dev);
	}
	if (release_preheat_ref) {
		export_async_put(dev);
	}
	/* Either put may synchronously free dev; the callback's reference is
	 * dropped last so no operation below can dereference it. */
	if (held) {
		export_async_put(dev);
	}
}

static void
export_preheat_msg(void *arg)
{
	struct s3_export_dev *dev = arg;

	(void)s3_export_bs_dev_preheat(&dev->bs_dev);
	/* The queued message owns a lifetime reference. This must be the final
	 * access: destroy may already be waiting for this put to free dev. */
	export_async_put(dev);
}

static void
export_preheat_token_unused(void *arg)
{
	/* Low-priority acquire does not queue, so this does not run. If it
	 * ever does, the token must not stick with no GET behind it. */
	(void)arg;
	s3_whole_get_token_release();
}

static int
export_preheat_poll(void *arg)
{
	struct s3_export_dev *dev = arg;
	bool callback_ref;
	bool release_preheat_ref = false;

	/* The walk reference normally pins this callback, but pump may retire it
	 * after a concurrent destroy. A transient callback reference keeps the
	 * remainder of this function safe in that race. */
	callback_ref = export_async_get_live(dev);
	if (dev->destroying || dev->preheat_stop) {
		spdk_poller_unregister(&dev->preheat_poller);
		if (dev->preheat_inflight == 0) {
			release_preheat_ref = export_preheat_take_lifetime(dev);
		}
		if (release_preheat_ref) {
			export_async_put(dev);
		}
		if (callback_ref) {
			export_async_put(dev);
		}
		return SPDK_POLLER_IDLE;
	}
	export_preheat_pump(dev);
	/* A retry can be armed after the cursor has already passed the end.
	 * Dropping the poller there leaves that chunk out of the cache. */
	if (dev->preheat_inflight != 0 ||
	    ((dev->preheat_stop ||
	      (dev->preheat_cursor >= dev->m->num_chunks &&
	       !dev->preheat_has_retry)) &&
	     dev->preheat_inflight == 0)) {
		spdk_poller_unregister(&dev->preheat_poller);
	}
	/* May complete a concurrent destroy; do not touch dev afterwards. */
	if (callback_ref) {
		export_async_put(dev);
	}
	return SPDK_POLLER_BUSY;
}

static bool
export_preheat_arm_poller(struct s3_export_dev *dev)
{
	/* An in-flight completion is already a wakeup. Deferring poller creation
	 * also guarantees that a rejected completion bounce never leaves an
	 * independently registered poller pointing at its device. */
	if (dev->preheat_inflight != 0) {
		return true;
	}
	if (dev->preheat_poller || dev->destroying) {
		return dev->preheat_poller != NULL;
	}
	if (dev->home_thread != NULL && dev->home_thread != spdk_get_thread()) {
		return false;
	}
	dev->preheat_poller = spdk_poller_register(export_preheat_poll, dev,
						   10 * 1000);
	if (!dev->preheat_poller) {
		/* There is no other guaranteed wakeup when the walk is token- or
		 * scan-budget-limited. Abort the optional optimisation instead of
		 * leaving a started walk permanently stranded. */
		SPDK_ERRLOG("Export %s template preheat could not register its "
			    "retry poller; stopping the walk\n", dev->m->uuid_str);
		dev->preheat_stop = true;
		dev->preheat_has_retry = false;
		return false;
	}
	return true;
}

/* Returns 1 when a GET was submitted, 0 when this chunk was skipped, and -1
 * when the walk must wait or stop. */
static int
export_preheat_issue(struct s3_export_dev *dev, uint64_t chunk)
{
	struct s3_export_fill *fill, *existing;
	struct s3_cache_object_id id;
	struct s3_cache_object_reservation *reservation = NULL;
	char key[S3_EXPORT_KEY_MAX];
	uint32_t object_len = 0;
	const struct s3_export_ref *ref;
	int rc;

	if (!s3_export_manifest_is_present(dev->m, chunk)) {
		return 0;
	}
	export_chunk_key(dev->m, chunk, key, sizeof(key));
	if (key[0] == '\0') {
		return 0;
	}
	if (dev->m->layout == S3_EXPORT_LAYOUT_REF) {
		ref = s3_export_manifest_get_ref(dev->m, chunk);
		if (!ref || ref->valid_bytes == 0) {
			return 0;
		}
		object_len = ref->valid_bytes;
	} else {
		object_len = dev->m->chunk_size;
	}
	id.endpoint = dev->cache_endpoint;
	id.bucket = dev->cache_bucket;
	id.key = key;
	rc = s3_cache_object_reserve(dev->shared_cache, &id, object_len,
				     &reservation);
	if (rc == 1) {
		return 0;
	}
	if (rc == -EAGAIN || rc == -EALREADY) {
		export_preheat_retry_from(dev, chunk);
		(void)export_preheat_arm_poller(dev);
		return -1;
	}
	if (rc != 0) {
		dev->preheat_stop = true;
		return -1;
	}

	rc = s3_whole_get_token_acquire_ex(true, export_preheat_token_unused,
					   NULL, dev);
	if (rc == -EAGAIN) {
		s3_cache_object_reservation_cancel(reservation);
		export_preheat_retry_from(dev, chunk);
		(void)export_preheat_arm_poller(dev);
		return -1;
	}
	if (rc != 1) {
		s3_cache_object_reservation_cancel(reservation);
		return -1;
	}

	fill = calloc(1, sizeof(*fill));
	if (!fill) {
		s3_whole_get_token_release();
		s3_cache_object_reservation_cancel(reservation);
		dev->preheat_stop = true;
		return -1;
	}
	fill->dev = dev;
	fill->chunk_index = chunk;
	fill->object_len = object_len;
	fill->preheat = true;
	fill->cache_reservation = reservation;
	fill->token_held = true;
	fill->buf = malloc(object_len);
	fill->buf_owned = fill->buf != NULL;
	if (!fill->buf) {
		s3_whole_get_token_release();
		s3_cache_object_reservation_cancel(reservation);
		free(fill);
		dev->preheat_stop = true;
		return -1;
	}
	snprintf(fill->key, sizeof(fill->key), "%s", key);
	TAILQ_INIT(&fill->waiters);

	pthread_mutex_lock(&dev->fill_lock);
	existing = export_fill_find_locked(dev, key, object_len);
	/* A demand read can leave a READY entry whose opportunistic cache
	 * populate was dropped because the disk-fill window was full. Reaching
	 * here with a reservation proves the object is still absent from the
	 * shared cache, and existing->buf already holds the whole object in RAM.
	 * Pin that entry across the reserved cache write instead of discarding it
	 * and fetching the same object from S3 a second time. */
	if (existing && existing->state == EXPORT_FILL_READY &&
	    existing->status == 0 && existing->buf != NULL &&
	    !existing->populate_wait && !dev->destroying) {
		int populate_rc;

		existing->chunk_index = chunk;
		existing->preheat = true;
		existing->preheat_detach = false;
		existing->populate_wait = true;
		existing->preheat_read_done_published = true;
		existing->preheat_finish_deferred = false;
		existing->lifetime_ref = true;
		dev->async_refs++;
		dev->preheat_inflight++;
		pthread_mutex_unlock(&dev->fill_lock);

		s3_whole_get_token_release();
		fill->cache_reservation = NULL;
		export_fill_free(fill);

		populate_rc = s3_cache_object_populate_reserved_notify(
			reservation, existing->buf, object_len,
			export_preheat_cached, existing);
		if (populate_rc != 0) {
			bool held = false;
			int issue_rc = 1;

			pthread_mutex_lock(&dev->fill_lock);
			existing->populate_wait = false;
			if (existing->lifetime_ref) {
				existing->lifetime_ref = false;
				held = true;
			}
			export_fill_evict_locked(dev);
			pthread_mutex_unlock(&dev->fill_lock);
			if (dev->preheat_inflight > 0) {
				dev->preheat_inflight--;
			}
			if (populate_rc == -ENOSPC || populate_rc == -ESHUTDOWN) {
				dev->preheat_stop = true;
				issue_rc = -1;
			} else if (populate_rc == -EAGAIN) {
				export_preheat_retry_from(dev, chunk);
				(void)export_preheat_arm_poller(dev);
				issue_rc = -1;
			}
			/* unregister may synchronously free dev; do not inspect any
			 * device member after this. */
			if (held) {
				export_async_put(dev);
			}
			return issue_rc;
		}
		return 1;
	}
	/* An in-flight demand GET keeps its own buffer, which may be the caller's
	 * and is released when that read completes. Attaching this reservation to
	 * it would let a queued disk fill read that buffer after the caller has
	 * freed it, and the completion can land off the preheat thread. Leave the
	 * demand GET alone and retry. Once it finishes, either the object is
	 * durable or the unpinned RAM entry above is discarded and this walk
	 * downloads it itself. */
	if (existing || dev->destroying ||
	    dev->fills_live >= EXPORT_FILL_MAX_LIVE ||
	    dev->fills_getting >= EXPORT_FILL_MAX_GETTING) {
		pthread_mutex_unlock(&dev->fill_lock);
		s3_whole_get_token_release();
		s3_cache_object_reservation_cancel(reservation);
		fill->cache_reservation = NULL;
		export_fill_free(fill);
		export_preheat_retry_from(dev, chunk);
		(void)export_preheat_arm_poller(dev);
		return -1;
	}
	fill->listed = true;
	fill->state = EXPORT_FILL_GETTING;
	fill->lifetime_ref = true;
	dev->async_refs++;
	TAILQ_INSERT_TAIL(&dev->fills, fill, link);
	dev->fills_live++;
	dev->fills_getting++;
	dev->preheat_inflight++;
	pthread_mutex_unlock(&dev->fill_lock);

	rc = s3_get_range(dev->client, fill->key, 0, fill->object_len, fill->buf,
			  export_fill_read_done, fill);
	if (rc != 0) {
		export_fill_read_done(fill, 0, rc);
	}
	return 1;
}

/* One line when the GET walk has issued every chunk. Disk fills may still be
 * queued behind it; callers that need the bytes wait on the cache counters. */
static void
export_preheat_note_idle(struct s3_export_dev *dev)
{
	if (!dev || dev->preheat_idle_logged || dev->destroying ||
	    dev->preheat_stop || dev->preheat_has_retry ||
	    dev->preheat_inflight != 0 ||
	    dev->preheat_cursor < dev->m->num_chunks) {
		return;
	}
	dev->preheat_idle_logged = true;
	SPDK_NOTICELOG("Export %s template preheat walk finished\n",
		       dev->m->uuid_str);
}

static void
export_preheat_pump(struct s3_export_dev *dev)
{
	uint32_t scanned = 0;
	bool release_preheat_ref = false;

	if (!dev || dev->preheat_pumping || dev->destroying || dev->preheat_stop) {
		if (dev && dev->preheat_inflight == 0 &&
		    (dev->destroying || dev->preheat_stop)) {
			if (dev->home_thread == NULL ||
			    dev->home_thread == spdk_get_thread()) {
				spdk_poller_unregister(&dev->preheat_poller);
				release_preheat_ref =
					export_preheat_take_lifetime(dev);
			}
		}
		if (release_preheat_ref) {
			export_async_put(dev);
		}
		return;
	}
	dev->preheat_pumping = true;
	/* Finish the current pass before revisiting blocked chunks. Otherwise one
	 * busy early reservation can repeatedly rewind the cursor and starve every
	 * later object. */
	if (dev->preheat_cursor >= dev->m->num_chunks &&
	    dev->preheat_has_retry) {
		dev->preheat_cursor = dev->preheat_retry;
		dev->preheat_has_retry = false;
	}
	while (dev->preheat_inflight < S3_EXPORT_PREHEAT_MAX &&
	       !dev->preheat_stop && !dev->destroying &&
	       scanned < EXPORT_PREHEAT_SCAN_BUDGET) {
		uint64_t chunk;
		int rc;

		if (dev->preheat_cursor >= dev->m->num_chunks) {
			break;
		}
		chunk = dev->preheat_cursor;
		scanned++;
		rc = export_preheat_issue(dev, chunk);
		if (chunk == dev->preheat_cursor) {
			dev->preheat_cursor++;
		}
		if (rc < 0 && dev->preheat_stop) {
			break;
		}
	}
	dev->preheat_pumping = false;
	if (!dev->preheat_stop && !dev->destroying &&
	    ((scanned == EXPORT_PREHEAT_SCAN_BUDGET &&
	      dev->preheat_cursor < dev->m->num_chunks) ||
	     (dev->preheat_inflight == 0 && dev->preheat_has_retry &&
	      dev->preheat_cursor >= dev->m->num_chunks))) {
		(void)export_preheat_arm_poller(dev);
	}
	if (dev->preheat_inflight == 0 &&
	    (dev->preheat_stop || dev->preheat_cursor >= dev->m->num_chunks) &&
	    !dev->preheat_has_retry &&
	    (dev->home_thread == NULL || dev->home_thread == spdk_get_thread())) {
		export_preheat_note_idle(dev);
		spdk_poller_unregister(&dev->preheat_poller);
		release_preheat_ref = export_preheat_take_lifetime(dev);
	}
	if (release_preheat_ref) {
		export_async_put(dev);
	}
}

int
s3_export_bs_dev_preheat(struct spdk_bs_dev *bs_dev)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;
	int rc;

	if (!dev || !dev->shared_cache || dev->destroying) {
		return -EINVAL;
	}
	if (dev->home_thread != NULL && dev->home_thread != spdk_get_thread()) {
		if (!export_async_get_live(dev)) {
			return -EINVAL;
		}
		rc = spdk_thread_send_msg(dev->home_thread, export_preheat_msg, dev);
		if (rc != 0) {
			export_async_put(dev);
		}
		return rc == 0 ? 0 : rc;
	}
	if (dev->preheat_started) {
		return 0;
	}
	if (!export_preheat_get_live(dev)) {
		return -EINVAL;
	}
	dev->preheat_started = true;
	SPDK_NOTICELOG("Export %s template preheat started\n", dev->m->uuid_str);
	export_preheat_pump(dev);
	return 0;
}

static void
export_fill_attach_locked(struct s3_export_dev *dev,
			  struct s3_export_fill *fill,
			  struct s3_export_fill_waiter *waiter)
{
	fill->users++;
	if (fill->state != EXPORT_FILL_READY) {
		TAILQ_INSERT_TAIL(&fill->waiters, waiter, link);
		dev->coalesced_reads++;
		return;
	}

	assert(fill->on_lru);
	TAILQ_REMOVE(&dev->fill_lru, fill, lru_link);
	TAILQ_INSERT_TAIL(&dev->fill_lru, fill, lru_link);
	fill->preheat_ready_hit = true;
	dev->ready_hits++;
}

static void
export_fill_token_granted(void *arg)
{
	struct s3_export_fill *fill = arg;
	struct s3_export_dev *dev = fill->dev;
	struct s3_export_fill_waiter *waiter;
	int rc;

	pthread_mutex_lock(&dev->fill_lock);
	assert(fill->state == EXPORT_FILL_GETTING);
	assert(!fill->token_held);
	fill->token_held = true;
	TAILQ_FOREACH(waiter, &fill->waiters, link) {
		if (waiter->offset == 0 && waiter->length >= fill->object_len) {
			fill->direct_waiter = waiter;
			fill->buf = waiter->dst;
			break;
		}
	}
	pthread_mutex_unlock(&dev->fill_lock);

	if (!fill->buf) {
		fill->buf = malloc(fill->object_len);
		fill->buf_owned = fill->buf != NULL;
	}
	if (!fill->buf) {
		export_fill_read_done(fill, 0, -ENOMEM);
		return;
	}
	rc = s3_get_range(dev->client, fill->key, 0, fill->object_len, fill->buf,
			  export_fill_read_done, fill);
	if (rc != 0) {
		export_fill_read_done(fill, 0, rc);
	}
}

static void
export_fill_token_cancelled(void *arg, int status)
{
	/* Delivery of the grant bounce failed. The fill is still GETTING with
	 * no token held; complete waiters the same way a GET submit failure
	 * does, rather than leaving them in GETTING forever. */
	export_fill_read_done(arg, 0, status);
}

static void
export_fill_request_token(struct s3_export_fill *fill)
{
	int rc;

	rc = s3_whole_get_token_acquire_ex(false, export_fill_token_granted,
					   export_fill_token_cancelled, fill);
	if (rc == 1) {
		export_fill_token_granted(fill);
	} else if (rc < 0) {
		export_fill_read_done(fill, 0, rc);
	}
}

/* Submit one logical slice through the bounded whole-object working set.
 * Every successful return has consumed waiter. */
static int
export_fill_submit_existing(struct s3_export_io *io, const char *key,
			    uint32_t object_len, uint32_t offset,
			    uint32_t length, void *dst)
{
	struct s3_export_dev *dev = io->dev;
	struct s3_export_fill_waiter *waiter;
	struct s3_export_fill *fill;
	bool ready;

	waiter = calloc(1, sizeof(*waiter));
	if (!waiter) {
		return -ENOMEM;
	}
	waiter->io = io;
	waiter->dst = dst;
	waiter->offset = offset;
	waiter->length = length;
	waiter->origin = io->origin;

	pthread_mutex_lock(&dev->fill_lock);
	fill = export_fill_find_locked(dev, key, object_len);
	if (!fill) {
		pthread_mutex_unlock(&dev->fill_lock);
		free(waiter);
		return -ENOENT;
	}
	waiter->fill = fill;
	ready = fill->state == EXPORT_FILL_READY;
	export_fill_attach_locked(dev, fill, waiter);
	pthread_mutex_unlock(&dev->fill_lock);
	if (ready) {
		export_fill_waiter_deliver(waiter);
	}
	return 0;
}

static int
export_fill_submit(struct s3_export_io *io, const char *key,
		   uint64_t chunk_index, uint32_t object_len,
		   uint32_t offset, uint32_t length, void *dst)
{
	struct s3_export_dev *dev = io->dev;
	struct s3_export_fill_waiter *waiter;
	struct s3_export_fill *fill, *candidate = NULL;
	bool ready, start;

	waiter = calloc(1, sizeof(*waiter));
	if (!waiter) {
		return -ENOMEM;
	}
	waiter->io = io;
	waiter->dst = dst;
	waiter->offset = offset;
	waiter->length = length;
	waiter->origin = io->origin;
	assert(offset <= object_len && length <= object_len - offset);

retry:
	pthread_mutex_lock(&dev->fill_lock);
	fill = export_fill_find_locked(dev, key, object_len);
	if (fill) {
		waiter->fill = fill;
		ready = fill->state == EXPORT_FILL_READY;
		export_fill_attach_locked(dev, fill, waiter);
		pthread_mutex_unlock(&dev->fill_lock);
		if (candidate) {
			free(candidate);
		}
		if (ready) {
			export_fill_waiter_deliver(waiter);
		}
		return 0;
	}
	while (dev->fills_live >= EXPORT_FILL_MAX_LIVE &&
	       export_fill_evict_one_locked(dev)) {
	}
	if (dev->fills_live >= EXPORT_FILL_MAX_LIVE) {
		pthread_mutex_unlock(&dev->fill_lock);
		free(candidate);
		free(waiter);
		return -EAGAIN;
	}
	if (!candidate) {
		pthread_mutex_unlock(&dev->fill_lock);
		candidate = calloc(1, sizeof(*candidate));
		if (!candidate) {
			free(waiter);
			return -ENOMEM;
		}
		goto retry;
	}

	fill = candidate;
	fill->dev = dev;
	fill->chunk_index = chunk_index;
	fill->object_len = object_len;
	fill->listed = true;
	TAILQ_INIT(&fill->waiters);
	snprintf(fill->key, sizeof(fill->key), "%s", key);
	waiter->fill = fill;
	fill->users = 1;
	TAILQ_INSERT_TAIL(&fill->waiters, waiter, link);
	TAILQ_INSERT_TAIL(&dev->fills, fill, link);
	fill->lifetime_ref = true;
	dev->async_refs++;
	dev->fills_live++;
	start = dev->fills_getting < EXPORT_FILL_MAX_GETTING;
	if (start) {
		fill->state = EXPORT_FILL_GETTING;
		dev->fills_getting++;
	} else {
		fill->state = EXPORT_FILL_QUEUED;
		fill->on_pending = true;
		TAILQ_INSERT_TAIL(&dev->fill_pending, fill, pending_link);
	}
	pthread_mutex_unlock(&dev->fill_lock);

	/* Reserve a local slot before asking for a process token.  The 1 MiB
	 * staging buffer is allocated only after both limits admit this fill. */
	if (start) {
		export_fill_request_token(fill);
	}
	return 0;
}

static void
export_chunk_read_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct s3_export_chunk_io *cio = cb_arg;
	struct s3_export_io *io = cio->io;

	if (status == -ENOENT && !io->retried) {
		/* The object this manifest names is gone. On a ref export that is what
		 * the source materialising it looks like from here: it rewrote the
		 * manifest with its own copies and deleted the originals, and nothing
		 * tells an importer -- the 404 is the notification.
		 *
		 * So this is not recorded as the I/O's status. The I/O is held instead,
		 * the manifest refetched, and the read run again against whatever comes
		 * back. -EALREADY from that swap still retries: another refetch may
		 * already have installed this generation. If the objects are genuinely
		 * gone the retry 404s, which is the other thing a 404 can mean.
		 *
		 * Only once. A 404 after the retry is against the manifest that was
		 * just fetched, so refetching again would find the same thing. */
		io->saw_missing = true;
	} else if (status != 0) {
		SPDK_ERRLOG("Failed to read export chunk '%s': %s\n",
			    cio->key, spdk_strerror(-status));
		if (io->status == 0) {
			io->status = status;
		}
	} else if (bytes_read != cio->expected) {
		/* A short read that reported success. The buffer past bytes_read still
		 * holds whatever it held before -- zeroes, for a freshly allocated
		 * materialisation buffer -- so letting this pass as success is how a
		 * chunk ends up silently readable as zeroes, and once a decouple has
		 * written that into a local cluster there is no way left to tell it
		 * from a hole the export really had.
		 *
		 * A range request is either satisfied or it is an error; a partial
		 * answer is neither, so it is turned into one here rather than
		 * retried. The caller retries whole I/Os, and blobstore fails the
		 * materialisation, which leaves the volume still reading through the
		 * export -- wrong but recoverable, instead of wrong and permanent.
		 */
		SPDK_ERRLOG("Short read of export chunk '%s': asked for %u byte(s), "
			    "got %" PRIu64 ". Refusing to treat the remainder as "
			    "zeroes.\n", cio->key, cio->expected, bytes_read);
		if (io->status == 0) {
			io->status = -EIO;
		}
	} else {
		__atomic_fetch_add(&io->dev->bytes_read, bytes_read, __ATOMIC_RELAXED);
		if (io->dev->shared_cache) {
			struct s3_cache_object_id id = {
				.endpoint = io->dev->cache_endpoint,
				.bucket = io->dev->cache_bucket,
				.key = cio->key,
			};

			s3_cache_object_populate(io->dev->shared_cache, &id,
						 cio->offset, cio->dst,
						 cio->expected, cio->object_len);
		}
	}

	free(cio);
	export_io_put(io);
}

static void
export_cache_submit_s3(struct s3_export_cache_io *cache_io)
{
	struct s3_export_io *io = cache_io->io;
	struct s3_export_dev *dev = io->dev;
	struct s3_export_chunk_io *cio;
	int rc;

	rc = export_fill_submit(io, cache_io->key, cache_io->chunk_index,
				cache_io->object_len, cache_io->offset,
				cache_io->get_len, cache_io->dst);
	if (rc == 0) {
		free(cache_io);
		return;
	}
	if (rc != -EAGAIN) {
		if (io->status == 0) {
			io->status = rc;
		}
		free(cache_io);
		export_io_put(io);
		return;
	}

	/* Preserve the existing pressure fallback: sharing is an optimisation,
	 * so neither a cache miss nor a busy L1 may turn a readable object into
	 * an error. */
	cio = calloc(1, sizeof(*cio));
	if (!cio) {
		if (io->status == 0) {
			io->status = -ENOMEM;
		}
		free(cache_io);
		export_io_put(io);
		return;
	}
	cio->io = io;
	cio->chunk_index = cache_io->chunk_index;
	cio->expected = cache_io->get_len;
	cio->object_len = cache_io->object_len;
	cio->offset = cache_io->offset;
	cio->dst = cache_io->dst;
	snprintf(cio->key, sizeof(cio->key), "%s", cache_io->key);
	rc = s3_get_range(dev->client, cio->key, cio->offset, cio->expected,
			  cio->dst, export_chunk_read_done, cio);
	if (rc != 0) {
		if (io->status == 0) {
			io->status = rc;
		}
		free(cio);
		export_io_put(io);
	} else {
		__atomic_fetch_add(&dev->exact_fallbacks, 1, __ATOMIC_RELAXED);
	}
	free(cache_io);
}

static void
export_cache_read_done(void *cb_arg, int status)
{
	struct s3_export_cache_io *cache_io = cb_arg;
	struct s3_export_dev *dev = cache_io->io->dev;

	if (status == 0) {
		struct s3_export_io *io = cache_io->io;

		__atomic_fetch_add(&dev->shared_cache_hits, 1, __ATOMIC_RELAXED);
		free(cache_io);
		export_io_put(io);
		return;
	}
	__atomic_fetch_add(&dev->shared_cache_misses, 1, __ATOMIC_RELAXED);
	__atomic_fetch_add(&dev->shared_cache_fallbacks, 1, __ATOMIC_RELAXED);
	export_cache_submit_s3(cache_io);
}

/* ==========================================================================
 * Read path
 * ========================================================================== */

/* The read itself. \p is_retry marks an I/O that has already been through one
 * refetch, so a 404 against the manifest just fetched is reported rather than
 * sent round again. */
static void
export_read_internal(struct spdk_bs_dev *bs_dev, struct spdk_io_channel *channel,
		     void *payload, uint64_t lba, uint32_t lba_count,
		     struct spdk_bs_dev_cb_args *cb_args, bool is_retry)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;
	struct s3_export_channel *export_ch = channel
					  ? spdk_io_channel_get_ctx(channel) : NULL;
	struct s3_export_io *io;
	uint64_t offset_bytes = lba * S3LVOL_BLOCK_SIZE;
	uint64_t remaining = (uint64_t)lba_count * S3LVOL_BLOCK_SIZE;
	uint8_t *buf = payload;

	if (lba + lba_count > bs_dev->blockcnt) {
		SPDK_ERRLOG("export read out of range: lba=%" PRIu64 " count=%u "
			    "blockcnt=%" PRIu64 "\n", lba, lba_count, bs_dev->blockcnt);
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -EINVAL);
		return;
	}
	if (lba_count == 0) {
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, 0);
		return;
	}

	/* A retry transfers the reference held by the original I/O. New reads are
	 * admitted under the same lock destroy() uses to enter draining state. */
	if (!is_retry && !export_async_get_live(dev)) {
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -EIO);
		return;
	}

	io = calloc(1, sizeof(*io));
	if (!io) {
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOMEM);
		export_async_put(dev);
		return;
	}
	io->dev = dev;
	io->lifetime_ref = true;
	io->m = __atomic_load_n(&dev->m, __ATOMIC_ACQUIRE);
	s3_export_manifest_ref(io->m);
	io->cb_args = cb_args;
	/* Kept so the read can be run again after a refetch. */
	io->payload = payload;
	io->lba = lba;
	io->lba_count = lba_count;
	io->origin = spdk_get_thread();
	io->retried = is_retry;
	/* Self reference, so a sub-read that completes inline -- which a hole
	 * always does -- cannot finish the I/O before the split loop is done. */
	io->num_pending = 1;

	__atomic_fetch_add(&dev->reads, 1, __ATOMIC_RELAXED);

	while (remaining > 0 && io->status == 0) {
		uint32_t chunk_size = io->m->chunk_size;
		uint32_t chunk_shift = (uint32_t)spdk_u32log2(chunk_size);
		uint64_t chunk_index = offset_bytes >> chunk_shift;
		uint32_t offset_in_chunk = (uint32_t)(offset_bytes &
						(chunk_size - 1));
		uint32_t length = (uint32_t)spdk_min(remaining,
						     chunk_size - offset_in_chunk);
		uint32_t get_len, object_len;
		char key[S3_EXPORT_KEY_MAX];
		int rc;

		/* A chunk with no object reads as zeroes. Not an optimisation: it is how
		 * an export represents a hole, and it is the common case across the
		 * sparse regions of a snapshot. */
		if (!s3_export_manifest_is_present(io->m, chunk_index)) {
			memset(buf, 0, length);
			__atomic_fetch_add(&dev->zero_fills, 1, __ATOMIC_RELAXED);
			goto next;
		}

		get_len = length;
		object_len = chunk_size;

		if (io->m->layout == S3_EXPORT_LAYOUT_REF) {
			const struct s3_export_ref *ref;

			ref = s3_export_manifest_get_ref(io->m, chunk_index);
			if (!ref) {
				/* Present in the bitmap with no ref behind it. parse()
				 * rejects that, so getting here means the manifest was
				 * mutated in memory afterwards. */
				SPDK_ERRLOG("export %s: chunk %" PRIu64 " is present but "
					    "has no ref\n", io->m->uuid_str, chunk_index);
				io->status = -EIO;
				break;
			}

			/* Same class of failure, for the prefix rather than the uuid: an
			 * index outside the source table, which parse() also rejects.
			 * Checked here rather than left to export_chunk_key() because this
			 * is where a failure can be reported -- and because the alternative
			 * is worse than an error. Falling back to src.prefix would build a
			 * key that very likely exists and holds another chunk's data, so the
			 * read would succeed and return the wrong bytes. */
			if (s3_export_manifest_chunk_prefix(io->m, chunk_index)[0] == '\0') {
				SPDK_ERRLOG("export %s: chunk %" PRIu64 " names a source this "
					    "manifest does not have\n", io->m->uuid_str,
					    chunk_index);
				io->status = -EIO;
				break;
			}

			/* A ref export names the source's live object, and that object is
			 * only as long as the writes which produced it. So the tail of a
			 * partially written chunk is zeroes here, not a range request
			 * against bytes that were never uploaded -- which would come back
			 * as an error, or as whatever the object store decides to do with
			 * an unsatisfiable range. A dense export needs none of this
			 * because it uploads whole chunks. */
			if (offset_in_chunk >= ref->valid_bytes) {
				memset(buf, 0, length);
				__atomic_fetch_add(&dev->zero_fills, 1, __ATOMIC_RELAXED);
				goto next;
			}
			if (offset_in_chunk + length > ref->valid_bytes) {
				get_len = ref->valid_bytes - offset_in_chunk;
				memset(buf + get_len, 0, length - get_len);
			}
			object_len = ref->valid_bytes;
		}

		export_chunk_key(io->m, chunk_index, key, sizeof(key));
		io->num_pending++;
		rc = export_fill_submit_existing(io, key, object_len,
						 offset_in_chunk, get_len, buf);
		if (rc == 0) {
			goto next;
		}
		if (rc != -ENOENT) {
			io->num_pending--;
			io->status = rc;
			break;
		}

		if (dev->shared_cache && export_ch) {
			struct s3_export_cache_io *cache_io;
			struct s3_cache_object_id id = {
				.endpoint = dev->cache_endpoint,
				.bucket = dev->cache_bucket,
				.key = key,
			};

			cache_io = calloc(1, sizeof(*cache_io));
			if (cache_io) {
				cache_io->io = io;
				cache_io->chunk_index = chunk_index;
				cache_io->object_len = object_len;
				cache_io->offset = offset_in_chunk;
				cache_io->get_len = get_len;
				cache_io->dst = buf;
				snprintf(cache_io->key, sizeof(cache_io->key), "%s",
					 key);
				rc = s3_cache_object_read_on_channel(
					dev->shared_cache, export_ch->cache_ch, &id,
					object_len, offset_in_chunk, length, buf,
					export_cache_read_done, cache_io);
				if (rc == 0) {
					goto next;
				}
				__atomic_fetch_add(&dev->shared_cache_misses, 1,
						   __ATOMIC_RELAXED);
				__atomic_fetch_add(&dev->shared_cache_fallbacks, 1,
						   __ATOMIC_RELAXED);
				export_cache_submit_s3(cache_io);
				goto next;
			}
		}

		/* Sharing is optional, including under allocation pressure. */
		{
			struct s3_export_cache_io *s3_io = calloc(1, sizeof(*s3_io));

			if (!s3_io) {
				io->num_pending--;
				io->status = -ENOMEM;
				break;
			}
			s3_io->io = io;
			s3_io->chunk_index = chunk_index;
			s3_io->object_len = object_len;
			s3_io->offset = offset_in_chunk;
			s3_io->get_len = get_len;
			s3_io->dst = buf;
			snprintf(s3_io->key, sizeof(s3_io->key), "%s", key);
			export_cache_submit_s3(s3_io);
		}
next:
		buf += length;
		offset_bytes += length;
		remaining -= length;
	}

	io->submit_done = true;
	export_io_put(io);
}

/* blobstore's entry point: always a first attempt. */
static void
export_read(struct spdk_bs_dev *bs_dev, struct spdk_io_channel *channel,
	    void *payload, uint64_t lba, uint32_t lba_count,
	    struct spdk_bs_dev_cb_args *cb_args)
{
	export_read_internal(bs_dev, channel, payload, lba, lba_count, cb_args,
			     false);
}

static void
export_readv(struct spdk_bs_dev *bs_dev, struct spdk_io_channel *channel,
	     struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
	     struct spdk_bs_dev_cb_args *cb_args)
{
	if (iovcnt == 1) {
		export_read(bs_dev, channel, iov[0].iov_base, lba, lba_count, cb_args);
		return;
	}

	/* Same limitation, and the same reasoning, as s3_bs_dev: reporting it is
	 * safe, quietly reading the wrong bytes into the wrong buffer is not. The
	 * bdev layer splits multi-segment I/O for us (max_num_segments = 1), and
	 * blobstore's own copy-on-write path uses a single buffer. */
	SPDK_ERRLOG("export readv with iovcnt=%d is not supported\n", iovcnt);
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOTSUP);
}

static void
export_readv_ext(struct spdk_bs_dev *bs_dev, struct spdk_io_channel *channel,
		 struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
		 struct spdk_bs_dev_cb_args *cb_args,
		 struct spdk_blob_ext_io_opts *ext_io_opts)
{
	/* ext_io_opts carries a memory domain, i.e. a payload this thread cannot
	 * memcpy from. Nothing in this path can honour that, and there is no
	 * memory domain in play in this stack today. */
	if (ext_io_opts && ext_io_opts->memory_domain) {
		SPDK_ERRLOG("export read with a memory domain is not supported\n");
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOTSUP);
		return;
	}
	export_readv(bs_dev, channel, iov, iovcnt, lba, lba_count, cb_args);
}

/* ==========================================================================
 * Write path: there isn't one
 * ========================================================================== */

static void
export_reject_write(struct spdk_bs_dev_cb_args *cb_args, const char *what)
{
	/* blobstore does not write to a back device, so reaching this is a bug
	 * somewhere above. Loud, and -EROFS rather than -ENOTSUP, because the
	 * device is not merely lacking the feature: the data it serves is a
	 * snapshot that another node may still be reading. */
	SPDK_ERRLOG("%s on an imported export is not allowed; it is read-only\n", what);
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -EROFS);
}

static void
export_write(struct spdk_bs_dev *dev, struct spdk_io_channel *channel, void *payload,
	     uint64_t lba, uint32_t lba_count, struct spdk_bs_dev_cb_args *cb_args)
{
	export_reject_write(cb_args, "write");
}

static void
export_writev(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
	      struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
	      struct spdk_bs_dev_cb_args *cb_args)
{
	export_reject_write(cb_args, "writev");
}

static void
export_writev_ext(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		  struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
		  struct spdk_bs_dev_cb_args *cb_args,
		  struct spdk_blob_ext_io_opts *ext_io_opts)
{
	export_reject_write(cb_args, "writev");
}

static void
export_write_zeroes(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		    uint64_t lba, uint64_t lba_count,
		    struct spdk_bs_dev_cb_args *cb_args)
{
	export_reject_write(cb_args, "write_zeroes");
}

static void
export_unmap(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
	     uint64_t lba, uint64_t lba_count, struct spdk_bs_dev_cb_args *cb_args)
{
	export_reject_write(cb_args, "unmap");
}

static void
export_flush(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
	     struct spdk_bs_dev_cb_args *cb_args)
{
	/* Nothing was ever dirtied here. Succeeding is the honest answer. */
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, 0);
}

/* ==========================================================================
 * Queries
 * ========================================================================== */

static bool
export_is_zeroes(struct spdk_bs_dev *bs_dev, uint64_t lba, uint64_t lba_count)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;
	struct s3_export_manifest *m;
	uint32_t chunk_shift;
	uint64_t first, last;

	if (lba_count == 0) {
		return true;
	}

	m = __atomic_load_n(&dev->m, __ATOMIC_ACQUIRE);
	chunk_shift = (uint32_t)spdk_u32log2(m->chunk_size);

	/* Whole chunks only. A range that starts or ends inside a present chunk is
	 * reported as non-zero even if those particular bytes are zero: the answer
	 * has to be conservative, since blobstore uses it to skip reading the
	 * parent entirely. */
	first = (lba * S3LVOL_BLOCK_SIZE) >> chunk_shift;
	last = ((lba + lba_count - 1) * S3LVOL_BLOCK_SIZE) >> chunk_shift;

	return s3_export_manifest_range_is_zeroes(m, first, last - first + 1);
}

static bool
export_is_range_valid(struct spdk_bs_dev *bs_dev, uint64_t lba, uint64_t lba_count)
{
	return lba + lba_count <= bs_dev->blockcnt;
}

static bool
export_translate_lba(struct spdk_bs_dev *bs_dev, uint64_t lba, uint64_t *base_lba)
{
	/* Identity: dest->copy during ingest treats src_lba as an LBA on this
	 * export device, not as a block on the destination disk. Returning false
	 * would force allocate_and_copy_cluster down the GET+write path even when
	 * the destination has installed copy(). */
	(void)bs_dev;
	*base_lba = lba;
	return true;
}

static bool
export_is_degraded(struct spdk_bs_dev *bs_dev)
{
	/* Reachability of the source bucket is not tracked. Claiming degraded
	 * would make blobstore refuse I/O that would probably succeed; a real
	 * outage surfaces as failed reads, which is what an operator sees anyway. */
	return false;
}

/* ==========================================================================
 * Channels and teardown
 * ========================================================================== */

static int
export_channel_create_cb(void *io_device, void *ctx_buf)
{
	struct s3_export_dev *dev = io_device;
	struct s3_export_channel *ch = ctx_buf;

	ch->cache_ch = dev->shared_cache
		       ? s3_cache_get_io_channel(dev->shared_cache) : NULL;
	return 0;
}

static void
export_channel_destroy_cb(void *io_device, void *ctx_buf)
{
	struct s3_export_channel *ch = ctx_buf;

	if (ch->cache_ch) {
		spdk_put_io_channel(ch->cache_ch);
	}
}

static struct spdk_io_channel *
export_create_channel(struct spdk_bs_dev *bs_dev)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;

	/* A real spdk_io_channel is required, not a stub: blobstore caches one of
	 * these per (thread, blob) and releases some of them with
	 * spdk_put_io_channel() rather than through destroy_channel()
	 * (blobstore.c, blob_esnap_destroy_bs_channel). Anything not obtained from
	 * spdk_get_io_channel() would blow up there. */
	return spdk_get_io_channel(dev);
}

static void
export_destroy_channel(struct spdk_bs_dev *bs_dev, struct spdk_io_channel *channel)
{
	spdk_put_io_channel(channel);
}

static void
export_io_device_unregistered(void *io_device)
{
	struct s3_export_dev *dev = io_device;
	struct s3_export_fill *fill;

	pthread_mutex_lock(&dev->fill_lock);
	assert(dev->async_refs == 0);
	assert(dev->refetch_delivery_poller == NULL);
	assert(!dev->refetch_delivery_lifetime_ref);
	assert(TAILQ_EMPTY(&dev->refetch_waiters));
	assert(TAILQ_EMPTY(&dev->refetch_delivery_waiters));
	assert(dev->fills_getting == 0);
	assert(TAILQ_EMPTY(&dev->fill_pending));
	while ((fill = TAILQ_FIRST(&dev->fills)) != NULL) {
		assert(fill->state == EXPORT_FILL_READY);
		assert(fill->users == 0);
		TAILQ_REMOVE(&dev->fills, fill, link);
		if (fill->on_lru) {
			TAILQ_REMOVE(&dev->fill_lru, fill, lru_link);
		}
		assert(dev->fills_live > 0);
		dev->fills_live--;
		export_fill_free(fill);
	}
	dev->fills_ready = 0;
	assert(dev->fills_live == 0);
	pthread_mutex_unlock(&dev->fill_lock);
	pthread_mutex_destroy(&dev->fill_lock);

	s3_export_manifest_unref(dev->m);
	if (dev->client) {
		s3_client_put(dev->client);
	}
	free(dev);
}

static void
export_refetch_delivery_stop(void *arg)
{
	struct s3_export_dev *dev = arg;
	bool release_ref = false;

	spdk_poller_unregister(&dev->refetch_delivery_poller);
	pthread_mutex_lock(&dev->fill_lock);
	if (dev->refetch_delivery_lifetime_ref) {
		dev->refetch_delivery_lifetime_ref = false;
		release_ref = true;
	}
	pthread_mutex_unlock(&dev->fill_lock);
	if (release_ref) {
		/* The poller reference is the only reason dev remains valid here.
		 * This put is deliberately the final access. */
		export_async_put(dev);
	}
}

static void
export_refetch_delivery_stop_msg(void *arg)
{
	struct s3_export_dev *dev = arg;

	export_refetch_delivery_stop(dev);
	/* Paired with the message reference taken by export_destroy(). The
	 * delivery poller may have retired its own reference first. */
	export_async_put(dev);
}

static void
export_destroy(struct spdk_bs_dev *bs_dev)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;
	bool home_thread;

	SPDK_NOTICELOG("Releasing imported export %s: %" PRIu64 " read(s), "
		       "%" PRIu64 " bytes from S3, %" PRIu64 " served as zeroes, "
		       "%" PRIu64 " whole-object GET(s), %" PRIu64
		       " coalesced read(s), %" PRIu64 " RAM hit(s), %" PRIu64
	       " exact fallback(s), %" PRIu64
	       " shared-cache hit(s), %" PRIu64 " shared-cache miss(es), %" PRIu64
	       " shared-cache fallback(s), %" PRIu64
	       " manifest refetch(es)\n",
		       dev->m->uuid_str, dev->reads, dev->bytes_read, dev->zero_fills,
		       dev->whole_gets, dev->coalesced_reads, dev->ready_hits,
	       dev->exact_fallbacks, dev->shared_cache_hits,
	       dev->shared_cache_misses, dev->shared_cache_fallbacks,
	       dev->refetches);

	/* No wait for a refetch, deliberately.
	 *
	 * A refetch only exists while an I/O is waiting on it, and that I/O is one
	 * blobstore is waiting for a callback on -- so blobstore cannot have decided
	 * the device is unreferenced. The same reasoning already covers an ordinary
	 * chunk GET, which writes to dev from its completion.
	 *
	 * What that rests on is that every path out of a refetch releases its
	 * waiters, including each failure: a refetch that returned without doing so
	 * would leave blobstore waiting for ever, and the leak would look like a
	 * hung volume rather than like anything to do with this file. */

	home_thread = dev->home_thread == NULL ||
		      dev->home_thread == spdk_get_thread();
	if (home_thread) {
		spdk_poller_unregister(&dev->preheat_poller);
	}

	/* Hold an explicit lifetime reference for every queued or in-flight fill.
	 * Blobstore normally covers demand GETs, but decouple can hot-unplug the
	 * backing device from a blob as its final reads complete while another
	 * fill is being promoted from the pending queue.  Prefetch has no
	 * blobstore I/O lifetime at all. */
	pthread_mutex_lock(&dev->fill_lock);
	dev->preheat_stop = true;
	dev->destroying = true;
	if (!home_thread) {
		/* Keep dev alive even if the poller observes destroying and releases
		 * its own lifetime reference before this queued callback runs. */
		dev->async_refs++;
	}
	/* On the home thread the poller has already been removed. If no fill can
	 * deliver another completion, retire the walk reference here as part of
	 * the same transition to destroying. Off-home destruction leaves it for
	 * the poller (or the final fill) to release on the home thread. */
	if (home_thread && dev->preheat_inflight == 0 &&
	    dev->preheat_lifetime_ref) {
		dev->preheat_lifetime_ref = false;
		assert(dev->async_refs > 0);
		dev->async_refs--;
	}
	if (dev->async_refs == 0) {
		dev->unregister_started = true;
		pthread_mutex_unlock(&dev->fill_lock);
		spdk_io_device_unregister(dev, export_io_device_unregistered);
	} else {
		pthread_mutex_unlock(&dev->fill_lock);
	}

	if (home_thread) {
		export_refetch_delivery_stop(dev);
	} else {
		/* Do not wait for the periodic retry poller to notice destruction.
		 * Promptly retire its lifetime reference after messages already
		 * queued on the home thread have run. If this send itself fails, the
		 * poller remains pinned and provides the fallback cleanup path. */
		if (spdk_thread_send_msg(dev->home_thread,
					export_refetch_delivery_stop_msg, dev) != 0) {
			export_async_put(dev);
		}
	}
}

/* ==========================================================================
 * Replacing the manifest
 *
 * A ref export names the source's live objects. When the source materialises it
 * -- rewrites the manifest as dense, with its own copies -- those objects stop
 * existing, and an importer still holding the old manifest reads 404s. Its way
 * out is to fetch the manifest again and carry on against the new one, which is
 * what `generation` is for.
 *
 * The hard part is not fetching it. Manifest access is lock free because a
 * parsed manifest never changes: blobstore makes a channel per thread that
 * touches the clone, and every one of them reads dev->m without taking the fill
 * table's unrelated mutex. Swapping it is therefore the one operation that has
 * to think about the other threads.
 *
 * What makes it tractable is that no reader holds the manifest across an await.
 * Every dereference is synchronous inside export_read() or export_is_zeroes():
 * a chunk's key is copied into the sub-I/O before the GET is submitted, and the
 * completion never looks at the manifest again. So a reader's window is one
 * synchronous stretch on its own thread.
 *
 * Hence a grace period rather than a lock. The pointer is replaced first -- a
 * pointer store, so every reader sees one manifest or the other, never a torn
 * value, and both are valid to read against. The old one is then released only
 * after spdk_for_each_channel() has run on every thread that has a channel:
 * having got a turn on each of them means every reader that could have loaded
 * the old pointer has already finished with it, because it could only have been
 * doing so from the same event loop the iteration just ran in.
 * ========================================================================== */

struct export_swap_ctx {
	/* Deliberately no dev pointer. destroy() can run while this is in flight --
	 * spdk_io_device_unregister() defers behind for_each, but the completion
	 * still arrives -- and holding the device here would suggest it is safe to
	 * dereference on the way out. Nothing below needs it: the old manifest's
	 * reference was transferred into this struct, so releasing it does not
	 * involve the device at all. */
	struct s3_export_manifest *old;
	s3_export_bs_dev_swap_cb   cb;
	void                      *cb_arg;
};

static void
export_swap_channel(struct spdk_io_channel_iter *i)
{
	/* Nothing to do per channel: being called here is the point. It says this
	 * thread has reached the iteration, so it is not inside a read that loaded
	 * the old manifest. */
	spdk_for_each_channel_continue(i, 0);
}

static void
export_swap_done(struct spdk_io_channel_iter *i, int status)
{
	struct export_swap_ctx *ctx = spdk_io_channel_iter_get_ctx(i);

	/* Past the grace period: the old manifest cannot be reachable from any
	 * reader now, so this reference is the last thing holding it. */
	s3_export_manifest_unref(ctx->old);

	if (ctx->cb) {
		ctx->cb(ctx->cb_arg, status);
	}
	free(ctx);
}

int
s3_export_bs_dev_swap_manifest(struct spdk_bs_dev *bs_dev,
			       struct s3_export_manifest *m,
			       s3_export_bs_dev_swap_cb cb, void *cb_arg)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;
	struct export_swap_ctx *ctx;

	if (!bs_dev || !m) {
		return -EINVAL;
	}

	/* A different export entirely. Nothing else here would catch it -- the
	 * geometry could match by coincidence -- and the result would be a clone
	 * silently reading another volume. */
	if (strcmp(m->uuid_str, dev->m->uuid_str) != 0) {
		SPDK_ERRLOG("export %s: refusing a manifest for %s\n",
			    dev->m->uuid_str, m->uuid_str);
		return -EINVAL;
	}

	/* blobstore sized the clone against the old manifest and has been serving
	 * that size since. A parent that changed length would put the end of the
	 * volume somewhere the clone does not agree with, so this is refused rather
	 * than adopted -- reading through a shorter parent is data loss, and a
	 * longer one is data blobstore will never ask for. */
	if (m->size_bytes != dev->m->size_bytes) {
		SPDK_ERRLOG("export %s: refusing a manifest of %" PRIu64 " byte(s) "
			    "against the %" PRIu64 " this device was built for\n",
			    dev->m->uuid_str, m->size_bytes, dev->m->size_bytes);
		return -EINVAL;
	}

	if (m->chunk_size != dev->m->chunk_size) {
		SPDK_ERRLOG("export %s: refusing a manifest with chunk_size %u "
			    "against the %u this device was built for\n",
			    dev->m->uuid_str, m->chunk_size, dev->m->chunk_size);
		return -EINVAL;
	}

	/* Older or identical: reported as -EALREADY rather than success so the
	 * caller can tell "already on this generation" from "now reading a new
	 * one". A refetch must not treat that as data loss -- another refetch
	 * may have swapped while this GET still used the pre-swap keys. */
	if (m->generation <= dev->m->generation) {
		return -EALREADY;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->old    = dev->m;
	ctx->cb     = cb;
	ctx->cb_arg = cb_arg;

	s3_export_manifest_ref(m);

	/* Publish one snapshot. Each I/O captures `m` at submit and uses that
	 * manifest's geometry until it completes, so these stores do not have to
	 * appear atomic with a concurrent read's split loop. Rejecting a chunk-size
	 * change above is what keeps is_zeroes and a in-flight read from disagreeing
	 * on which byte range a chunk index covers. */
	dev->chunk_shift      = (uint32_t)spdk_u32log2(m->chunk_size);
	dev->blocks_per_chunk = m->chunk_size / S3LVOL_BLOCK_SIZE;
	__atomic_store_n(&dev->current_generation, m->generation, __ATOMIC_RELEASE);
	__atomic_store_n(&dev->m, m, __ATOMIC_RELEASE);

	if (dev->on_swap) {
		dev->on_swap(dev->on_swap_arg, m);
	}

	SPDK_NOTICELOG("export %s: now reading generation %u (%s), was %u (%s)\n",
		       m->uuid_str, m->generation,
		       m->layout == S3_EXPORT_LAYOUT_REF ? "ref" : "copied",
		       ctx->old->generation,
		       ctx->old->layout == S3_EXPORT_LAYOUT_REF ? "ref" : "copied");

	spdk_for_each_channel(dev, export_swap_channel, ctx, export_swap_done);
	return 0;
}

/* ==========================================================================
 * Refetch: what a 404 means, and what to do about it
 *
 * A ref export names the source's live objects. The source's way of taking its
 * snapshot back is to materialise the export -- copy the data under the export's
 * own prefix, rewrite the manifest as dense with `generation` bumped, and delete
 * the originals. Nothing tells the importer. The 404 *is* the notification.
 *
 * So a missing object is not failed straight back to blobstore. The I/O is held,
 * the manifest fetched again, and the read run once more against whatever came
 * back. A caller sees a slower read rather than an error, which matters because
 * materialising is an ordinary operation on the source and the importer cannot
 * anticipate it.
 *
 * It all runs on home_thread. Several threads can hit 404 on the same export at
 * the same moment -- one large read splits into many chunk GETs, and after a
 * materialisation every one of them misses -- so they have to produce one refetch
 * between them. The waiters queue there and each is sent back to its own thread
 * to be retried, because blobstore's completion belongs on the thread that
 * issued the read.
 * ========================================================================== */

/* Big enough for any manifest this can produce, and small enough that a wrong
 * answer from HEAD cannot turn into a huge allocation. Matches the import path. */
#define EXPORT_MANIFEST_MAX (64u * 1024 * 1024)

struct export_refetch_ctx {
	struct s3_export_dev *dev;
	char                  key[S3_EXPORT_KEY_MAX];
	uint64_t              size;
	char                 *body;
};

static void export_retry_msg(void *arg);

static int
export_retry_delivery_poll(void *arg)
{
	struct s3_export_dev *dev = arg;
	struct s3_export_io *io, *tmp;

	TAILQ_FOREACH_SAFE(io, &dev->refetch_delivery_waiters, waiter_link, tmp) {
		if (spdk_thread_send_msg(io->origin, export_retry_msg, io) == 0) {
			TAILQ_REMOVE(&dev->refetch_delivery_waiters, io, waiter_link);
		}
	}
	if (!TAILQ_EMPTY(&dev->refetch_delivery_waiters)) {
		return SPDK_POLLER_BUSY;
	}
	if (__atomic_load_n(&dev->destroying, __ATOMIC_ACQUIRE)) {
		export_refetch_delivery_stop(dev);
	}
	return SPDK_POLLER_IDLE;
}

/* Release every waiter back to its own thread. Called on home_thread once the
 * refetch has settled, whatever the outcome: a waiter that is not retried still
 * has to be completed, or blobstore waits for a callback that never comes. */
static void
export_release_waiters(struct s3_export_dev *dev, bool retry)
{
	struct s3_export_io *io, *tmp;

	TAILQ_FOREACH_SAFE(io, &dev->refetch_waiters, waiter_link, tmp) {
		TAILQ_REMOVE(&dev->refetch_waiters, io, waiter_link);
		/* Set either way: it is what stops the retry from refetching again. */
		io->retried = true;
		/* Cleared on the retry path, kept on the give-up path. The pair is what
		 * export_retry_msg() reads to tell "run it again against the new
		 * manifest" from "there is no new manifest, report the 404". */
		io->saw_missing = !retry;
		if (spdk_thread_send_msg(io->origin, export_retry_msg, io) != 0) {
			TAILQ_INSERT_TAIL(&dev->refetch_delivery_waiters, io,
					 waiter_link);
		}
	}
	dev->refetch_in_flight = false;
}

static void
export_refetch_free(struct export_refetch_ctx *ctx)
{
	free(ctx->body);
	free(ctx);
}

static void
export_refetch_swapped(void *cb_arg, int status)
{
	struct export_refetch_ctx *ctx = cb_arg;

	/* The swap is complete and the old manifest is gone, so the waiters can now
	 * be re-run against the new one. */
	export_release_waiters(ctx->dev, true);
	export_refetch_free(ctx);
}

static void
export_refetch_got_manifest(void *cb_arg, uint64_t bytes_read, int status)
{
	struct export_refetch_ctx *ctx = cb_arg;
	struct s3_export_dev *dev = ctx->dev;
	struct s3_export_manifest *m = NULL;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("export %s: could not read the manifest back: %s\n",
			    dev->m->uuid_str, spdk_strerror(-status));
		goto give_up;
	}

	rc = s3_export_manifest_parse(ctx->body, bytes_read, &m);
	if (rc != 0) {
		SPDK_ERRLOG("export %s: the refetched manifest does not parse: %s\n",
			    dev->m->uuid_str, spdk_strerror(-rc));
		goto give_up;
	}

	rc = s3_export_bs_dev_swap_manifest(&dev->bs_dev, m,
					    export_refetch_swapped, ctx);
	/* The device holds its own reference now, or refused it; either way this
	 * one is done with. */
	s3_export_manifest_unref(m);

	if (rc == 0) {
		__atomic_fetch_add(&dev->refetches, 1, __ATOMIC_RELAXED);
		return;
	}

	if (s3_export_bs_dev_refetch_already_current(rc)) {
		/* Already on this generation. Retry against the keys now in
		 * dev->m: a GET that left before a swap 404s after it, and a
		 * second refetch then hits -EALREADY even though the new
		 * objects are there. Genuine deletion still fails -- the retry
		 * 404s and io->retried blocks another refetch. */
		export_release_waiters(dev, true);
		export_refetch_free(ctx);
		return;
	}

	SPDK_ERRLOG("export %s: the refetched manifest was refused: %s\n",
		    dev->m->uuid_str, spdk_strerror(-rc));

give_up:
	export_release_waiters(dev, false);
	export_refetch_free(ctx);
}

static void
export_refetch_head_done(void *cb_arg, int status)
{
	struct export_refetch_ctx *ctx = cb_arg;
	struct s3_export_dev *dev = ctx->dev;
	int rc;

	if (status != 0) {
		/* A 404 here is worth saying out loud: the manifest itself is gone, so
		 * there is nothing to refetch and nothing will fix this export. */
		SPDK_ERRLOG("export %s: cannot stat the manifest '%s': %s\n",
			    dev->m->uuid_str, ctx->key, spdk_strerror(-status));
		goto give_up;
	}
	if (ctx->size == 0 || ctx->size > EXPORT_MANIFEST_MAX) {
		SPDK_ERRLOG("export %s: manifest '%s' has an implausible size of %"
			    PRIu64 " bytes\n", dev->m->uuid_str, ctx->key, ctx->size);
		goto give_up;
	}

	ctx->body = malloc(ctx->size);
	if (!ctx->body) {
		goto give_up;
	}

	rc = s3_get_range(dev->client, ctx->key, 0, ctx->size, ctx->body,
			  export_refetch_got_manifest, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("export %s: could not submit the manifest read: %s\n",
			    dev->m->uuid_str, spdk_strerror(-rc));
		goto give_up;
	}
	return;

give_up:
	export_release_waiters(dev, false);
	export_refetch_free(ctx);
}

/* On home_thread. Queues the I/O and starts a refetch if one is not already
 * under way. */
static void
export_refetch_msg(void *arg)
{
	struct s3_export_io *io = arg;
	struct s3_export_dev *dev = io->dev;
	struct export_refetch_ctx *ctx;
	int rc;

	TAILQ_INSERT_TAIL(&dev->refetch_waiters, io, waiter_link);

	/* One refetch for all of them. After a materialisation every chunk read
	 * misses at once, and one GET per miss would be a burst of identical
	 * requests -- and several swaps racing to replace the same manifest. */
	if (dev->refetch_in_flight) {
		return;
	}
	dev->refetch_in_flight = true;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		export_release_waiters(dev, false);
		return;
	}
	ctx->dev = dev;
	s3_export_manifest_key(dev->m->uuid_str, ctx->key, sizeof(ctx->key));

	SPDK_NOTICELOG("export %s: a chunk is missing; refetching the manifest "
		       "(generation %u now)\n", dev->m->uuid_str, dev->m->generation);

	rc = s3_head(dev->client, ctx->key, &ctx->size, export_refetch_head_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("export %s: could not submit the manifest stat: %s\n",
			    dev->m->uuid_str, spdk_strerror(-rc));
		export_release_waiters(dev, false);
		export_refetch_free(ctx);
	}
}

static void
export_io_start_refetch(struct s3_export_io *io)
{
	int rc;

	rc = spdk_thread_send_msg(io->dev->home_thread, export_refetch_msg, io);
	if (rc != 0) {
		/* Still on the submitting thread, so completing here preserves
		 * blobstore's channel affinity. Treat a failed dispatch like a
		 * failed refetch instead of stranding the I/O and its device ref. */
		SPDK_ERRLOG("export %s: could not dispatch manifest refetch: %s\n",
			    io->dev->m->uuid_str, spdk_strerror(-rc));
		io->retried = true;
		io->saw_missing = true;
		export_retry_msg(io);
	}
}

/* Back on the I/O's own thread, with the manifest already replaced. */
static void
export_retry_msg(void *arg)
{
	struct s3_export_io *io = arg;
	struct spdk_bs_dev *bs_dev = &io->dev->bs_dev;
	struct spdk_bs_dev_cb_args *cb_args = io->cb_args;
	void *payload = io->payload;
	uint64_t lba = io->lba;
	uint32_t lba_count = io->lba_count;

	if (io->retried && io->saw_missing) {
		/* The refetch gave up. Completed rather than re-read, since reading
		 * again would produce the same 404. */
		export_io_complete(io);
		return;
	}

	/* A fresh read against the new manifest, so the whole splitting and hole
	 * logic is reused rather than a second, subtly different, copy of it.
	 *
	 * is_retry carries across. Without it the new I/O would start with a clean
	 * slate, and a 404 against the manifest that was *just* fetched would send
	 * it round again -- a materialisation the source is still part way through
	 * would loop for as long as it lasted. One refetch per I/O is the rule, and
	 * this is where it is kept.
	 *
	 * The old I/O is dropped here: export_read() builds its own, and this one's
	 * captured manifest is released with it. */
	export_io_drop(io);
	export_read_internal(bs_dev, cb_args->channel, payload, lba, lba_count,
			     cb_args, true);
}

/* ==========================================================================
 * Construction
 * ========================================================================== */

int
s3_export_bs_dev_create(struct s3_client *client, struct s3_export_manifest *m,
			struct s3_cache *shared_cache, struct spdk_bs_dev **out)
{
	struct s3_export_dev *dev;

	if (!client || !m || !out) {
		return -EINVAL;
	}
	if (m->size_bytes % S3LVOL_BLOCK_SIZE != 0) {
		return -EINVAL;
	}

	dev = calloc(1, sizeof(*dev));
	if (!dev) {
		return -ENOMEM;
	}
	if (pthread_mutex_init(&dev->fill_lock, NULL) != 0) {
		free(dev);
		return -ENOMEM;
	}
	TAILQ_INIT(&dev->fills);
	TAILQ_INIT(&dev->fill_lru);
	TAILQ_INIT(&dev->fill_pending);
	dev->current_generation = m->generation;

	dev->client           = client;
	dev->m                = m;
	dev->shared_cache     = shared_cache;
	if (shared_cache) {
		snprintf(dev->cache_endpoint, sizeof(dev->cache_endpoint), "%s",
			 m->src.endpoint);
		snprintf(dev->cache_bucket, sizeof(dev->cache_bucket), "%s",
			 s3_client_bucket(client));
	}
	dev->chunk_shift      = (uint32_t)spdk_u32log2(m->chunk_size);
	dev->blocks_per_chunk = m->chunk_size / S3LVOL_BLOCK_SIZE;
	snprintf(dev->name, sizeof(dev->name), "esnap:%s", m->uuid_str);

	/* The refetch machinery's thread. Taken here rather than from the first
	 * read, because it has to be a thread that stays: blobstore's channels come
	 * and go with the poll groups, and a refetch outliving the thread that
	 * started it would send a message to an exited thread, which aborts. */
	dev->home_thread = spdk_get_thread();
	TAILQ_INIT(&dev->refetch_waiters);
	TAILQ_INIT(&dev->refetch_delivery_waiters);
	/* Allocate the rare send-failure retry path before admitting any I/O.
	 * Registering it only after a send failure could itself fail under the
	 * same memory pressure and strand every waiter. It normally polls idle. */
	dev->refetch_delivery_poller =
		SPDK_POLLER_REGISTER(export_retry_delivery_poll, dev, 1000);
	if (!dev->refetch_delivery_poller) {
		pthread_mutex_destroy(&dev->fill_lock);
		free(dev);
		return -ENOMEM;
	}
	dev->refetch_delivery_lifetime_ref = true;
	dev->async_refs++;

	s3_export_manifest_ref(m);

	spdk_io_device_register(dev, export_channel_create_cb, export_channel_destroy_cb,
				sizeof(struct s3_export_channel), dev->name);

	dev->bs_dev.blockcnt       = m->size_bytes / S3LVOL_BLOCK_SIZE;
	dev->bs_dev.blocklen       = S3LVOL_BLOCK_SIZE;
	dev->bs_dev.phys_blocklen  = S3LVOL_BLOCK_SIZE;

	dev->bs_dev.create_channel  = export_create_channel;
	dev->bs_dev.destroy_channel = export_destroy_channel;
	dev->bs_dev.destroy         = export_destroy;
	dev->bs_dev.read            = export_read;
	dev->bs_dev.readv           = export_readv;
	dev->bs_dev.readv_ext       = export_readv_ext;
	dev->bs_dev.write           = export_write;
	dev->bs_dev.writev          = export_writev;
	dev->bs_dev.writev_ext      = export_writev_ext;
	dev->bs_dev.write_zeroes    = export_write_zeroes;
	dev->bs_dev.unmap           = export_unmap;
	dev->bs_dev.flush           = export_flush;
	dev->bs_dev.is_zeroes       = export_is_zeroes;
	dev->bs_dev.is_range_valid  = export_is_range_valid;
	dev->bs_dev.translate_lba   = export_translate_lba;
	dev->bs_dev.is_degraded     = export_is_degraded;
	/* copy stays NULL: the destination of a CoW copy is the lvstore bs_dev. */

	SPDK_NOTICELOG("Imported export %s as a read-only parent: %" PRIu64 " bytes "
		       "(%" PRIu64 " blocks), %" PRIu64 " of %" PRIu64 " chunk(s) "
		       "present, source %s/%s\n", m->uuid_str, m->size_bytes,
		       dev->bs_dev.blockcnt, m->present_chunks, m->num_chunks,
		       m->src.bucket, m->src.prefix);

	/* Named individually when there is more than one, because the line above
	 * would otherwise describe a derived import as depending on a single prefix.
	 * Which prefixes an import reads from is what decides whose lease has to be
	 * renewed and whose snapshot cannot be deleted, so an operator looking into
	 * either has to be able to see it. */
	if (m->num_srcs > 1) {
		uint32_t si;

		for (si = 1; si < m->num_srcs; si++) {
			SPDK_NOTICELOG("Export %s also reads prefix '%s' (export %s)\n",
				       m->uuid_str, m->srcs[si].prefix,
				       m->srcs[si].export_uuid[0] ? m->srcs[si].export_uuid
				       : "unnamed");
		}
	}

	*out = &dev->bs_dev;
	return 0;
}

void
s3_export_bs_dev_set_on_swap(struct spdk_bs_dev *bs_dev,
			      s3_export_bs_dev_on_swap_fn fn, void *arg)
{
	struct s3_export_dev *dev = (struct s3_export_dev *)bs_dev;

	if (!bs_dev) {
		return;
	}
	dev->on_swap = fn;
	dev->on_swap_arg = arg;
}
