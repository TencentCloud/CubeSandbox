# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  app/s3lvol_tgt -- the SPDK target carrying the s3lvol module
#
#  Building:
#    make -C app/s3lvol_tgt
#
#  Running (credentials are read from the environment):
#    export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#    ./s3lvol_tgt -m 0x3            # needs root (DPDK)
#
#  === --whole-archive must cover SPDK's own libraries, not just ours ===
#
#  Three kinds of things in SPDK register themselves through constructors, with
#  no symbol referenced explicitly:
#    - SPDK_BDEV_MODULE_REGISTER  bdev modules
#    - SPDK_RPC_REGISTER          RPC methods
#    - SPDK_SUBSYSTEM_REGISTER    bdev/nvmf/accel and other subsystems, in the
#                                 event_* libraries
#
#  Under an ordinary static link the linker drops those "unreferenced" object
#  files wholesale. Adding --whole-archive to our own libraries alone is not
#  enough -- then g_subsystems comes up empty, and the symptoms are misleading:
#  the process starts, the RPC socket exists, and our own RPCs even register
#  (they are in our library), yet
#    * framework_wait_init hangs forever
#    * bdev_get_bdevs reports "Method not found"
#    * the bdev modules' module_init is never called
#      (bdev_modules_init belongs to the bdev subsystem, which was never
#      registered at all)
#  It looks like "our module did not register" when in fact none of SPDK's
#  subsystems registered.
#
#  So SPDK's libraries go into --whole-archive as well.
#
#  **DPDK's libraries go in too** (this file used to say "DPDK's do not need it,
#  they are referenced explicitly and can sit in a plain group"; that only holds
#  for the shared-library form, where constructors still run at dlopen time).
#  Under static linking, rte_bus_pci / rte_mempool_ring / rte_power_* are all
#  RTE_INIT-registered and referenced by no symbol; the symptom of dropping them
#  is *runtime* -- EAL comes up but finds no bus to probe, or no mempool ops
#  named "ring". DPDK's own libdpdk.pc is written that way (the whole Libs.private
#  section wrapped in --whole-archive), and so is SPDK's
#  (lib/env_dpdk/env.mk:117, DPDK_STATIC_LIB_LINKER_ARGS).
#
#  === Why the .a full paths instead of -L + -l ===
#
#  When libs3lvol_bdev.a and .so coexist in one directory, `-L<dir> -ls3lvol_bdev`
#  makes the linker prefer the .so, so --whole-archive has no effect on it (it
#  is for static libraries), the constructors are dropped anyway, and the .so
#  being a product of a different build surfaces as undefined references.
#  Giving the .a path outright lets the .a and .so coexist without interfering.
#
#  **DPDK's directory is exactly this case** (.a and .so side by side), so the
#  -lrte_* that pkg-config returns are rewritten to .a full paths by
#  dpdk_static_libs in mk/s3lvol.common.mk. Without that step the linker picks
#  the .so, and the binary carries 30 librte_*.so.26 dependencies plus two rpaths
#  into the local SPDK tree -- fine on a dev box, broken on any other machine.
#
#  **OpenSSL is the same trick.** -lssl would pick libssl.so.1.1 next to
#  libssl.a, and the package would need that SONAME on the target. Ubuntu 22.04+
#  does not ship it. OPENSSL_STATIC_LIBS names the .a files; SYS_LIBS has already
#  dropped -lssl/-lcrypto so they cannot come back as shared objects.
#

include ../../mk/s3lvol.common.mk

S3BSDEV_DIR = ../../lib/s3bsdev
MODULE_DIR  = ../../module/bdev/s3lvol

APP = s3lvol_tgt

# The full SPDK app subsystem set: event_nvmf drags in bdev / sock / accel and
# so on, and pkg-config expands the list so the order is not maintained by hand.
# spdk_env_dpdk must be listed explicitly -- none of the .pc files above contain
# it, and leaving it out surfaces as undefined references on symbols like
# spdk_env_get_*_core / spdk_get_ticks / spdk_free.
SPDK_APP_LIBS = $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                  pkg-config --libs spdk_event_nvmf spdk_event_bdev spdk_lvol \
                              spdk_env_dpdk 2>/dev/null)

# Keep -lspdk_* and -lrte_* apart: the former are named as-is (SPDK only ships
# .a, so they cannot be mistaken for anything else), the latter become .a full
# paths. Both groups need --whole-archive, for the reasons in the header.
SPDK_ONLY_LIBS = $(call spdk_only_libs,$(SPDK_APP_LIBS))
DPDK_STATIC_LIBS = $(call dpdk_static_libs,$(SPDK_APP_LIBS))

.PHONY: all clean

all: $(APP)
$(APP): s3lvol_tgt.c $(MODULE_DIR)/libs3lvol_bdev.a $(S3BSDEV_DIR)/libs3bsdev.a
	$(CC) $(COMMON_CFLAGS) s3lvol_tgt.c \
	    -Wl,--whole-archive \
	        $(MODULE_DIR)/libs3lvol_bdev.a \
	        $(S3BSDEV_DIR)/libs3bsdev.a \
	        -Wl,--start-group $(SPDK_ONLY_LIBS) -Wl,--end-group \
	        -Wl,--start-group $(DPDK_STATIC_LIBS) -Wl,--end-group \
	    -Wl,--no-whole-archive \
	    $(AWS_LIBS) \
	    $(OPENSSL_STATIC_LIBS) \
	    $(SYS_LIBS) -pthread -lrt -luuid -lm -ldl -o $@

# Both marked phony so the sub-makes always run: without that, an existing .a is
# considered up to date no matter what changed under lib/ or module/, and the app
# links yesterday's library. Recursing costs a no-op make when nothing changed.
.PHONY: $(MODULE_DIR)/libs3lvol_bdev.a $(S3BSDEV_DIR)/libs3bsdev.a

# The build flags are not part of the dependencies, so switching
# S3LVOL_BUILD_TYPE has to go through this stamp (see mk/). The libraries hang
# their own copy of the dependency; this line covers s3lvol_tgt.c itself.
$(APP): $(S3LVOL_BUILD_STAMP)

$(MODULE_DIR)/libs3lvol_bdev.a:
	$(MAKE) --directory=$(MODULE_DIR) static

$(S3BSDEV_DIR)/libs3bsdev.a:
	$(MAKE) --directory=$(S3BSDEV_DIR) static

# Header dependencies for s3lvol_tgt.c itself, recorded by -MMD. The name comes
# from -o, so it is $(APP).d.
-include $(APP).d

clean:
	rm -f $(APP) *.o *.d
