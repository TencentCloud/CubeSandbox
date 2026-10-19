/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * Offline process-shared write-back ring test. No SPDK, S3, or credentials.
 */

#include "s3lvol/s3_writeback.h"

#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define TEST_CAPACITY 1024u
#define MIB (1024u * 1024u)

static int g_pass;
static int g_fail;

static void
check_true(const char *what, bool ok)
{
	if (ok) {
		g_pass++;
		printf("\t[PASS] %s\n", what);
	} else {
		g_fail++;
		printf("\t[FAIL] %s\n", what);
	}
}

static void
fill_pattern(uint8_t *buf, size_t len, uint8_t seed)
{
	size_t i;

	for (i = 0; i < len; i++) {
		buf[i] = (uint8_t)(seed + i * 17u);
	}
}

static int
publish_initial(const char *lvstore, const uint8_t *payload,
		uint64_t *first_seq)
{
	struct s3_writeback_opts opts = {
		.lvstore_name = lvstore,
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	uint64_t seq;
	int rc;

	rc = s3_writeback_open(&opts, &writeback);
	if (rc != 0) {
		return rc;
	}
	rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE, 10, 2,
				  payload, 96, 7, first_seq, NULL);
	if (rc == 0) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_ZEROES, 20, 3,
					  NULL, 0, 8, &seq, NULL);
	}
	if (rc == 0) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_UNMAP, 30, 4,
					  NULL, 0, 9, &seq, NULL);
	}
	/* Deliberately do not close: _exit() below simulates process death. */
	return rc;
}

static void
test_ring(void)
{
	struct s3_writeback_opts opts = {0};
	struct s3_writeback *writeback = NULL;
	struct s3_writeback *second = NULL;
	struct s3_writeback_iter iter;
	struct s3_writeback_record record;
	struct s3_writeback_stats stats;
	uint8_t initial[96], small[100], large[200];
	uint8_t instance_a[S3_WRITEBACK_INSTANCE_ID_SIZE] = {1};
	uint8_t instance_b[S3_WRITEBACK_INSTANCE_ID_SIZE] = {2};
	uint64_t initial_seq = 0, seqs[32], wrap_seq[4];
	uint64_t head_chunks[3];
	uint32_t head_count = 0;
	char lvstore[96];
	pid_t child;
	int status, rc, count;
	uint64_t reclaimed;

	snprintf(lvstore, sizeof(lvstore), "writeback-test-%ld", (long)getpid());
	fill_pattern(initial, sizeof(initial), 3);
	fill_pattern(small, sizeof(small), 19);
	fill_pattern(large, sizeof(large), 41);

	printf("\n[1] process crash, reopen, order, payload and CRC\n");
	child = fork();
	if (child == 0) {
		rc = publish_initial(lvstore, initial, &initial_seq);
		_exit(rc == 0 ? 0 : 100);
	}
	check_true("child process started", child > 0);
	status = -1;
	if (child > 0) {
		waitpid(child, &status, 0);
	}
	check_true("crashed owner published records",
		   WIFEXITED(status) && WEXITSTATUS(status) == 0);

	opts.lvstore_name = lvstore;
	opts.capacity_bytes = TEST_CAPACITY;
	opts.record_alignment = 64;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("stale owner PID is recovered", rc == 0);
	if (rc != 0) {
		return;
	}
	rc = s3_writeback_open(&opts, &second);
	check_true("live concurrent owner is rejected", rc == -EBUSY);

	s3_writeback_iter_begin(writeback, &iter);
	count = 0;
	while ((rc = s3_writeback_iter_next(writeback, &iter, &record)) == 1) {
		if (count == 0) {
			initial_seq = record.seq;
			check_true("WRITE metadata is preserved",
				   record.type == S3_WRITEBACK_WRITE &&
				   record.lba == 10 && record.nblocks == 2 &&
				   record.chunk_index == 7 &&
				   record.payload_len == sizeof(initial));
			check_true("WRITE payload is preserved",
				   memcmp(record.payload, initial,
					  sizeof(initial)) == 0);
			check_true("WRITE CRC is present", record.crc32c != 0);
		} else if (count == 1) {
			seqs[0] = record.seq;
			check_true("ZEROES follows WRITE",
				   record.type == S3_WRITEBACK_ZEROES &&
				   record.seq == initial_seq + 1);
		} else if (count == 2) {
			seqs[1] = record.seq;
			check_true("UNMAP follows ZEROES",
				   record.type == S3_WRITEBACK_UNMAP &&
				   record.seq == initial_seq + 2);
		}
		count++;
	}
	check_true("three pending records iterate in order", rc == 0 && count == 3);
	rc = s3_writeback_collect_head_chunks(writeback, head_chunks, 2, 2,
					      &head_count);
	check_true("head scheduling query preserves ring order",
		   rc == 0 && head_count == 2 &&
		   head_chunks[0] == 7 && head_chunks[1] == 8);
	rc = s3_writeback_get_record(writeback, initial_seq, &record);
	check_true("first record can be addressed by its stable handle", rc == 0);
	rc = s3_writeback_set_wal_seq(writeback, &record, 1234);
	check_true("assigned WAL identity is persisted", rc == 0);
	rc = s3_writeback_get_record(writeback, initial_seq, &record);
	check_true("WAL identity is visible before host acknowledgement",
		   rc == 0 && record.wal_seq == 1234);

	printf("\n[2] out-of-order completion reclaims only a prefix\n");
	rc = s3_writeback_get_record(writeback, seqs[0], &record);
	check_true("middle record has an O(1) completion handle", rc == 0);
	rc = s3_writeback_mark_record_complete(writeback, &record, -EIO);
	check_true("middle record stores its outcome", rc == 0);
	rc = s3_writeback_collect_head_chunks(writeback, head_chunks, 3, 3,
					      &head_count);
	check_true("head scheduling query skips completed records",
		   rc == 0 && head_count == 2 &&
		   head_chunks[0] == 7 && head_chunks[1] == 9);
	reclaimed = s3_writeback_reclaim(writeback);
	check_true("completed middle record is not reclaimed", reclaimed == 0);
	s3_writeback_close(writeback);
	writeback = NULL;

	rc = s3_writeback_open(&opts, &writeback);
	check_true("normal reopen succeeds", rc == 0);
	if (rc != 0) {
		return;
	}
	s3_writeback_iter_begin(writeback, &iter);
	count = 0;
	while ((rc = s3_writeback_iter_next(writeback, &iter, &record)) == 1) {
		if (record.seq == initial_seq) {
			check_true("WAL identity survives process reopen",
				   record.wal_seq == 1234);
		}
		seqs[count++] = record.seq;
	}
	check_true("reopen skips completed middle record",
		   rc == 0 && count == 2 && seqs[0] == initial_seq &&
		   seqs[1] == initial_seq + 2);
	check_true("complete first record",
		   s3_writeback_mark_complete(writeback, initial_seq, 0) == 0);
	check_true("contiguous first and middle records reclaim together",
		   s3_writeback_reclaim(writeback) == 2);
	check_true("complete final initial record",
		   s3_writeback_mark_complete(writeback, initial_seq + 2, 0) == 0);
	check_true("final initial record reclaims",
		   s3_writeback_reclaim(writeback) == 1);

	printf("\n[3] wrap padding remains ordered and reclaimable\n");
	for (count = 0; count < 3; count++) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE,
					  100 + count, 1, small, sizeof(small),
					  1000 + count / 2, &wrap_seq[count], NULL);
		check_true("small wrap setup record publishes", rc == 0);
	}
	rc = s3_writeback_collect_head_chunks(writeback, head_chunks, 3, 2,
					      &head_count);
	check_true("head scan stops at its record budget",
		   rc == 0 && head_count == 1 && head_chunks[0] == 1000);
	rc = s3_writeback_collect_head_chunks(writeback, head_chunks, 3, 3,
					      &head_count);
	check_true("a wider record budget reaches the next distinct chunk",
		   rc == 0 && head_count == 2 &&
		   head_chunks[0] == 1000 && head_chunks[1] == 1001);
	check_true("first wrap record completes",
		   s3_writeback_mark_complete(writeback, wrap_seq[0], 0) == 0);
	check_true("second wrap record completes",
		   s3_writeback_mark_complete(writeback, wrap_seq[1], 0) == 0);
	check_true("two-record prefix reclaims",
		   s3_writeback_reclaim(writeback) == 2);
	rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE, 200, 2,
				  large, sizeof(large), 2000, &wrap_seq[3], NULL);
	check_true("record publishes across physical wrap", rc == 0);
	s3_writeback_iter_begin(writeback, &iter);
	count = 0;
	while ((rc = s3_writeback_iter_next(writeback, &iter, &record)) == 1) {
		seqs[count++] = record.seq;
		if (record.seq == wrap_seq[3]) {
			check_true("wrapped payload is intact",
				   record.payload_len == sizeof(large) &&
				   memcmp(record.payload, large,
					  sizeof(large)) == 0);
		}
	}
	check_true("wrapped records retain sequence order",
		   rc == 0 && count == 2 && seqs[0] == wrap_seq[2] &&
		   seqs[1] == wrap_seq[3]);
	check_true("later wrapped record completes first",
		   s3_writeback_mark_complete(writeback, wrap_seq[3], 0) == 0);
	check_true("later completion cannot pass prefix",
		   s3_writeback_reclaim(writeback) == 0);
	check_true("older wrapped record completes",
		   s3_writeback_mark_complete(writeback, wrap_seq[2], 0) == 0);
	check_true("padding and both wrapped records reclaim",
		   s3_writeback_reclaim(writeback) == 2);

	printf("\n[4] capacity applies backpressure and reports occupancy\n");
	count = 0;
	while (count < (int)(sizeof(seqs) / sizeof(seqs[0]))) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE,
					  300 + count, 1, small, sizeof(small),
					  3000 + count, &seqs[count], NULL);
		if (rc != 0) {
			break;
		}
		count++;
	}
	check_true("full ring rejects another publish", rc == -ENOSPC && count > 0);
	rc = s3_writeback_get_stats(writeback, &stats);
	check_true("occupancy and full event are exposed",
		   rc == 0 && stats.occupied_bytes > 0 &&
		   stats.free_bytes < stats.capacity_bytes &&
		   stats.pending_records == (uint64_t)count &&
		   stats.full_events > 0);
	check_true("node budget reservation is exposed in O(1) stats",
		   stats.global_active_rings == 1 &&
		   stats.global_reserved_bytes >= stats.capacity_bytes &&
		   stats.global_limit_bytes >= stats.global_reserved_bytes);
	check_true("occupied ring is visible through O(1) stats",
		   stats.occupied_bytes > 0);
	for (int i = count - 1; i >= 0; i--) {
		check_true("backpressure record completes",
			   s3_writeback_mark_complete(writeback, seqs[i], 0) == 0);
	}
	check_true("all backpressure records reclaim contiguously",
		   s3_writeback_reclaim(writeback) == (uint64_t)count);
	rc = s3_writeback_get_stats(writeback, &stats);
	check_true("empty ring reports all bytes free",
		   rc == 0 && stats.occupied_bytes == 0 &&
		   stats.free_bytes == TEST_CAPACITY);
	check_true("empty ring is visible through O(1) stats",
		   stats.occupied_bytes == 0);
	check_true("empty ring binds to its lvstore instance",
		   s3_writeback_bind_instance(writeback, instance_a) == 0);

	printf("\n[5] close preserves shm; explicit clean+empty removes it\n");
	s3_writeback_close(writeback);
	writeback = NULL;
	opts.instance_id = instance_b;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("different lvstore instance is rejected", rc == -EXDEV);
	opts.instance_id = instance_a;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("matching lvstore instance reopens", rc == 0);
	s3_writeback_close(writeback);
	writeback = NULL;
	opts.instance_id = NULL;
	check_true("explicit cleanup succeeds", s3_writeback_cleanup(lvstore) == 0);
	rc = s3_writeback_open(&opts, &writeback);
	check_true("cleaned object is absent", rc == -ENOENT);
}

static void
test_corrupt_header(void)
{
	struct s3_writeback_opts opts = {
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	char lvstore[96], shm_name[S3_WRITEBACK_SHM_NAME_MAX], shm_path[192];
	uint64_t *magic;
	void *mapping;
	int fd, rc;

	printf("\n[6] corrupt headers are rejected\n");
	snprintf(lvstore, sizeof(lvstore), "writeback-corrupt-%ld",
		 (long)getpid());
	opts.lvstore_name = lvstore;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("corruption fixture creates", rc == 0);
	if (rc != 0) {
		return;
	}
	s3_writeback_close(writeback);
	writeback = NULL;
	check_true("deterministic shm name is available",
		   s3_writeback_shm_name(lvstore, shm_name, sizeof(shm_name)) == 0);
	snprintf(shm_path, sizeof(shm_path), "/dev/shm/s3lvol-wb-%lu/%s",
		 (unsigned long)geteuid(), shm_name + 1);
	fd = open(shm_path, O_RDWR | O_CLOEXEC | O_NOFOLLOW);
	check_true("fixture shm opens directly", fd >= 0);
	if (fd < 0) {
		return;
	}
	mapping = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	check_true("fixture header maps", mapping != MAP_FAILED);
	if (mapping != MAP_FAILED) {
		magic = mapping;
		*magic ^= UINT64_C(0x100);
		munmap(mapping, 4096);
	}
	close(fd);
	opts.flags = 0;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("corrupt header is rejected", rc == -EBADMSG);
	unlink(shm_path);
}

static void
test_interrupted_init(void)
{
	struct s3_writeback_opts opts = {
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	char lvstore[96], shm_name[S3_WRITEBACK_SHM_NAME_MAX], path[192];
	int fd, rc;

	printf("\n[7] an interrupted INIT is safely rebuilt\n");
	snprintf(lvstore, sizeof(lvstore), "writeback-init-%ld", (long)getpid());
	opts.lvstore_name = lvstore;
	rc = s3_writeback_shm_name(lvstore, shm_name, sizeof(shm_name));
	snprintf(path, sizeof(path), "/dev/shm/s3lvol-wb-%lu/%s",
		 (unsigned long)geteuid(), shm_name + 1);
	fd = open(path, O_RDWR | O_CREAT | O_EXCL | O_CLOEXEC | O_NOFOLLOW,
		  0600);
	check_true("zeroed INIT fixture is created", rc == 0 && fd >= 0);
	if (fd < 0) {
		return;
	}
	rc = ftruncate(fd, 4096 + TEST_CAPACITY);
	close(fd);
	check_true("INIT fixture has its final allocation size", rc == 0);
	rc = s3_writeback_open(&opts, &writeback);
	check_true("exclusive owner rebuilds the interrupted header", rc == 0);
	s3_writeback_close(writeback);
	check_true("rebuilt empty ring cleans up",
		   s3_writeback_cleanup(lvstore) == 0);
}

static void
test_corrupt_recovery_state(void)
{
	struct s3_writeback_opts opts = {
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	char lvstore[96], shm_name[S3_WRITEBACK_SHM_NAME_MAX], path[192];
	uint8_t payload[64] = {0};
	uint8_t bad_state = 2; /* old unpaired encoding: must not be accepted */
	uint64_t seq;
	int fd, rc;

	printf("\n[8] recovery decision fields reject corruption\n");
	snprintf(lvstore, sizeof(lvstore), "writeback-state-%ld", (long)getpid());
	opts.lvstore_name = lvstore;
	rc = s3_writeback_open(&opts, &writeback);
	if (rc == 0) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE, 0, 1,
					  payload, sizeof(payload), 0, &seq, NULL);
	}
	check_true("state-corruption fixture publishes", rc == 0);
	s3_writeback_close(writeback);
	s3_writeback_shm_name(lvstore, shm_name, sizeof(shm_name));
	snprintf(path, sizeof(path), "/dev/shm/s3lvol-wb-%lu/%s",
		 (unsigned long)geteuid(), shm_name + 1);
	fd = open(path, O_RDWR | O_CLOEXEC | O_NOFOLLOW);
	rc = fd < 0 ? -errno :
	     (pwrite(fd, &bad_state, sizeof(bad_state), 4096 + 7) ==
	      (ssize_t)sizeof(bad_state) ? 0 : -EIO);
	if (fd >= 0) {
		close(fd);
	}
	check_true("record state byte is corrupted", rc == 0);
	opts.flags = 0;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("unpaired state is rejected during recovery", rc == -EBADMSG);
	unlink(path);
}

static void
test_torn_wal_assignment(void)
{
	struct s3_writeback_opts opts = {
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	struct s3_writeback_record record;
	struct s3_writeback_iter iter;
	char lvstore[96], name[S3_WRITEBACK_SHM_NAME_MAX], path[192];
	uint8_t payload[64] = {0};
	uint64_t seq, complement = ~UINT64_C(456);
	int32_t invalid_tag = 0;
	int fd, rc;

	printf("\n[9] torn WAL assignment is conservatively retried\n");
	snprintf(lvstore, sizeof(lvstore), "writeback-torn-%ld", (long)getpid());
	opts.lvstore_name = lvstore;
	rc = s3_writeback_open(&opts, &writeback);
	if (rc == 0) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE, 0, 1,
					  payload, sizeof(payload), 0, &seq, &record);
	}
	if (rc == 0) {
		rc = s3_writeback_set_wal_seq(writeback, &record, 123);
	}
	check_true("valid WAL assignment fixture is created", rc == 0);
	s3_writeback_close(writeback);
	s3_writeback_shm_name(lvstore, name, sizeof(name));
	snprintf(path, sizeof(path), "/dev/shm/s3lvol-wb-%lu/%s",
		 (unsigned long)geteuid(), name + 1);
	fd = open(path, O_RDWR | O_CLOEXEC | O_NOFOLLOW);
	if (fd >= 0) {
		/* outcome is the assignment commit tag at +40; reserved1 is the
		 * complement at +56. This models a crash after invalidation and
		 * the first field update but before the final commit tag. */
		rc = pwrite(fd, &invalid_tag, sizeof(invalid_tag), 4096 + 40) ==
			     (ssize_t)sizeof(invalid_tag) &&
		     pwrite(fd, &complement, sizeof(complement), 4096 + 56) ==
			     (ssize_t)sizeof(complement) ? 0 : -EIO;
		close(fd);
	} else {
		rc = -errno;
	}
	check_true("WAL assignment is cut before its commit tag", rc == 0);
	opts.flags = 0;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("torn assignment does not corrupt the ring", rc == 0);
	s3_writeback_iter_begin(writeback, &iter);
	rc = s3_writeback_iter_next(writeback, &iter, &record);
	check_true("torn assignment is exposed as unbound for re-append",
		   rc == 1 && record.wal_seq == 0);
	s3_writeback_mark_record_complete(writeback, &record, 0);
	s3_writeback_reclaim(writeback);
	s3_writeback_close(writeback);
	check_true("torn-assignment fixture cleans up",
		   s3_writeback_cleanup(lvstore) == 0);
}

static void
test_legacy_migration(void)
{
	struct s3_writeback_opts opts = {
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	char lvstore[96], name[S3_WRITEBACK_SHM_NAME_MAX];
	char private_path[192], legacy_path[128];
	int rc;

	printf("\n[10] legacy root-level shm migrates without data loss\n");
	snprintf(lvstore, sizeof(lvstore), "writeback-legacy-%ld",
		 (long)getpid());
	opts.lvstore_name = lvstore;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("migration fixture creates", rc == 0);
	s3_writeback_close(writeback);
	s3_writeback_shm_name(lvstore, name, sizeof(name));
	snprintf(private_path, sizeof(private_path),
		 "/dev/shm/s3lvol-wb-%lu/%s",
		 (unsigned long)geteuid(), name + 1);
	snprintf(legacy_path, sizeof(legacy_path), "/dev/shm/%s", name + 1);
	rc = rename(private_path, legacy_path);
	check_true("fixture is moved to the legacy location", rc == 0);
	rc = s3_writeback_open(&opts, &writeback);
	check_true("legacy object is attached and migrated", rc == 0);
	check_true("migration used the private directory",
		   access(private_path, F_OK) == 0 &&
		   access(legacy_path, F_OK) != 0);
	s3_writeback_close(writeback);
	check_true("migrated fixture cleans up",
		   s3_writeback_cleanup(lvstore) == 0);
}

static void
test_permanent_discard(void)
{
	struct s3_writeback_opts opts = {
		.capacity_bytes = TEST_CAPACITY,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *writeback = NULL;
	uint8_t payload[64] = {0};
	uint8_t instance_a[S3_WRITEBACK_INSTANCE_ID_SIZE] = {1};
	uint8_t instance_b[S3_WRITEBACK_INSTANCE_ID_SIZE] = {2};
	char lvstore[96];
	uint64_t seq;
	int rc;

	printf("\n[11] permanent lvstore deletion discards recovery data\n");
	snprintf(lvstore, sizeof(lvstore), "writeback-discard-%ld",
		 (long)getpid());
	opts.lvstore_name = lvstore;
	opts.instance_id = instance_a;
	rc = s3_writeback_open(&opts, &writeback);
	if (rc == 0) {
		rc = s3_writeback_publish(writeback, S3_WRITEBACK_WRITE, 0, 1,
					  payload, sizeof(payload), 0, &seq, NULL);
	}
	check_true("discard fixture contains a pending record", rc == 0);
	check_true("discard refuses a live owner",
		   s3_writeback_discard(lvstore, instance_a) == -EBUSY);
	s3_writeback_close(writeback);
	writeback = NULL;
	check_true("ordinary cleanup preserves recovery data",
		   s3_writeback_cleanup(lvstore) == -ENOTEMPTY);
	check_true("discard rejects a newer same-name lvstore",
		   s3_writeback_discard(lvstore, instance_b) == -EXDEV);
	check_true("permanent discard removes the non-empty ring",
		   s3_writeback_discard(lvstore, instance_a) == 0);
	opts.flags = 0;
	rc = s3_writeback_open(&opts, &writeback);
	check_true("discarded ring is absent", rc == -ENOENT);
}

static void
test_discard_ignores_runtime_budget(void)
{
	struct s3_writeback_opts stale_opts = {
		.capacity_bytes = 8u * MIB,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback_opts holder_opts = {
		.capacity_bytes = 60u * MIB,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	struct s3_writeback *stale = NULL, *holder = NULL;
	uint8_t instance[S3_WRITEBACK_INSTANCE_ID_SIZE] = {3};
	char stale_name[96], holder_name[96], leaf[S3_WRITEBACK_SHM_NAME_MAX];
	char path[192];
	int fd = -1, rc;

	printf("\n[12] discard does not consume the runtime ring budget\n");
	snprintf(stale_name, sizeof(stale_name), "writeback-budget-stale-%ld",
		 (long)getpid());
	snprintf(holder_name, sizeof(holder_name), "writeback-budget-holder-%ld",
		 (long)getpid());
	stale_opts.lvstore_name = stale_name;
	stale_opts.instance_id = instance;
	holder_opts.lvstore_name = holder_name;

	rc = s3_writeback_open(&stale_opts, &stale);
	check_true("oversized stale fixture opens before the budget is occupied",
		   rc == 0);
	if (rc != 0) {
		return;
	}
	s3_writeback_close(stale);
	stale = NULL;

	/* Keep the valid header and logical capacity while releasing the tmpfs
	 * data pages, so this regression test also fits a 64 MiB CI /dev/shm. */
	rc = s3_writeback_shm_name(stale_name, leaf, sizeof(leaf));
	if (rc == 0) {
		snprintf(path, sizeof(path), "/dev/shm/s3lvol-wb-%lu%s",
			 (unsigned long)geteuid(), leaf);
		fd = open(path, O_RDWR | O_CLOEXEC);
		rc = fd < 0 ? -errno : 0;
	}
	if (rc == 0 && ftruncate(fd, 4096) != 0) {
		rc = -errno;
	}
	if (rc == 0 &&
	    ftruncate(fd, 4096 + (off_t)stale_opts.capacity_bytes) != 0) {
		rc = -errno;
	}
	if (fd >= 0) {
		close(fd);
	}
	check_true("stale fixture is sparse but keeps its declared capacity",
		   rc == 0);

	rc = s3_writeback_open(&holder_opts, &holder);
	check_true("another ring occupies 60 of the 64 MiB budget", rc == 0);
	if (rc == 0) {
		check_true("discard succeeds although reopening would exceed budget",
			   s3_writeback_discard(stale_name, instance) == 0);
		s3_writeback_close(holder);
		holder = NULL;
		check_true("budget holder cleans up",
			   s3_writeback_cleanup(holder_name) == 0);
	} else {
		(void)s3_writeback_discard(stale_name, instance);
	}
}

int
main(void)
{
	printf("=== s3lvol process-shared write-back ring test ===\n");
	/* Must precede the first open: the production budget is initialized once. */
	setenv("S3LVOL_WRITEBACK_TOTAL_MB", "64", 1);
	test_ring();
	test_corrupt_header();
	test_interrupted_init();
	test_corrupt_recovery_state();
	test_torn_wal_assignment();
	test_legacy_migration();
	test_permanent_discard();
	test_discard_ignores_runtime_budget();
	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
