# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  test/integration
#
#  Test programs, with increasing dependencies:
#
#    s3_spawner_test        no S3, no credentials, no DPDK     -> CI can run it
#                                                               unconditionally
#    s3_thread_bounce_test  no S3, no credentials, needs DPDK  -> CI can run it
#                                                               (--no-huge)
#    s3_client_test         needs a real S3 backend + creds    -> manual / CI
#                                                               with credentials
#
#  The other nine (journal / wal / cache / flush / export / statefile / local_dev
#  / checkpoint / bs_dev) sit between these: the offline ones build an aio bdev or
#  stub the S3 client, and only bs_dev_test and client_test need credentials.
#
#  Building:
#    make -C test/integration
#
#  Running (credentials are read from the environment only, never passed on the
#  command line):
#    export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#    ./s3_client_test --endpoint <host> --bucket <name> [--path-style] [--no-tls]
#    ./s3_spawner_test
#    ./s3_thread_bounce_test
#
#  === Two ways to link, and they must not be mixed ===
#
#  lib/s3bsdev references three SPDK symbols: spdk_log, spdk_get_thread and
#  spdk_thread_send_msg. spdk_log links on its own; the other two live in
#  libspdk_thread.a, and linking that library drags in spdk_env_dpdk and with it
#  the whole DPDK EAL.
#
#  (a) Stubbed (spdk_thread_stub.c): used by s3_client_test / s3_spawner_test.
#      They submit from plain pthreads, so owner_thread is always NULL and the
#      bounce branch is unreachable at runtime; the stub is therefore correct
#      rather than papering over a problem (see the stub's header comment).
#
#  (b) Real thread library: used by s3_thread_bounce_test. It needs real
#      spdk_threads to exercise the bounce, so it links spdk_thread +
#      spdk_env_dpdk in full and passes --no-huge to avoid the hugepages
#      requirement. It must not link the stub, or the symbols clash.
#
#  === DPDK is always static, and always --whole-archive ===
#
#  Anything in group (b) (pkg-config output containing -lrte_*) goes through
#  $(call dpdk_link_args,...) to get .a full paths wrapped in --whole-archive;
#  the reason is in mk/s3lvol.common.mk. What deserves an extra note here is why
#  the *tests* have to do this too, rather than "tests only run on dev boxes":
#
#    - These tests are the first check of whether the binary will run anywhere
#      else. They use the same link arguments as the target, so a difference
#      between the two (right in the test, wrong in the target, or vice versa)
#      would make the two conclusions incomparable.
#    - More concretely: a dropped DPDK constructor shows up at *runtime* (EAL
#      comes up but cannot find a mempool ops named "ring"). Of the links here,
#      s3_thread_bounce_test is the first to reach spdk_env_init() -- it
#      finishes in 40 seconds, making it the cheapest probe for that class of
#      error.
#
#  This file used to rely on -Wl,-rpath pointing into the local SPDK tree to
#  find librte_*.so, so the test programs could not be copied to another machine
#  either. The rpath has since been removed entirely.
#

include ../../mk/s3lvol.common.mk

S3BSDEV_DIR = ../../lib/s3bsdev
MODULE_DIR   = ../../module/bdev/s3lvol

APPS = s3_client_test s3_spawner_test s3_thread_bounce_test s3_bs_dev_test \
       s3_journal_test s3_wal_test s3_cache_test s3_flush_test s3_export_test \
       s3_statefile_test s3_local_dev_test s3_checkpoint_test

# (a) minimal library set for the stubbed path
STUB_SRCS      = spdk_thread_stub.c
STUB_SPDK_LIBS = -lspdk_log -lspdk_util

# (b) real thread library path: let pkg-config expand it, rather than hand-
#     maintaining the DPDK dependency order
REAL_PC= $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                   pkg-config --libs spdk_thread spdk_env_dpdk 2>/dev/null)
REAL_SPDK_LIBS = $(call spdk_only_libs,$(REAL_PC))
REAL_DPDK_ARGS = $(call dpdk_link_args,$(REAL_PC))

# (c) blobstore: the Step 3 acceptance test needs spdk_blob, and since s3_bs_dev
#     now calls into the WAL (which does bdev I/O) it needs spdk_bdev as well --
#     even in the direct-to-S3 mode this test uses, where no WAL is attached.
BLOB_PC        = $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                   pkg-config --libs spdk_blob spdk_bdev spdk_accel spdk_thread \
                                     spdk_env_dpdk 2>/dev/null)
BLOB_SPDK_LIBS = $(call spdk_only_libs,$(BLOB_PC))
BLOB_DPDK_ARGS = $(call dpdk_link_args,$(BLOB_PC))

TEST_SYS_LIBS  = $(OPENSSL_STATIC_LIBS) -pthread -lrt -luuid -lm -ldl

.PHONY: all clean check

all: $(APPS)

# The build flags are not part of the dependencies, so switching
# S3LVOL_BUILD_TYPE has to go through this stamp (see mk/). Every test is a
# single cc step, so hanging it off the executables is enough.
$(APPS): $(S3LVOL_BUILD_STAMP)

s3_client_test: s3_client_test.c $(STUB_SRCS) $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_client_test.c $(STUB_SRCS) \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(STUB_SPDK_LIBS) -Wl,--end-group \
	    $(TEST_SYS_LIBS) -o $@

s3_spawner_test: s3_spawner_test.c $(STUB_SRCS) $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_spawner_test.c $(STUB_SRCS) \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(STUB_SPDK_LIBS) -Wl,--end-group \
	    $(TEST_SYS_LIBS) -o $@

# Note: STUB_SRCS is deliberately not linked here -- this test exists to
# exercise the real spdk_thread. It is the first of the links to reach
# spdk_env_init(), which makes it the earliest and cheapest place this class of
# "wrong DPDK static link" error shows up (see the header).
s3_thread_bounce_test: s3_thread_bounce_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_thread_bounce_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(REAL_SPDK_LIBS) -Wl,--end-group \
	    $(REAL_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# Step 3 acceptance: blobstore on S3. Needs spdk_blob and a real spdk_thread,
# so the stub is not linked.
s3_bs_dev_test: s3_bs_dev_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_bs_dev_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(BLOB_SPDK_LIBS) -Wl,--end-group \
	    $(BLOB_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# chunk map persistence check: no S3, but it does need the bdev layer.
#
# The local device is the upstream bdev_aio plus a sparse file created with
# ftruncate (under /data by default). No hand-written in-memory bdev: using a
# real bdev module also exercises the 4 KiB alignment and O_DIRECT constraints,
# and it is one less implementation to maintain.
#
# Two linking notes:
#
#  1. bdev_aio.h is not part of the installed includes, so -I$(SPDK_ROOT)/module
#     is required to #include "bdev/aio/bdev_aio.h". Do not copy the prototype --
#     a copy drifts from upstream.
#  2. --whole-archive must cover SPDK's own libraries: the bdev module (aio) and
#     the accel module (software) both self-register from constructors, with no
#     symbol referenced explicitly, so an ordinary link drops them
#     (same as HANDOFF 5.15).
JOURNAL_CFLAGS = -I$(SPDK_ROOT)/module

JOURNAL_PC = $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                      pkg-config --libs spdk_bdev_aio spdk_accel spdk_bdev \
                                        spdk_thread spdk_env_dpdk \
                      2>/dev/null)
JOURNAL_SPDK_LIBS = $(call spdk_only_libs,$(JOURNAL_PC))
JOURNAL_DPDK_ARGS = $(call dpdk_link_args,$(JOURNAL_PC))

s3_journal_test: s3_journal_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) $(JOURNAL_CFLAGS) s3_journal_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--whole-archive \
	        -Wl,--start-group $(JOURNAL_SPDK_LIBS) -Wl,--end-group \
	    -Wl,--no-whole-archive \
	    $(JOURNAL_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

s3_wal_test: s3_wal_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) $(JOURNAL_CFLAGS) s3_wal_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--whole-archive \
	        -Wl,--start-group $(JOURNAL_SPDK_LIBS) -Wl,--end-group \
	    -Wl,--no-whole-archive \
	    $(JOURNAL_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# The cache reads and writes a region of a real bdev, so it needs the same
# treatment as the journal and WAL tests: bdev_aio has to register itself, hence
# --whole-archive.
s3_cache_test: s3_cache_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) $(JOURNAL_CFLAGS) s3_cache_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--whole-archive \
	        -Wl,--start-group $(JOURNAL_SPDK_LIBS) -Wl,--end-group \
	    -Wl,--no-whole-archive \
	    $(JOURNAL_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# overlay + flusher: no S3 and no local device, but two things still pull in
# libraries. The flusher registers a poller, so a real spdk_thread is needed, and
# it calls into the WAL to report truncation progress, which drags in spdk_bdev
# even though this test passes no WAL at all. Hence the same library set as the
# journal and WAL tests, minus --whole-archive: no bdev module has to register
# itself here.
s3_flush_test: s3_flush_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_flush_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(JOURNAL_SPDK_LIBS) -Wl,--end-group \
	    $(JOURNAL_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# manifest only: no S3, no local device, no threads, no DPDK init.
#
# It still has to be linked against spdk_blob and the environment, because the
# object it needs (s3_export.o) also holds the copy engine, which calls
# spdk_blob_io_read() and spdk_zmalloc(). Nothing in this test reaches them --
# the link is satisfied, not exercised. Splitting the manifest into its own
# translation unit purely to shorten this line would be the tail wagging the dog.
EXPORT_PC = $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                     pkg-config --libs spdk_blob spdk_bdev spdk_accel spdk_thread \
                                       spdk_json spdk_util spdk_log \
                                       spdk_env_dpdk 2>/dev/null)
EXPORT_SPDK_LIBS = $(call spdk_only_libs,$(EXPORT_PC))
EXPORT_DPDK_ARGS = $(call dpdk_link_args,$(EXPORT_PC))

s3_export_test: s3_export_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_export_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(EXPORT_SPDK_LIBS) -Wl,--end-group \
	    $(EXPORT_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# The state file writer is pure file I/O: it needs no bdev, no threads, no DPDK.
# It is compiled directly (it lives in the module, whose .a also drags in bdev /
# nvmf), and it only pulls in spdk_log -- the one SPDK symbol its macros use.
# The extra -I finds vbdev_s3lvol.h, which is a module-local header.
s3_statefile_test: s3_statefile_test.c $(MODULE_DIR)/vbdev_s3lvol_statefile.c
	$(CC) $(COMMON_CFLAGS) -I$(MODULE_DIR) s3_statefile_test.c \
	    $(MODULE_DIR)/vbdev_s3lvol_statefile.c \
	    -Wl,--start-group $(STUB_SPDK_LIBS) -Wl,--end-group \
	    $(TEST_SYS_LIBS) -o $@

# Super block validation: needs a real bdev (same shape as the journal test), so
# it links the aio module whole-archive and brings the framework up by hand.
s3_local_dev_test: s3_local_dev_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) $(JOURNAL_CFLAGS) s3_local_dev_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--whole-archive \
	        -Wl,--start-group $(JOURNAL_SPDK_LIBS) -Wl,--end-group \
	    -Wl,--no-whole-archive \
	    $(JOURNAL_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# Checkpoint validation: no S3, no DPDK init. The test stubs s3_head /
# s3_get_range / s3_put / s3_delete, which keeps s3_client_aws.o out of the
# link and lets the load run synchronously. But s3_chunk_map.o, which
# s3_checkpoint.o needs, also references s3_journal_append_* in its insert /
# remove paths, and that drags in s3_journal.o + s3_local_dev.o with their bdev
# and DMA dependencies. Same link as the export test therefore: the bdev /
# thread / env libraries are linked to satisfy those objects, but never
# initialised -- the stubs resolve everything the test actually calls.
s3_checkpoint_test: s3_checkpoint_test.c $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3_checkpoint_test.c \
	    $(S3BSDEV_DIR)/libs3bsdev.a \
	    $(AWS_LIBS) \
	    -Wl,--start-group $(EXPORT_SPDK_LIBS) -Wl,--end-group \
	    $(EXPORT_DPDK_ARGS) \
	    $(SYS_LIBS) $(TEST_SYS_LIBS) -o $@

# Run only the tests that need no S3 credentials
check: s3_spawner_test s3_thread_bounce_test s3_journal_test s3_wal_test s3_cache_test \
       s3_flush_test s3_export_test s3_statefile_test s3_local_dev_test \
       s3_checkpoint_test
	./s3_spawner_test
	./s3_thread_bounce_test
	./s3_journal_test
	./s3_wal_test
	./s3_cache_test
	./s3_flush_test
	./s3_export_test
	./s3_statefile_test
	./s3_local_dev_test
	./s3_checkpoint_test

.PHONY: $(S3BSDEV_DIR)/libs3bsdev.a
$(S3BSDEV_DIR)/libs3bsdev.a:
	$(MAKE) --directory=$(S3BSDEV_DIR) static

# Header dependencies recorded by -MMD (see mk/s3lvol.common.mk). Each test is
# compiled and linked in one step, so the .d is named after the executable.
-include $(APPS:=.d)

clean:
	rm -f $(APPS) *.o *.d
