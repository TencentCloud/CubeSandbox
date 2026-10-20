/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "s3lvol/s3_writeback.h"

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <unistd.h>

#include "spdk/crc32.h"

#define S3_WB_MAGIC           UINT64_C(0x53425752494e4731) /* "SBWRING1" */
#define S3_WB_VERSION         4u
#define S3_WB_HEADER_SIZE     4096u
#define S3_WB_RECORD_SIZE     64u
#define S3_WB_PAD_TYPE        0xffu

/* The high nibble is the one's complement of the low nibble.  State is a
 * crash-recovery decision field, so accepting a single torn/corrupt byte here
 * could reclaim an acknowledged record prematurely. */
#define S3_WB_STATE_PREPARING 0xe1u
#define S3_WB_STATE_PENDING   0xd2u
#define S3_WB_STATE_COMPLETE  0xc3u

struct s3_wb_header {
	uint64_t magic;
	uint32_t version;
	uint32_t header_size;
	uint32_t record_size;
	uint32_t alignment;
	uint64_t capacity;
	uint8_t instance_id[S3_WRITEBACK_INSTANCE_ID_SIZE];
	uint32_t layout_crc32c;
	uint32_t reserved0;

	uint64_t head;
	uint64_t tail;
	uint64_t next_seq;
	uint64_t publishes;
	uint64_t full_events;
	uint64_t pending_records;
	uint64_t completed_records;
	uint32_t owner_pid;
	uint32_t reserved1;
};

struct s3_wb_record_header {
	uint32_t total_len;
	uint16_t header_len;
	uint8_t type;
	uint8_t state;
	uint64_t seq;
	uint64_t lba;
	uint32_t nblocks;
	uint32_t payload_len;
	uint64_t chunk_index;
	int32_t outcome;
	uint32_t crc32c;
	uint64_t reserved0;
	uint64_t reserved1;
};

_Static_assert(sizeof(struct s3_wb_record_header) == S3_WB_RECORD_SIZE,
	       "write-back record header must be 64 bytes");
_Static_assert(sizeof(struct s3_wb_header) <= S3_WB_HEADER_SIZE,
	       "write-back header must fit its page");

struct s3_writeback {
	int fd;
	int dirfd;
	size_t map_size;
	void *mapping;
	struct s3_wb_header *header;
	uint8_t *data;
	pid_t owner_pid;
	char shm_name[S3_WRITEBACK_SHM_NAME_MAX];
	uint64_t budget_bytes;
};

static int
writeback_open_private_dir(void)
{
	char path[96];
	struct stat st;
	int dirfd;

	snprintf(path, sizeof(path), "/dev/shm/s3lvol-wb-%lu",
		 (unsigned long)geteuid());
	if (mkdir(path, 0700) != 0 && errno != EEXIST) {
		return -errno;
	}
	dirfd = open(path, O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
	if (dirfd < 0) {
		return -errno;
	}
	if (fstat(dirfd, &st) != 0) {
		int rc = -errno;

		close(dirfd);
		return rc;
	}
	if (!S_ISDIR(st.st_mode) || st.st_uid != geteuid() ||
	    (st.st_mode & 0777) != 0700 || st.st_nlink < 2) {
		close(dirfd);
		return -EACCES;
	}
	return dirfd;
}

static int
writeback_migrate_legacy(int dirfd, const char *leaf)
{
	char path[128];
	struct stat st;
	int fd;

	snprintf(path, sizeof(path), "/dev/shm/%s", leaf);
	fd = open(path, O_RDWR | O_CLOEXEC | O_NOFOLLOW);
	if (fd < 0) {
		return errno == ENOENT ? -ENOENT : -errno;
	}
	if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) ||
	    st.st_uid != geteuid() || (st.st_mode & 0077) != 0 ||
	    st.st_nlink != 1) {
		close(fd);
		return -EACCES;
	}
	if (flock(fd, LOCK_EX | LOCK_NB) != 0) {
		int rc = errno == EWOULDBLOCK ? -EBUSY : -errno;

		close(fd);
		return rc;
	}
	if (renameat2(AT_FDCWD, path, dirfd, leaf, RENAME_NOREPLACE) != 0) {
		int rc = -errno;

		close(fd);
		return rc;
	}
	return fd;
}

#define S3_WRITEBACK_DEFAULT_TOTAL_CAPACITY \
	(UINT64_C(8) * 1024 * 1024 * 1024)

static pthread_mutex_t g_budget_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_once_t g_budget_once = PTHREAD_ONCE_INIT;
static uint64_t g_budget_limit = S3_WRITEBACK_DEFAULT_TOTAL_CAPACITY;
static uint64_t g_budget_used;
static uint32_t g_budget_active;

static void
writeback_budget_init(void)
{
	const char *value = getenv("S3LVOL_WRITEBACK_TOTAL_MB");
	char *end = NULL;
	unsigned long long mb;

	if (!value || value[0] == '\0') {
		return;
	}
	errno = 0;
	mb = strtoull(value, &end, 10);
	if (errno == 0 && end && *end == '\0' && mb >= 64 &&
	    mb <= UINT64_MAX / (1024 * 1024)) {
		g_budget_limit = (uint64_t)mb * 1024 * 1024;
	}
}

static int
writeback_budget_reserve(struct s3_writeback *writeback, uint64_t bytes)
{
	int rc = 0;

	pthread_once(&g_budget_once, writeback_budget_init);
	pthread_mutex_lock(&g_budget_lock);
	if (bytes > g_budget_limit - g_budget_used) {
		rc = -ENOSPC;
	} else {
		g_budget_used += bytes;
		g_budget_active++;
		writeback->budget_bytes = bytes;
	}
	pthread_mutex_unlock(&g_budget_lock);
	return rc;
}

static void
writeback_budget_release(struct s3_writeback *writeback)
{
	if (writeback->budget_bytes == 0) {
		return;
	}
	pthread_mutex_lock(&g_budget_lock);
	assert(g_budget_used >= writeback->budget_bytes);
	g_budget_used -= writeback->budget_bytes;
	assert(g_budget_active > 0);
	g_budget_active--;
	pthread_mutex_unlock(&g_budget_lock);
	writeback->budget_bytes = 0;
}

struct s3_wb_crc_fields {
	uint8_t type;
	uint8_t reserved[3];
	uint32_t payload_len;
	uint64_t seq;
	uint64_t lba;
	uint32_t nblocks;
	uint32_t reserved2;
	uint64_t chunk_index;
};

static void export_record(const struct s3_wb_record_header *record,
			  struct s3_writeback_record *out,
			  uint64_t position);

static uint64_t
align_up_u64(uint64_t value, uint32_t alignment)
{
	return (value + alignment - 1) & ~((uint64_t)alignment - 1);
}

static bool
valid_alignment(uint32_t alignment)
{
	return alignment >= S3_WRITEBACK_DEFAULT_ALIGNMENT && alignment <= 4096 &&
	       (alignment & (alignment - 1)) == 0;
}

static bool
instance_id_is_zero(const uint8_t *instance_id)
{
	for (uint32_t i = 0; i < S3_WRITEBACK_INSTANCE_ID_SIZE; i++) {
		if (instance_id[i] != 0) {
			return false;
		}
	}
	return true;
}

static uint32_t
layout_crc(const struct s3_wb_header *header)
{
	struct {
		uint64_t magic;
		uint32_t version;
		uint32_t header_size;
		uint32_t record_size;
		uint32_t alignment;
		uint64_t capacity;
	} fields;

	memset(&fields, 0, sizeof(fields));
	fields.magic = S3_WB_MAGIC;
	fields.version = header->version;
	fields.header_size = header->header_size;
	fields.record_size = header->record_size;
	fields.alignment = header->alignment;
	fields.capacity = header->capacity;
	return spdk_crc32c_update(&fields, sizeof(fields), ~0u);
}

static uint32_t
record_crc(const struct s3_wb_record_header *record, const void *payload)
{
	struct s3_wb_crc_fields fields;
	uint32_t crc;

	memset(&fields, 0, sizeof(fields));
	fields.type = record->type;
	fields.payload_len = record->payload_len;
	fields.seq = record->seq;
	fields.lba = record->lba;
	fields.nblocks = record->nblocks;
	fields.chunk_index = record->chunk_index;
	crc = spdk_crc32c_update(&fields, sizeof(fields), ~0u);
	return spdk_crc32c_update(payload, record->payload_len, crc);
}

static int32_t
wal_seq_tag(uint64_t wal_seq)
{
	uint32_t tag = spdk_crc32c_update(&wal_seq, sizeof(wal_seq), ~0u);

	/* Zero is the explicit "assignment is absent or interrupted" marker. */
	return (int32_t)(tag != 0 ? tag : 1);
}

static uint64_t
hash_name(const char *name, uint64_t seed)
{
	const uint8_t *p = (const uint8_t *)name;
	uint64_t hash = seed;

	while (*p != '\0') {
		hash ^= *p++;
		hash *= UINT64_C(1099511628211);
	}
	hash ^= hash >> 33;
	hash *= UINT64_C(0xff51afd7ed558ccd);
	hash ^= hash >> 33;
	hash *= UINT64_C(0xc4ceb9fe1a85ec53);
	return hash ^ (hash >> 33);
}

int
s3_writeback_shm_name(const char *lvstore_name, char *name, size_t name_size)
{
	uint64_t first, second;
	int length;

	if (!lvstore_name || lvstore_name[0] == '\0' || !name ||
	    name_size < S3_WRITEBACK_SHM_NAME_MAX) {
		return -EINVAL;
	}
	first = hash_name(lvstore_name, UINT64_C(1469598103934665603));
	second = hash_name(lvstore_name, UINT64_C(7809847782465536322));
	length = snprintf(name, name_size, "/s3lvol-wb-%016llx%016llx",
			  (unsigned long long)first, (unsigned long long)second);
	if (length < 0 || (size_t)length >= name_size) {
		return -ENAMETOOLONG;
	}
	return 0;
}

static int
claim_owner(struct s3_wb_header *header, pid_t pid)
{
	/* The caller already owns the exclusive flock on this object. That lock,
	 * unlike a persisted PID, cannot be stale or be confused by PID reuse. */
	__atomic_store_n(&header->owner_pid, (uint32_t)pid, __ATOMIC_RELEASE);
	return 0;
}

static void
release_owner(struct s3_wb_header *header, pid_t pid)
{
	uint32_t expected = (uint32_t)pid;

	(void)__atomic_compare_exchange_n(&header->owner_pid, &expected, 0, false,
					  __ATOMIC_RELEASE, __ATOMIC_RELAXED);
}

static int
validate_header(const struct s3_wb_header *header, size_t object_size)
{
	if (__atomic_load_n(&header->magic, __ATOMIC_ACQUIRE) != S3_WB_MAGIC) {
		return -EBADMSG;
	}
	if (header->version != S3_WB_VERSION ||
	    header->header_size != S3_WB_HEADER_SIZE ||
	    header->record_size != S3_WB_RECORD_SIZE ||
	    !valid_alignment(header->alignment)) {
		return -EPROTONOSUPPORT;
	}
	if (header->capacity < header->alignment * 4u ||
	    header->capacity % header->alignment != 0 ||
	    header->capacity > SIZE_MAX - S3_WB_HEADER_SIZE ||
	    object_size != S3_WB_HEADER_SIZE + (size_t)header->capacity) {
		return -EBADMSG;
	}
	if (header->layout_crc32c != layout_crc(header)) {
		return -EBADMSG;
	}
	return 0;
}

static bool
recoverable_init_header(const struct s3_wb_header *header, uint64_t capacity,
			uint32_t alignment,
			const uint8_t *instance_id)
{
	struct s3_wb_header expected = {0};

	expected.version = S3_WB_VERSION;
	expected.header_size = S3_WB_HEADER_SIZE;
	expected.record_size = S3_WB_RECORD_SIZE;
	expected.alignment = alignment;
	expected.capacity = capacity;
	expected.next_seq = 1;
	if (instance_id) {
		memcpy(expected.instance_id, instance_id,
		       S3_WRITEBACK_INSTANCE_ID_SIZE);
	}
	expected.layout_crc32c = layout_crc(&expected);

#define INIT_FIELD_OK(field) \
	(header->field == 0 || header->field == expected.field)
	if (header->magic != 0 || !INIT_FIELD_OK(version) ||
	    !INIT_FIELD_OK(header_size) || !INIT_FIELD_OK(record_size) ||
	    !INIT_FIELD_OK(alignment) || !INIT_FIELD_OK(capacity) ||
	    !INIT_FIELD_OK(layout_crc32c) || !INIT_FIELD_OK(next_seq) ||
	    header->head != 0 || header->tail != 0 ||
	    header->publishes != 0 || header->full_events != 0 ||
	    header->pending_records != 0 || header->completed_records != 0) {
		return false;
	}
#undef INIT_FIELD_OK
	return instance_id_is_zero(header->instance_id) ||
	       (instance_id &&
		memcmp(header->instance_id, instance_id,
		       S3_WRITEBACK_INSTANCE_ID_SIZE) == 0);
}

static int
record_at(const struct s3_writeback *writeback, uint64_t position,
	  struct s3_wb_record_header **out, uint64_t *advance)
{
	const struct s3_wb_header *header = writeback->header;
	uint64_t offset = position % header->capacity;
	uint64_t remaining = header->capacity - offset;
	struct s3_wb_record_header *record;
	uint64_t expected_len;
	uint8_t state;

	if (remaining < S3_WB_RECORD_SIZE) {
		*out = NULL;
		*advance = remaining;
		return 0;
	}

	record = (struct s3_wb_record_header *)(writeback->data + offset);
	state = __atomic_load_n(&record->state, __ATOMIC_ACQUIRE);
	if (state == S3_WB_STATE_PREPARING) {
		return -EINPROGRESS;
	}
	if (state != S3_WB_STATE_PENDING && state != S3_WB_STATE_COMPLETE) {
		return -EBADMSG;
	}
	if (record->type == S3_WB_PAD_TYPE) {
		if (record->total_len != remaining ||
		    record->header_len != S3_WB_RECORD_SIZE) {
			return -EBADMSG;
		}
		*out = record;
		*advance = remaining;
		return 0;
	}
	if (record->type < S3_WRITEBACK_WRITE ||
	    record->type > S3_WRITEBACK_UNMAP ||
	    record->header_len != header->alignment ||
	    record->seq == 0 || record->nblocks == 0 ||
	    record->lba > UINT64_MAX - record->nblocks) {
		return -EBADMSG;
	}
	if (state == S3_WB_STATE_PENDING) {
		int32_t tag = __atomic_load_n(&record->outcome, __ATOMIC_ACQUIRE);

		/* A zero tag is a deliberately invalidated assignment. A crash at
		 * any following store therefore causes conservative WAL re-append,
		 * rather than making the whole recoverable ring unreadable. */
		if (tag != 0 &&
		    (record->reserved1 != ~record->reserved0 ||
		     tag != wal_seq_tag(record->reserved0))) {
			return -EBADMSG;
		}
	} else if (record->reserved1 != ~record->reserved0) {
		return -EBADMSG;
	}
	if (record->type != S3_WRITEBACK_WRITE && record->payload_len != 0) {
		return -EBADMSG;
	}
	expected_len = align_up_u64((uint64_t)record->header_len +
				   record->payload_len, header->alignment);
	if (expected_len > remaining || expected_len > UINT32_MAX ||
	    record->total_len != expected_len ||
	    record->crc32c != record_crc(record,
					 (uint8_t *)record + record->header_len)) {
		return -EBADMSG;
	}
	*out = record;
	*advance = expected_len;
	return 0;
}

/* Fast path for records already validated when they were published or during
 * reopen. It deliberately does not rescan the payload CRC on completion and
 * reclaim; doing that twice per 1 MiB write adds 20 GiB of memory traffic to a
 * 10 GiB pause without improving process-local correctness. */
static int
record_at_trusted(const struct s3_writeback *writeback, uint64_t position,
		  struct s3_wb_record_header **out, uint64_t *advance)
{
	const struct s3_wb_header *header = writeback->header;
	uint64_t offset = position % header->capacity;
	uint64_t remaining = header->capacity - offset;
	struct s3_wb_record_header *record;
	uint8_t state;

	if (remaining < S3_WB_RECORD_SIZE) {
		*out = NULL;
		*advance = remaining;
		return 0;
	}
	record = (struct s3_wb_record_header *)(writeback->data + offset);
	state = __atomic_load_n(&record->state, __ATOMIC_ACQUIRE);
	if (state != S3_WB_STATE_PENDING && state != S3_WB_STATE_COMPLETE) {
		return -EBADMSG;
	}
	if (record->type == S3_WB_PAD_TYPE) {
		if (record->total_len != remaining) {
			return -EBADMSG;
		}
		*out = record;
		*advance = remaining;
		return 0;
	}
	if (record->total_len < header->alignment ||
	    record->total_len > remaining ||
	    record->total_len % header->alignment != 0) {
		return -EBADMSG;
	}
	*out = record;
	*advance = record->total_len;
	return 0;
}

static int
recover_records(struct s3_writeback *writeback)
{
	struct s3_wb_header *header = writeback->header;
	uint64_t head = __atomic_load_n(&header->head, __ATOMIC_ACQUIRE);
	uint64_t tail = __atomic_load_n(&header->tail, __ATOMIC_ACQUIRE);
	uint64_t position, advance;
	uint64_t last_seq = 0;
	uint64_t pending_records = 0;
	uint64_t completed_records = 0;
	struct s3_wb_record_header *record;
	int rc;

	if (head > tail || tail - head > header->capacity) {
		return -EBADMSG;
	}
	for (position = head; position < tail; position += advance) {
		rc = record_at(writeback, position, &record, &advance);
		if (rc == -EINPROGRESS) {
			uint64_t offset = position % header->capacity;
			struct s3_wb_record_header *preparing =
				(struct s3_wb_record_header *)(writeback->data + offset);

			/* Publication is single-writer and tail is advanced for only
			 * the current record. PREPARING anywhere but the exact tail
			 * is corruption, not a legitimate crash cut. */
			if (preparing->total_len < header->alignment ||
			    preparing->total_len > header->capacity - offset ||
			    position + preparing->total_len != tail) {
				return -EBADMSG;
			}
			__atomic_store_n(&header->tail, position, __ATOMIC_RELEASE);
			return 0;
		}
		if (rc != 0 || advance == 0 || advance > tail - position) {
			return -EBADMSG;
		}
		if (record && record->type != S3_WB_PAD_TYPE) {
			if (last_seq != 0 && record->seq <= last_seq) {
				return -EBADMSG;
			}
			last_seq = record->seq;
			if (__atomic_load_n(&record->state, __ATOMIC_ACQUIRE) ==
			    S3_WB_STATE_COMPLETE) {
				completed_records++;
			} else {
				pending_records++;
			}
		}
	}
	if (position != tail) {
		return -EBADMSG;
	}
	if (last_seq >= header->next_seq || header->next_seq == 0) {
		return -EBADMSG;
	}
	__atomic_store_n(&header->pending_records, pending_records,
			 __ATOMIC_RELEASE);
	__atomic_store_n(&header->completed_records, completed_records,
			 __ATOMIC_RELEASE);
	return 0;
}

static void
unmap_writeback(struct s3_writeback *writeback)
{
	if (!writeback) {
		return;
	}
	if (writeback->mapping != MAP_FAILED) {
		munmap(writeback->mapping, writeback->map_size);
	}
	if (writeback->fd >= 0) {
		close(writeback->fd);
	}
	if (writeback->dirfd >= 0) {
		close(writeback->dirfd);
	}
	writeback_budget_release(writeback);
	free(writeback);
}

int
s3_writeback_open(const struct s3_writeback_opts *opts,
		  struct s3_writeback **out)
{
	struct s3_writeback *writeback;
	struct stat st;
	uint64_t capacity;
	uint32_t alignment;
	bool created = false;
	int fd, rc, saved_errno;

	if (!opts || !opts->lvstore_name || !out ||
	    (opts->flags & ~S3_WRITEBACK_OPEN_CREATE) != 0) {
		return -EINVAL;
	}
	*out = NULL;
	alignment = opts->record_alignment ?
		    opts->record_alignment : S3_WRITEBACK_DEFAULT_ALIGNMENT;
	capacity = opts->capacity_bytes ?
		   opts->capacity_bytes : S3_WRITEBACK_DEFAULT_CAPACITY;
	if (!valid_alignment(alignment) || capacity < alignment * 4u ||
	    capacity % alignment != 0 ||
	    capacity > SIZE_MAX - S3_WB_HEADER_SIZE) {
		return -EINVAL;
	}

	writeback = calloc(1, sizeof(*writeback));
	if (!writeback) {
		return -ENOMEM;
	}
	writeback->fd = -1;
	writeback->dirfd = -1;
	writeback->mapping = MAP_FAILED;
	rc = s3_writeback_shm_name(opts->lvstore_name, writeback->shm_name,
				   sizeof(writeback->shm_name));
	if (rc != 0) {
		free(writeback);
		return rc;
	}
	writeback->dirfd = writeback_open_private_dir();
	if (writeback->dirfd < 0) {
		rc = writeback->dirfd;
		free(writeback);
		return rc;
	}

	fd = openat(writeback->dirfd, writeback->shm_name + 1,
		    O_RDWR | O_CLOEXEC | O_NOFOLLOW);
	if (fd < 0 && errno == ENOENT &&
	    (opts->flags & S3_WRITEBACK_OPEN_CREATE)) {
		fd = writeback_migrate_legacy(writeback->dirfd,
					      writeback->shm_name + 1);
		if (fd == -ENOENT) {
			fd = openat(writeback->dirfd, writeback->shm_name + 1,
				    O_RDWR | O_CREAT | O_EXCL | O_CLOEXEC |
				    O_NOFOLLOW, 0600);
			if (fd >= 0) {
				created = true;
			} else if (errno != EEXIST) {
				rc = -errno;
				unmap_writeback(writeback);
				return rc;
			}
		} else if (fd == -EEXIST) {
			fd = -1;
			errno = EEXIST;
		} else if (fd < 0) {
			rc = fd;
			unmap_writeback(writeback);
			return rc;
		}
	}
	if (fd < 0) {
		fd = openat(writeback->dirfd, writeback->shm_name + 1,
			    O_RDWR | O_CLOEXEC | O_NOFOLLOW);
		if (fd < 0) {
			rc = -errno;
			unmap_writeback(writeback);
			return rc;
		}
	}
	writeback->fd = fd;
	if (flock(fd, LOCK_EX | LOCK_NB) != 0) {
		rc = errno == EWOULDBLOCK ? -EBUSY : -errno;
		goto fail_created;
	}

	if (created) {
		rc = writeback_budget_reserve(writeback, capacity);
		if (rc != 0) {
			goto fail_created;
		}
		writeback->map_size = S3_WB_HEADER_SIZE + (size_t)capacity;
		if (ftruncate(fd, (off_t)writeback->map_size) != 0) {
			rc = -errno;
			goto fail_created;
		}
		/* A sparse tmpfs object can mmap successfully and later SIGBUS when
		 * the first write reaches an unreserved page. Reserve the complete
		 * ring up front so insufficient /dev/shm space is a normal create
		 * error, never a process-killing surprise on the write path. */
		rc = posix_fallocate(fd, 0, (off_t)writeback->map_size);
		if (rc != 0) {
			rc = -rc;
			goto fail_created;
		}
	} else {
		if (fstat(fd, &st) != 0) {
			rc = -errno;
			goto fail;
		}
		if (!S_ISREG(st.st_mode) || st.st_uid != geteuid() ||
		    (st.st_mode & 0077) != 0 || st.st_nlink != 1) {
			rc = -EACCES;
			goto fail;
		}
		if (st.st_size < S3_WB_HEADER_SIZE) {
			if (!(opts->flags & S3_WRITEBACK_OPEN_CREATE)) {
				rc = -EBADMSG;
				goto fail;
			}
			rc = writeback_budget_reserve(writeback, capacity);
			if (rc != 0) {
				goto fail;
			}
			writeback->map_size =
				S3_WB_HEADER_SIZE + (size_t)capacity;
			if (ftruncate(fd, (off_t)writeback->map_size) != 0) {
				rc = -errno;
				goto fail_created;
			}
			rc = posix_fallocate(fd, 0,
					    (off_t)writeback->map_size);
			if (rc != 0) {
				rc = -rc;
				goto fail_created;
			}
			created = true;
		} else {
			if ((uint64_t)st.st_size > SIZE_MAX) {
				rc = -EBADMSG;
				goto fail;
			}
			writeback->map_size = (size_t)st.st_size;
		}
	}

	writeback->mapping = mmap(NULL, writeback->map_size,
				  PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	if (writeback->mapping == MAP_FAILED) {
		rc = -errno;
		goto fail_created;
	}
	writeback->header = writeback->mapping;
	writeback->data = (uint8_t *)writeback->mapping + S3_WB_HEADER_SIZE;
	writeback->owner_pid = getpid();
	if (!created && (opts->flags & S3_WRITEBACK_OPEN_CREATE) &&
	    writeback->map_size == S3_WB_HEADER_SIZE + (size_t)capacity &&
	    recoverable_init_header(writeback->header, capacity, alignment,
				    opts->instance_id)) {
		rc = writeback_budget_reserve(writeback, capacity);
		if (rc != 0) {
			goto fail;
		}
		created = true;
		rc = posix_fallocate(fd, 0, (off_t)writeback->map_size);
		if (rc != 0) {
			rc = -rc;
			goto fail_created;
		}
	}

	if (created) {
		memset(writeback->mapping, 0, S3_WB_HEADER_SIZE);
		writeback->header->version = S3_WB_VERSION;
		writeback->header->header_size = S3_WB_HEADER_SIZE;
		writeback->header->record_size = S3_WB_RECORD_SIZE;
		writeback->header->alignment = alignment;
		writeback->header->capacity = capacity;
		if (opts->instance_id) {
			memcpy(writeback->header->instance_id, opts->instance_id,
			       S3_WRITEBACK_INSTANCE_ID_SIZE);
		}
		writeback->header->next_seq = 1;
		writeback->header->layout_crc32c = layout_crc(writeback->header);
		writeback->header->owner_pid = (uint32_t)writeback->owner_pid;
		__atomic_store_n(&writeback->header->magic, S3_WB_MAGIC,
				 __ATOMIC_RELEASE);
	} else {
		rc = validate_header(writeback->header, writeback->map_size);
		if (rc != 0) {
			goto fail;
		}
		if ((opts->capacity_bytes &&
		     opts->capacity_bytes != writeback->header->capacity) ||
		    (opts->record_alignment &&
		     opts->record_alignment != writeback->header->alignment)) {
			rc = -EINVAL;
			goto fail;
		}
		rc = writeback_budget_reserve(writeback,
					     writeback->header->capacity);
		if (rc != 0) {
			goto fail;
		}
		if (opts->instance_id &&
		    memcmp(opts->instance_id, writeback->header->instance_id,
			   S3_WRITEBACK_INSTANCE_ID_SIZE) != 0) {
			if (instance_id_is_zero(writeback->header->instance_id) &&
			    writeback->header->head == writeback->header->tail) {
				memcpy(writeback->header->instance_id,
				       opts->instance_id,
				       S3_WRITEBACK_INSTANCE_ID_SIZE);
			} else {
				rc = -EXDEV;
				goto fail;
			}
		}
	}

	if (!created) {
		rc = claim_owner(writeback->header, writeback->owner_pid);
		if (rc != 0) {
			goto fail;
		}
	}
	rc = recover_records(writeback);
	if (rc != 0) {
		release_owner(writeback->header, writeback->owner_pid);
		goto fail;
	}
	*out = writeback;
	return 0;

fail_created:
	saved_errno = errno;
	if (created) {
		(void)unlinkat(writeback->dirfd, writeback->shm_name + 1, 0);
	}
	errno = saved_errno;
fail:
	unmap_writeback(writeback);
	return rc;
}

void
s3_writeback_close(struct s3_writeback *writeback)
{
	if (!writeback) {
		return;
	}
	release_owner(writeback->header, writeback->owner_pid);
	unmap_writeback(writeback);
}

int
s3_writeback_publish(struct s3_writeback *writeback,
		     enum s3_writeback_record_type type,
		     uint64_t lba, uint32_t nblocks,
		     const void *payload, uint32_t payload_len,
		     uint64_t chunk_index, uint64_t *out_seq,
		     struct s3_writeback_record *out_record)
{
	struct s3_wb_header *header;
	struct s3_wb_record_header *record, *padding;
	uint64_t head, tail, offset, remaining, record_len, required, position;
	uint64_t seq;

	if (!writeback || nblocks == 0 ||
	    type < S3_WRITEBACK_WRITE || type > S3_WRITEBACK_UNMAP ||
	    (type == S3_WRITEBACK_WRITE && (!payload || payload_len == 0)) ||
	    (type != S3_WRITEBACK_WRITE && (payload || payload_len != 0))) {
		return -EINVAL;
	}
	header = writeback->header;
	record_len = align_up_u64((uint64_t)header->alignment + payload_len,
				  header->alignment);
	if (record_len > UINT32_MAX || record_len > header->capacity) {
		return -E2BIG;
	}

	head = __atomic_load_n(&header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&header->tail, __ATOMIC_RELAXED);
	if (head > tail || tail - head > header->capacity) {
		return -EBADMSG;
	}
	offset = tail % header->capacity;
	remaining = header->capacity - offset;
	required = record_len + (record_len > remaining ? remaining : 0);
	if (required > header->capacity - (tail - head)) {
		__atomic_add_fetch(&header->full_events, 1, __ATOMIC_RELAXED);
		return -ENOSPC;
	}

	position = tail;
	if (record_len > remaining) {
		if (remaining >= S3_WB_RECORD_SIZE) {
			padding = (struct s3_wb_record_header *)(writeback->data + offset);
			memset(padding, 0, S3_WB_RECORD_SIZE);
			padding->total_len = (uint32_t)remaining;
			padding->header_len = S3_WB_RECORD_SIZE;
			padding->type = S3_WB_PAD_TYPE;
			__atomic_store_n(&padding->state, S3_WB_STATE_COMPLETE,
					 __ATOMIC_RELEASE);
		}
		position += remaining;
		offset = 0;
	}

	record = (struct s3_wb_record_header *)(writeback->data + offset);
	memset(record, 0, header->alignment);
	seq = header->next_seq++;
	record->total_len = (uint32_t)record_len;
	record->header_len = (uint16_t)header->alignment;
	record->type = (uint8_t)type;
	record->seq = seq;
	record->lba = lba;
	record->nblocks = nblocks;
	record->payload_len = payload_len;
	record->chunk_index = chunk_index;
	record->reserved1 = UINT64_MAX;
	__atomic_store_n(&record->state, S3_WB_STATE_PREPARING, __ATOMIC_RELEASE);

	/* Publish the reservation first. Recovery truncates at PREPARING if this
	 * process dies before the final release store below. */
	__atomic_store_n(&header->tail, position + record_len, __ATOMIC_RELEASE);
	if (payload_len != 0) {
		memcpy((uint8_t *)record + record->header_len, payload, payload_len);
	}
	record->crc32c = record_crc(record,
				   (uint8_t *)record + record->header_len);
	__atomic_store_n(&record->state, S3_WB_STATE_PENDING, __ATOMIC_RELEASE);
	__atomic_add_fetch(&header->publishes, 1, __ATOMIC_RELAXED);
	__atomic_add_fetch(&header->pending_records, 1, __ATOMIC_RELAXED);
	if (out_seq) {
		*out_seq = seq;
	}
	if (out_record) {
		export_record(record, out_record, position);
	}
	return 0;
}

static void
export_record(const struct s3_wb_record_header *record,
	      struct s3_writeback_record *out, uint64_t position)
{
	uint8_t state;
	int32_t tag;

	memset(out, 0, sizeof(*out));
	out->position = position;
	out->seq = record->seq;
	state = __atomic_load_n(&record->state, __ATOMIC_ACQUIRE);
	tag = __atomic_load_n(&record->outcome, __ATOMIC_ACQUIRE);
	if (state == S3_WB_STATE_PENDING && tag != 0) {
		uint64_t wal_seq =
			__atomic_load_n(&record->reserved0, __ATOMIC_RELAXED);
		uint64_t complement =
			__atomic_load_n(&record->reserved1, __ATOMIC_RELAXED);

		if (complement == ~wal_seq && tag == wal_seq_tag(wal_seq)) {
			out->wal_seq = wal_seq;
		}
	}
	out->lba = record->lba;
	out->chunk_index = record->chunk_index;
	out->nblocks = record->nblocks;
	out->payload_len = record->payload_len;
	out->crc32c = record->crc32c;
	out->outcome = state == S3_WB_STATE_COMPLETE ? record->outcome : 0;
	out->type = (enum s3_writeback_record_type)record->type;
	out->payload = (const uint8_t *)record + record->header_len;
	out->completed = state == S3_WB_STATE_COMPLETE;
}

void
s3_writeback_iter_begin(const struct s3_writeback *writeback,
			 struct s3_writeback_iter *iter)
{
	if (writeback && iter) {
		iter->position = __atomic_load_n(&writeback->header->head,
						 __ATOMIC_ACQUIRE);
	}
}

int
s3_writeback_iter_next(const struct s3_writeback *writeback,
		       struct s3_writeback_iter *iter,
		       struct s3_writeback_record *out)
{
	struct s3_wb_record_header *record;
	uint64_t tail, advance;
	int rc;

	if (!writeback || !iter || !out) {
		return -EINVAL;
	}
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	while (iter->position < tail) {
		rc = record_at(writeback, iter->position, &record, &advance);
		if (rc != 0 || advance == 0 || advance > tail - iter->position) {
			return rc != 0 ? rc : -EBADMSG;
		}
		iter->position += advance;
		if (!record || record->type == S3_WB_PAD_TYPE ||
		    __atomic_load_n(&record->state, __ATOMIC_ACQUIRE) ==
		    S3_WB_STATE_COMPLETE) {
			continue;
		}
		export_record(record, out, iter->position - advance);
		return 1;
	}
	return 0;
}

int
s3_writeback_set_wal_seq(struct s3_writeback *writeback,
			 const struct s3_writeback_record *ref,
			 uint64_t wal_seq)
{
	struct s3_wb_record_header *record;
	uint64_t head, tail, advance;
	int rc;

	if (!writeback || !ref || ref->seq == 0 || wal_seq == 0) {
		return -EINVAL;
	}
	head = __atomic_load_n(&writeback->header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	if (ref->position < head || ref->position >= tail) {
		return -ENOENT;
	}
	rc = record_at_trusted(writeback, ref->position, &record, &advance);
	if (rc != 0) {
		return rc;
	}
	if (!record || record->type == S3_WB_PAD_TYPE ||
	    record->seq != ref->seq) {
		return -ENOENT;
	}
	__atomic_store_n(&record->outcome, 0, __ATOMIC_RELEASE);
	__atomic_store_n(&record->reserved1, ~wal_seq, __ATOMIC_RELAXED);
	__atomic_store_n(&record->reserved0, wal_seq, __ATOMIC_RELAXED);
	__atomic_store_n(&record->outcome, wal_seq_tag(wal_seq),
			 __ATOMIC_RELEASE);
	return 0;
}

int
s3_writeback_mark_record_complete(struct s3_writeback *writeback,
				  const struct s3_writeback_record *ref,
				  int32_t outcome)
{
	struct s3_wb_record_header *record;
	uint64_t head, tail, advance;
	int rc;

	if (!writeback || !ref || ref->seq == 0) {
		return -EINVAL;
	}
	head = __atomic_load_n(&writeback->header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	if (ref->position < head || ref->position >= tail) {
		return -ENOENT;
	}
	rc = record_at_trusted(writeback, ref->position, &record, &advance);
	if (rc != 0) {
		return rc;
	}
	if (!record || record->type == S3_WB_PAD_TYPE ||
	    record->seq != ref->seq) {
		return -ENOENT;
	}
	if (__atomic_load_n(&record->state, __ATOMIC_ACQUIRE) ==
	    S3_WB_STATE_COMPLETE) {
		return -EALREADY;
	}
	/* Publish COMPLETE before reusing outcome (which is the WAL assignment
	 * commit tag while PENDING). A crash in the opposite order would expose a
	 * durable record as unbound and let recovery assign it a newer WAL seq. */
	__atomic_store_n(&record->state, S3_WB_STATE_COMPLETE,
			 __ATOMIC_RELEASE);
	record->outcome = outcome;
	__atomic_sub_fetch(&writeback->header->pending_records, 1,
			   __ATOMIC_RELAXED);
	__atomic_add_fetch(&writeback->header->completed_records, 1,
			   __ATOMIC_RELAXED);
	return 0;
}

uint64_t
s3_writeback_reclaim(struct s3_writeback *writeback)
{
	struct s3_wb_record_header *record;
	uint64_t head, tail, advance, reclaimed = 0;
	int rc;

	if (!writeback) {
		return 0;
	}
	head = __atomic_load_n(&writeback->header->head, __ATOMIC_RELAXED);
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	while (head < tail) {
		rc = record_at_trusted(writeback, head, &record, &advance);
		if (rc != 0 || advance == 0 || advance > tail - head) {
			break;
		}
		if (record && record->type != S3_WB_PAD_TYPE &&
		    __atomic_load_n(&record->state, __ATOMIC_ACQUIRE) !=
		    S3_WB_STATE_COMPLETE) {
			break;
		}
		if (record && record->type != S3_WB_PAD_TYPE) {
			reclaimed++;
			__atomic_sub_fetch(&writeback->header->completed_records, 1,
					   __ATOMIC_RELAXED);
		}
		head += advance;
	}
	__atomic_store_n(&writeback->header->head, head, __ATOMIC_RELEASE);
	return reclaimed;
}

int
s3_writeback_get_stats(const struct s3_writeback *writeback,
		       struct s3_writeback_stats *stats)
{
	uint64_t head, tail;

	if (!writeback || !stats) {
		return -EINVAL;
	}
	memset(stats, 0, sizeof(*stats));
	head = __atomic_load_n(&writeback->header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	if (head > tail || tail - head > writeback->header->capacity) {
		return -EBADMSG;
	}
	stats->capacity_bytes = writeback->header->capacity;
	stats->occupied_bytes = tail - head;
	stats->free_bytes = stats->capacity_bytes - stats->occupied_bytes;
	stats->publishes = __atomic_load_n(&writeback->header->publishes,
					   __ATOMIC_RELAXED);
	stats->full_events = __atomic_load_n(&writeback->header->full_events,
					     __ATOMIC_RELAXED);
	stats->pending_records =
		__atomic_load_n(&writeback->header->pending_records,
				__ATOMIC_RELAXED);
	stats->completed_records =
		__atomic_load_n(&writeback->header->completed_records,
				__ATOMIC_RELAXED);
	stats->next_seq = __atomic_load_n(&writeback->header->next_seq,
					  __ATOMIC_RELAXED);
	pthread_once(&g_budget_once, writeback_budget_init);
	pthread_mutex_lock(&g_budget_lock);
	stats->global_reserved_bytes = g_budget_used;
	stats->global_limit_bytes = g_budget_limit;
	stats->global_active_rings = g_budget_active;
	pthread_mutex_unlock(&g_budget_lock);
	return 0;
}

bool
s3_writeback_should_force_flush(const struct s3_writeback *writeback)
{
	uint64_t head, tail, capacity;

	if (!writeback) {
		return false;
	}
	head = __atomic_load_n(&writeback->header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	capacity = writeback->header->capacity;
	if (head >= tail || capacity == 0) {
		return false;
	}
	return tail - head >= capacity / 2;
}

int
s3_writeback_collect_head_chunks(const struct s3_writeback *writeback,
				 uint64_t *chunks, uint32_t max_chunks,
				 uint32_t max_records, uint32_t *out_count)
{
	struct s3_wb_record_header *record;
	uint64_t position, tail, advance;
	uint32_t count = 0, scanned = 0;
	int rc;

	if (!writeback || !chunks || max_chunks == 0 || max_records == 0 ||
	    !out_count) {
		return -EINVAL;
	}
	*out_count = 0;
	position = __atomic_load_n(&writeback->header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&writeback->header->tail, __ATOMIC_ACQUIRE);
	while (position < tail && count < max_chunks && scanned < max_records) {
		bool duplicate = false;

		rc = record_at_trusted(writeback, position, &record, &advance);
		if (rc != 0 || advance == 0 || advance > tail - position) {
			return rc != 0 ? rc : -EBADMSG;
		}
		position += advance;
		scanned++;
		if (!record || record->type == S3_WB_PAD_TYPE ||
		    __atomic_load_n(&record->state, __ATOMIC_ACQUIRE) ==
			    S3_WB_STATE_COMPLETE) {
			continue;
		}
		for (uint32_t i = 0; i < count; i++) {
			if (chunks[i] == record->chunk_index) {
				duplicate = true;
				break;
			}
		}
		if (!duplicate) {
			chunks[count++] = record->chunk_index;
		}
	}
	*out_count = count;
	return 0;
}

int
s3_writeback_bind_instance(struct s3_writeback *writeback,
			   const uint8_t *instance_id)
{
	if (!writeback || !instance_id || instance_id_is_zero(instance_id)) {
		return -EINVAL;
	}
	if (memcmp(writeback->header->instance_id, instance_id,
		   S3_WRITEBACK_INSTANCE_ID_SIZE) == 0) {
		return 0;
	}
	if (!instance_id_is_zero(writeback->header->instance_id)) {
		return -EEXIST;
	}
	memcpy(writeback->header->instance_id, instance_id,
	       S3_WRITEBACK_INSTANCE_ID_SIZE);
	__atomic_thread_fence(__ATOMIC_RELEASE);
	return 0;
}

static int
writeback_unlink(const char *lvstore_name, const uint8_t *instance_id,
		 bool require_empty)
{
	struct s3_wb_header *header = MAP_FAILED;
	struct stat st, path_st;
	char shm_name[S3_WRITEBACK_SHM_NAME_MAX];
	uint64_t head, tail;
	int dirfd = -1, fd = -1;
	int rc = 0;

	if (!lvstore_name) {
		return -EINVAL;
	}
	rc = s3_writeback_shm_name(lvstore_name, shm_name, sizeof(shm_name));
	if (rc != 0) {
		return rc;
	}
	dirfd = writeback_open_private_dir();
	if (dirfd < 0) {
		return dirfd;
	}
	fd = openat(dirfd, shm_name + 1, O_RDWR | O_CLOEXEC | O_NOFOLLOW);
	if (fd < 0) {
		rc = -errno;
		goto out;
	}
	if (flock(fd, LOCK_EX | LOCK_NB) != 0) {
		rc = errno == EWOULDBLOCK ? -EBUSY : -errno;
		goto out;
	}
	if (fstat(fd, &st) != 0) {
		rc = -errno;
		goto out;
	}
	if (!S_ISREG(st.st_mode) || st.st_uid != geteuid() ||
	    (st.st_mode & 0077) != 0 || st.st_nlink != 1 ||
	    st.st_size < S3_WB_HEADER_SIZE ||
	    (uint64_t)st.st_size > SIZE_MAX) {
		rc = -EACCES;
		goto out;
	}
	header = mmap(NULL, S3_WB_HEADER_SIZE, PROT_READ, MAP_SHARED, fd, 0);
	if (header == MAP_FAILED) {
		rc = -errno;
		goto out;
	}
	rc = validate_header(header, (size_t)st.st_size);
	if (rc != 0) {
		goto out;
	}
	if (instance_id &&
	    memcmp(instance_id, header->instance_id,
		   S3_WRITEBACK_INSTANCE_ID_SIZE) != 0) {
		rc = -EXDEV;
		goto out;
	}
	head = __atomic_load_n(&header->head, __ATOMIC_ACQUIRE);
	tail = __atomic_load_n(&header->tail, __ATOMIC_ACQUIRE);
	if (require_empty && head != tail) {
		rc = -ENOTEMPTY;
		goto out;
	}
	/* The name must still refer to the inode we locked. Without this check,
	 * another cleanup could unlink it and a concurrent creator could install
	 * a new same-name ring before this unlinkat(). */
	if (fstatat(dirfd, shm_name + 1, &path_st, AT_SYMLINK_NOFOLLOW) != 0) {
		rc = -errno;
		goto out;
	}
	if (path_st.st_dev != st.st_dev || path_st.st_ino != st.st_ino) {
		rc = -ESTALE;
		goto out;
	}
	if (unlinkat(dirfd, shm_name + 1, 0) != 0) {
		rc = -errno;
	}

out:
	if (header != MAP_FAILED) {
		munmap(header, S3_WB_HEADER_SIZE);
	}
	if (fd >= 0) {
		close(fd);
	}
	if (dirfd >= 0) {
		close(dirfd);
	}
	return rc;
}

int
s3_writeback_cleanup(const char *lvstore_name)
{
	return writeback_unlink(lvstore_name, NULL, true);
}

int
s3_writeback_discard(const char *lvstore_name, const uint8_t *instance_id)
{
	if (!instance_id || instance_id_is_zero(instance_id)) {
		return -EINVAL;
	}
	return writeback_unlink(lvstore_name, instance_id, false);
}
