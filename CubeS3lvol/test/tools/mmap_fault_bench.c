/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Fault a block-backed MAP_PRIVATE mapping the same way the hypervisor maps a
 * restored memory image.  This is a dataplane benchmark helper, not a unit
 * test: it emits one JSON object so a shell driver can compare builds.
 */

#define _GNU_SOURCE

#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <inttypes.h>
#include <linux/fs.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#define DEFAULT_CHUNK_SIZE (1024U * 1024U)

enum access_pattern {
	PATTERN_SEQUENTIAL,
	PATTERN_RANDOM,
	PATTERN_STAMPEDE,
};

struct bench;

struct worker {
	struct bench *bench;
	unsigned int id;
	pthread_t thread;
	uint64_t latency_offset;
	uint64_t checksum;
	uint64_t samples;
	int error;
};

struct bench {
	uint8_t *mapping;
	uint64_t map_offset;
	uint64_t length;
	uint64_t npages;
	uint64_t *pages;
	uint64_t *latencies;
	uint64_t latency_capacity;
	unsigned int threads;
	unsigned int write_percent;
	uint32_t page_size;
	uint32_t chunk_size;
	enum access_pattern pattern;
	pthread_barrier_t start;
	pthread_barrier_t cohort;
};

static uint64_t
now_ns(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC_RAW, &ts);
	return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

static uint64_t
xorshift64(uint64_t *state)
{
	uint64_t x = *state;

	x ^= x << 13;
	x ^= x >> 7;
	x ^= x << 17;
	*state = x;
	return x;
}

static void
shuffle_pages(uint64_t *pages, uint64_t count, uint64_t seed)
{
	uint64_t i;

	if (seed == 0) {
		seed = 1;
	}
	for (i = count; i > 1; i--) {
		uint64_t j = xorshift64(&seed) % i;
		uint64_t tmp = pages[i - 1];

		pages[i - 1] = pages[j];
		pages[j] = tmp;
	}
}

static void
touch_page(struct worker *worker, uint64_t page)
{
	struct bench *bench = worker->bench;
	volatile uint8_t *p = bench->mapping + page * bench->page_size;
	uint64_t begin, elapsed, sample;
	uint8_t value;

	begin = now_ns();
	value = *p;
	if (bench->write_percent != 0 &&
	    page % 100 < bench->write_percent) {
		/* Writing the byte already present still causes a MAP_PRIVATE CoW
		 * fault without changing the block device's contents. */
		*p = value;
	}
	elapsed = now_ns() - begin;

	sample = worker->latency_offset + worker->samples;
	bench->latencies[sample] = elapsed;
	worker->checksum += value;
	worker->samples++;
}

static void *
worker_main(void *arg)
{
	struct worker *worker = arg;
	struct bench *bench = worker->bench;
	uint64_t i;
	int rc;

	rc = pthread_barrier_wait(&bench->start);
	if (rc != 0 && rc != PTHREAD_BARRIER_SERIAL_THREAD) {
		worker->error = rc;
		return NULL;
	}

	if (bench->pattern == PATTERN_STAMPEDE) {
		uint64_t pages_per_chunk = bench->chunk_size / bench->page_size;
		uint64_t nchunks = bench->length / bench->chunk_size;

		for (i = 0; i < nchunks; i++) {
			uint64_t page = i * pages_per_chunk +
					(worker->id % pages_per_chunk);

			rc = pthread_barrier_wait(&bench->cohort);
			if (rc != 0 && rc != PTHREAD_BARRIER_SERIAL_THREAD) {
				worker->error = rc;
				return NULL;
			}
			touch_page(worker, page);
			rc = pthread_barrier_wait(&bench->cohort);
			if (rc != 0 && rc != PTHREAD_BARRIER_SERIAL_THREAD) {
				worker->error = rc;
				return NULL;
			}
		}
		return NULL;
	}

	for (i = bench->npages * worker->id / bench->threads;
	     i < bench->npages * (worker->id + 1) / bench->threads; i++) {
		touch_page(worker, bench->pages[i]);
	}
	return NULL;
}

static int
compare_u64(const void *a, const void *b)
{
	uint64_t lhs = *(const uint64_t *)a;
	uint64_t rhs = *(const uint64_t *)b;

	return (lhs > rhs) - (lhs < rhs);
}

static uint64_t
percentile(const uint64_t *values, uint64_t count, unsigned int pct)
{
	uint64_t index;

	if (count == 0) {
		return 0;
	}
	index = ((count - 1) * pct + 99) / 100;
	return values[index];
}

static const char *
pattern_name(enum access_pattern pattern)
{
	switch (pattern) {
	case PATTERN_SEQUENTIAL:
		return "sequential";
	case PATTERN_RANDOM:
		return "random";
	case PATTERN_STAMPEDE:
		return "stampede";
	}
	return "unknown";
}

static void
usage(const char *prog)
{
	fprintf(stderr,
		"Usage: %s --device PATH [options]\n"
		"  --offset-mib N     mapping offset (default: 0)\n"
		"  --size-mib N       mapped bytes to touch (default: device size)\n"
		"  --threads N        faulting threads (default: 1)\n"
		"  --pattern NAME     sequential, random, or stampede (default: sequential)\n"
		"  --chunk-kib N      stampede boundary (default: 1024)\n"
		"  --write-percent N  MAP_PRIVATE CoW percentage, 0..100 (default: 0)\n"
		"  --seed N           random permutation seed (default: 1)\n",
		prog);
}

int
main(int argc, char **argv)
{
	static const struct option options[] = {
		{"device", required_argument, NULL, 'd'},
		{"offset-mib", required_argument, NULL, 'o'},
		{"size-mib", required_argument, NULL, 's'},
		{"threads", required_argument, NULL, 't'},
		{"pattern", required_argument, NULL, 'p'},
		{"chunk-kib", required_argument, NULL, 'c'},
		{"write-percent", required_argument, NULL, 'w'},
		{"seed", required_argument, NULL, 'r'},
		{"help", no_argument, NULL, 'h'},
		{NULL, 0, NULL, 0},
	};
	struct rusage before, after;
	struct stat st;
	struct bench bench = {};
	struct worker *workers = NULL;
	const char *device = NULL;
	uint64_t requested = 0, device_size = 0, seed = 1;
	uint64_t total_checksum = 0, total_samples = 0;
	uint64_t begin, elapsed;
	long parsed;
	int fd = -1, opt, rc = EXIT_FAILURE;
	unsigned int i, created = 0;

	bench.threads = 1;
	bench.chunk_size = DEFAULT_CHUNK_SIZE;
	bench.pattern = PATTERN_SEQUENTIAL;

	while ((opt = getopt_long(argc, argv, "d:o:s:t:p:c:w:r:h", options,
				  NULL)) != -1) {
		switch (opt) {
		case 'd':
			device = optarg;
			break;
		case 'o':
			bench.map_offset =
				strtoull(optarg, NULL, 10) * 1024ULL * 1024ULL;
			break;
		case 's':
			requested = strtoull(optarg, NULL, 10) * 1024ULL * 1024ULL;
			break;
		case 't':
			parsed = strtol(optarg, NULL, 10);
			if (parsed < 1 || parsed > 256) {
				fprintf(stderr, "threads must be in [1, 256]\n");
				goto out;
			}
			bench.threads = (unsigned int)parsed;
			break;
		case 'p':
			if (strcmp(optarg, "sequential") == 0) {
				bench.pattern = PATTERN_SEQUENTIAL;
			} else if (strcmp(optarg, "random") == 0) {
				bench.pattern = PATTERN_RANDOM;
			} else if (strcmp(optarg, "stampede") == 0 ||
				   strcmp(optarg, "cohort") == 0) {
				bench.pattern = PATTERN_STAMPEDE;
			} else {
				fprintf(stderr, "unknown pattern: %s\n", optarg);
				goto out;
			}
			break;
		case 'c':
			bench.chunk_size = (uint32_t)strtoul(optarg, NULL, 10) * 1024U;
			break;
		case 'w':
			parsed = strtol(optarg, NULL, 10);
			if (parsed < 0 || parsed > 100) {
				fprintf(stderr, "write-percent must be in [0, 100]\n");
				goto out;
			}
			bench.write_percent = (unsigned int)parsed;
			break;
		case 'r':
			seed = strtoull(optarg, NULL, 10);
			break;
		case 'h':
			usage(argv[0]);
			return EXIT_SUCCESS;
		default:
			usage(argv[0]);
			goto out;
		}
	}
	if (!device) {
		usage(argv[0]);
		goto out;
	}

	fd = open(device, O_RDONLY | O_CLOEXEC);
	if (fd < 0) {
		fprintf(stderr, "open %s: %s\n", device, strerror(errno));
		goto out;
	}
	if (ioctl(fd, BLKGETSIZE64, &device_size) != 0) {
		if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode)) {
			fprintf(stderr, "size of %s: %s\n", device, strerror(errno));
			goto out;
		}
		device_size = (uint64_t)st.st_size;
	}

	bench.page_size = (uint32_t)sysconf(_SC_PAGESIZE);
	if (bench.map_offset >= device_size) {
		fprintf(stderr, "offset %" PRIu64
			" is outside device size %" PRIu64 "\n",
			bench.map_offset, device_size);
		goto out;
	}
	bench.length = requested ? requested : device_size - bench.map_offset;
	bench.length -= bench.length % bench.page_size;
	if (bench.map_offset > device_size || bench.length == 0 ||
	    bench.length > device_size - bench.map_offset ||
	    bench.map_offset % bench.page_size != 0) {
		fprintf(stderr, "offset %" PRIu64 " and length %" PRIu64
			" are outside/aligned differently from device size %" PRIu64 "\n",
			bench.map_offset, bench.length, device_size);
		goto out;
	}
	if (bench.chunk_size < bench.page_size ||
	    bench.chunk_size % bench.page_size != 0) {
		fprintf(stderr, "chunk size must be page aligned and at least one page\n");
		goto out;
	}
	if (bench.pattern == PATTERN_STAMPEDE &&
	    bench.length % bench.chunk_size != 0) {
		fprintf(stderr, "stampede length must be a multiple of chunk size\n");
		goto out;
	}
	if (bench.pattern == PATTERN_STAMPEDE &&
	    bench.threads > bench.chunk_size / bench.page_size) {
		fprintf(stderr, "stampede needs no more than one thread per chunk page\n");
		goto out;
	}

	bench.npages = bench.pattern == PATTERN_STAMPEDE ?
		(bench.length / bench.chunk_size) * bench.threads :
		bench.length / bench.page_size;
	bench.latency_capacity =
		(bench.npages + bench.threads - 1) / bench.threads;
	bench.latencies = calloc(bench.latency_capacity * bench.threads,
				 sizeof(*bench.latencies));
	workers = calloc(bench.threads, sizeof(*workers));
	if (!bench.latencies || !workers) {
		fprintf(stderr, "allocation failed\n");
		goto out;
	}
	if (bench.pattern != PATTERN_STAMPEDE) {
		uint64_t n;

		bench.pages = malloc(bench.npages * sizeof(*bench.pages));
		if (!bench.pages) {
			fprintf(stderr, "page-list allocation failed\n");
			goto out;
		}
		for (n = 0; n < bench.npages; n++) {
			bench.pages[n] = n;
		}
		if (bench.pattern == PATTERN_RANDOM) {
			shuffle_pages(bench.pages, bench.npages, seed);
		}
	}

	bench.mapping = mmap(NULL, bench.length, PROT_READ | PROT_WRITE,
			     MAP_PRIVATE | MAP_NORESERVE, fd,
			     (off_t)bench.map_offset);
	if (bench.mapping == MAP_FAILED) {
		bench.mapping = NULL;
		fprintf(stderr, "mmap %s: %s\n", device, strerror(errno));
		goto out;
	}
	if (pthread_barrier_init(&bench.start, NULL, bench.threads + 1) != 0 ||
	    pthread_barrier_init(&bench.cohort, NULL, bench.threads) != 0) {
		fprintf(stderr, "barrier initialization failed\n");
		goto out;
	}

	for (i = 0; i < bench.threads; i++) {
		workers[i].bench = &bench;
		workers[i].id = i;
		workers[i].latency_offset = i * bench.latency_capacity;
		if (pthread_create(&workers[i].thread, NULL, worker_main,
				   &workers[i]) != 0) {
			fprintf(stderr, "pthread_create failed at worker %u\n", i);
			/* The workers already created are blocked at a barrier whose
			 * participant count cannot be changed. Exiting the helper is
			 * the only non-deadlocking recovery. */
			exit(EXIT_FAILURE);
		}
		created++;
	}
	/* Thread stacks and libc setup are benchmark scaffolding, not memory-image
	 * faults, so start process fault accounting only after workers exist. */
	getrusage(RUSAGE_SELF, &before);
	begin = now_ns();
	pthread_barrier_wait(&bench.start);
	for (i = 0; i < created; i++) {
		pthread_join(workers[i].thread, NULL);
	}
	created = 0;
	elapsed = now_ns() - begin;
	getrusage(RUSAGE_SELF, &after);

	for (i = 0; i < bench.threads; i++) {
		if (workers[i].error != 0) {
			fprintf(stderr, "worker %u barrier failed: %d\n",
				i, workers[i].error);
			goto out;
		}
		total_checksum += workers[i].checksum;
		total_samples += workers[i].samples;
	}
	if (total_samples != bench.npages) {
		fprintf(stderr, "sample count mismatch: %" PRIu64 " != %" PRIu64 "\n",
			total_samples, bench.npages);
		goto out;
	}
	total_samples = 0;
	for (i = 0; i < bench.threads; i++) {
		memmove(&bench.latencies[total_samples],
			&bench.latencies[workers[i].latency_offset],
			workers[i].samples * sizeof(*bench.latencies));
		total_samples += workers[i].samples;
	}
	qsort(bench.latencies, total_samples, sizeof(*bench.latencies), compare_u64);

	printf("{\"device\":\"%s\",\"pattern\":\"%s\",\"threads\":%u,"
	       "\"offset_bytes\":%" PRIu64 ",\"mapped_bytes\":%" PRIu64 ","
	       "\"touched_bytes\":%" PRIu64 ",\"samples\":%" PRIu64 ","
	       "\"write_percent\":%u,\"elapsed_ms\":%.3f,\"mib_per_sec\":%.3f,"
	       "\"latency_us\":{\"p50\":%.3f,\"p95\":%.3f,\"p99\":%.3f,"
	       "\"max\":%.3f},\"faults\":{\"major\":%ld,\"minor\":%ld},"
	       "\"checksum\":%" PRIu64 "}\n",
	       device, pattern_name(bench.pattern), bench.threads, bench.map_offset,
	       bench.length, total_samples * bench.page_size, total_samples,
	       bench.write_percent, (double)elapsed / 1e6,
	       ((double)(total_samples * bench.page_size) / (1024.0 * 1024.0)) /
		       ((double)elapsed / 1e9),
	       (double)percentile(bench.latencies, bench.npages, 50) / 1e3,
	       (double)percentile(bench.latencies, bench.npages, 95) / 1e3,
	       (double)percentile(bench.latencies, bench.npages, 99) / 1e3,
	       (double)bench.latencies[bench.npages - 1] / 1e3,
	       after.ru_majflt - before.ru_majflt,
	       after.ru_minflt - before.ru_minflt, total_checksum);
	rc = EXIT_SUCCESS;
	goto out;

out:
	if (bench.mapping) {
		munmap(bench.mapping, bench.length);
	}
	if (fd >= 0) {
		close(fd);
	}
	free(bench.pages);
	free(bench.latencies);
	free(workers);
	return rc;
}
