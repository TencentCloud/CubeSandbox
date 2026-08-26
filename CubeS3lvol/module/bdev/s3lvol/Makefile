# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  module/bdev/s3lvol -- the vbdev module + RPC
#
#  The .so it produces can be loaded directly by spdk_tgt:
#    LD_PRELOAD=./libs3lvol_bdev.so spdk/build/bin/spdk_tgt
#

include ../../../mk/s3lvol.common.mk

LIBNAME = s3lvol_bdev
S3BSDEV_DIR = ../../../lib/s3bsdev

SRCS = vbdev_s3lvol.c \
       vbdev_s3lvol_lvstore.c \
       vbdev_s3lvol_lvol.c \
       vbdev_s3lvol_xfer.c vbdev_s3lvol_exports.c \
       vbdev_s3lvol_namespace.c \
       vbdev_s3lvol_statefile.c \
       vbdev_s3lvol_active.c \
       vbdev_s3lvol_nvmf.c \
       vbdev_s3lvol_bstore.c \
       vbdev_s3lvol_rpc.c

OBJS = $(SRCS:.c=.o)
PIC_OBJS = $(SRCS:.c=.pic.o)
DEPS = $(OBJS:.o=.d) $(PIC_OBJS:.o=.d)

.PHONY: shared static clean

shared: lib$(LIBNAME).so

static: lib$(LIBNAME).a

# Link s3bsdev with whole-archive so the SPDK_BDEV_MODULE_REGISTER /
# SPDK_RPC_REGISTER constructors are not dropped by the linker as unreferenced
# symbols (section 12.0.1 of the design doc).
lib$(LIBNAME).so: $(PIC_OBJS)
	$(CC) $(COMMON_CFLAGS) -shared $^ \
	    $(call add_whole_archive,-L$(S3BSDEV_DIR) -ls3bsdev) \
	    $(AWS_LIBS) -o $@

lib$(LIBNAME).a: $(OBJS)
	$(AR) rcs $@ $^

%.pic.o: %.c
	$(CC) $(COMMON_CFLAGS) -fPIC -c $< -o $@

%.o: %.c
	$(CC) $(COMMON_CFLAGS) -c $< -o $@

# The build flags are not part of the .o dependencies, so switching
# S3LVOL_BUILD_TYPE has to go through this stamp (see mk/).
$(OBJS) $(PIC_OBJS): $(S3LVOL_BUILD_STAMP)

# Header dependencies recorded by -MMD (see mk/s3lvol.common.mk). This directory
# is the one that got caught by their absence: vbdev_s3lvol_lvstore.o kept an old
# struct layout after a header change and read the wrong field.
-include $(DEPS)

clean:
	rm -f *.o *.pic.o *.d lib$(LIBNAME).so lib$(LIBNAME).a
