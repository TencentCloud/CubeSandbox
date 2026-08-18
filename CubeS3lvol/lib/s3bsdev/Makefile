# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  lib/s3bsdev -- the core library
#
#  Dependency constraints (enforced by the CI check):
#    1. Nothing in this directory may include a header from module/ (it must
#       stay unit-testable on its own).
#    2. Only s3_client_aws.c may include <aws/...>.
#

include ../../mk/s3lvol.common.mk

LIBNAME = s3bsdev

SRCS = s3_bs_dev.c \
       s3_local_dev.c \
       s3_spawner.c \
       s3_export_bs_dev.c \
       s3_chunk_map.c \
       s3_wal.c \
       s3_overlay.c \
       s3_cache.c \
       s3_flusher.c \
       s3_checkpoint.c \
       s3_journal.c \
       s3_owner.c \
       s3_gc.c \
       s3_export.c \
       s3_export_ref.c \
       s3_client_aws.c

OBJS = $(SRCS:.c=.o)
PIC_OBJS = $(SRCS:.c=.pic.o)
DEPS = $(OBJS:.o=.d) $(PIC_OBJS:.o=.d)

.PHONY: shared static clean

shared: lib$(LIBNAME).so

static: lib$(LIBNAME).a

lib$(LIBNAME).so: $(PIC_OBJS)
	$(CC) $(COMMON_CFLAGS) -shared $^ $(AWS_LIBS) -o $@

lib$(LIBNAME).a: $(OBJS)
	$(AR) rcs $@ $^

%.pic.o: %.c
	$(CC) $(COMMON_CFLAGS) -fPIC -c $< -o $@

%.o: %.c
	$(CC) $(COMMON_CFLAGS) -c $< -o $@

# The build flags are not part of the .o dependencies, so switching
# S3LVOL_BUILD_TYPE has to go through this stamp (see mk/).
$(OBJS) $(PIC_OBJS): $(S3LVOL_BUILD_STAMP)

# Header dependencies recorded by -MMD (see mk/s3lvol.common.mk). Absent on a
# clean tree, hence -include: everything gets compiled that first time anyway.
-include $(DEPS)

clean:
	rm -f *.o *.pic.o *.d lib$(LIBNAME).so lib$(LIBNAME).a
