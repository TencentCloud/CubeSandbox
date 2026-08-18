# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  s3lvol top-level Makefile
#
#  Common targets:
#    make check-env     verify the SPDK / aws-c-s3 environment (run this first)
#    make               libraries + binary (default; the binary is always fresh)
#    make shared        build the .so only
#    make static        build the .a only
#    make app           build the binaries under app/
#    make check-rules   check the three layering hard rules (HANDOFF section 8)
#    make check         run every test the environment allows (= test/run_all.sh)
#    make check-offline run only the tests that need no S3 credentials and no root
#    make clean
#
#  Build modes:
#    S3LVOL_BUILD_TYPE=debug (default)  -DDEBUG -g3 -O0 -fno-omit-frame-pointer
#    S3LVOL_BUILD_TYPE=release          -g -O2
#  -DDEBUG matters: without it, the SPDK_DEBUGLOG calls in the code never make it
#  into the binary at all. Switching triggers a full rebuild, for the reasons
#  described in mk/s3lvol.common.mk.
#  The other two knobs (SPDK's --enable-debug and CRT's AWS_BUILD_TYPE) are
#  independent of this one; `make check-env` prints all three together.
#

S3LVOL_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
include $(S3LVOL_ROOT)/mk/s3lvol.common.mk

DIRS := lib/s3bsdev module/bdev/s3lvol
APP_DIRS := app/s3lvol_tgt

.PHONY: all shared static app clean check-env check check-offline check-rules $(DIRS)

# The default target builds the binaries too, not just the libraries.
#
# It used to be `all: shared`, with app/s3lvol_tgt outside DIRS and therefore
# outside every top-level target. A plain `make` rebuilt the libraries and left
# the binary alone, so the tests kept exercising whatever had last been linked by
# hand -- and since nothing warns about that, a full dataplane regression once
# came back green against a binary that predated the fix it was meant to prove.
# Only the log line naming a function that no longer existed gave it away.
#
# app/s3lvol_tgt rebuilds what it links: it marks the .a files phony and recurses
# into lib/ and module/, so naming it here is enough to keep the binary current.
# test/integration does the same for its own test binaries.
#
# The two recipes run in sequence rather than as prerequisites of `all`: under -j
# the `static` half of `app` and a concurrent `static` would write the same
# archives at the same time.
all:
	@$(MAKE) --no-print-directory shared
	@$(MAKE) --no-print-directory app

shared:
	@for d in $(DIRS); do $(MAKE) --directory=$$d shared || exit 1; done

static:
	@for d in $(DIRS); do $(MAKE) --directory=$$d static || exit 1; done

# Binaries. Each app directory is responsible for its own libraries being fresh.
app:
	@for d in $(APP_DIRS); do $(MAKE) --directory=$$d || exit 1; done

clean:
	@for d in $(DIRS) $(APP_DIRS); do $(MAKE) --directory=$$d clean || exit 1; done

# ---------------------------------------------------------------------------
# Tests
#
# These two targets only forward to test/run_all.sh; the logic lives there,
# because the decisions it has to make -- credentials present or not, root or
# not, another target running on this host or not, which suites must therefore
# be skipped -- are clearer in shell than they would be as a pile of $(shell)
# calls in make.
#
# run_all.sh runs make itself, so no dependency is declared here: it judges
# freshness on its own, rather than letting two places disagree about it.
#
# The exit codes are worth remembering: 0 all passed, 1 something failed,
# **2 all passed but some suites could not run because of the environment**.
# 2 is deliberately distinct from 0 -- "did not run" sharing a code with "ran
# and passed" is exactly how a CI goes green without having tested anything.
# ---------------------------------------------------------------------------
check:
	@$(S3LVOL_ROOT)/test/run_all.sh

check-offline:
	@$(S3LVOL_ROOT)/test/run_all.sh --offline

# Layering rules (HANDOFF section 8). Runs in seconds, and a violation makes
# lib/ unable to link on its own -- which is the premise of every test under
# test/integration/.
check-rules:
	@$(S3LVOL_ROOT)/test/tools/check_layering.sh

# ---------------------------------------------------------------------------
# Environment self-check: run this first thing on a development machine
# ---------------------------------------------------------------------------
check-env:
	@echo "=== Paths ==="
	@echo "S3LVOL_ROOT     = $(S3LVOL_ROOT)"
	@echo "SPDK_ROOT       = $(SPDK_ROOT)"
	@echo "AWS_INSTALL_DIR = $(if $(AWS_INSTALL_DIR),$(AWS_INSTALL_DIR),(system library))"
	@echo ""
	@echo "=== Build modes (three independent knobs) ==="
	@echo "  s3lvol  S3LVOL_BUILD_TYPE = $(s3lvol_bt)"
	@echo "          $(s3lvol_bt_cflags)"
	@printf "  SPDK    CONFIG_DEBUG     = "
	@grep -m1 '^CONFIG_DEBUG' "$(SPDK_ROOT)/mk/config.mk" 2>/dev/null | cut -d= -f2 \
		|| echo "(cannot read $(SPDK_ROOT)/mk/config.mk; SPDK may not have been configured)"
	@printf "  AWS CRT AWS_BUILD_TYPE   = "
	@cat "$(S3LVOL_ROOT)/deps/aws/.build_type" 2>/dev/null \
		|| echo "(not recorded; see setup_dep.sh)"
	@echo ""
	@echo "=== SPDK source tree ==="
	@test -d "$(SPDK_ROOT)" \
		&& echo "  [OK]   SPDK directory exists" \
		|| { echo "  [FAIL] cannot find $(SPDK_ROOT); pass make SPDK_ROOT=<path>"; exit 1; }
	@test -f "$(SPDK_HEADER_DIR)/spdk/blob.h" \
		&& echo "  [OK]   include/spdk/blob.h" \
		|| echo "  [FAIL] missing $(SPDK_HEADER_DIR)/spdk/blob.h"
	@test -d "$(SPDK_LIB_DIR)" \
		&& echo "  [OK]   build/lib exists (SPDK is built)" \
		|| echo "  [FAIL] missing $(SPDK_LIB_DIR); run ./configure && make inside the SPDK tree first"
	@printf "  SPDK version: "; cd "$(SPDK_ROOT)" && git describe --tags 2>/dev/null || echo "(not a git tree)"
	@echo ""
	@echo "=== Key SPDK APIs (s3lvol depends on these; absence means the version is too old) ==="
	@grep -q "spdk_lvol_create_esnap_clone" "$(SPDK_HEADER_DIR)/spdk/lvol.h" 2>/dev/null \
		&& echo "  [OK]   spdk_lvol_create_esnap_clone  (external snapshot support)" \
		|| echo "  [FAIL] missing spdk_lvol_create_esnap_clone -- SPDK too old for cross-node migration"
	@grep -q "esnap_bs_dev_create" "$(SPDK_HEADER_DIR)/spdk/lvol.h" 2>/dev/null \
		&& echo "  [OK]   spdk_lvs_opts.esnap_bs_dev_create" \
		|| echo "  [FAIL] missing esnap_bs_dev_create field"
	@grep -q "struct spdk_bs_dev {" "$(SPDK_HEADER_DIR)/spdk/blob.h" 2>/dev/null \
		&& echo "  [OK]   struct spdk_bs_dev is a public struct" \
		|| echo "  [FAIL] struct spdk_bs_dev not in the public headers"
	@echo ""
	@echo "=== Build tools ==="
	@which pkg-config >/dev/null 2>&1 && echo "  [OK]   pkg-config" || echo "  [FAIL] missing pkg-config"
	@echo ""
	@echo "=== aws-c-s3 ==="
	@echo '#include <aws/s3/s3_client.h>' > /tmp/.s3lvol_awstest.c
	@echo 'int main(void){return 0;}' >> /tmp/.s3lvol_awstest.c
	@$(CC) $(AWS_CFLAGS) -c /tmp/.s3lvol_awstest.c -o /tmp/.s3lvol_awstest.o 2>/dev/null \
		&& echo "  [OK]   aws/s3/s3_client.h usable" \
		|| echo "  [FAIL] aws-c-s3 headers not found; install it or pass make AWS_INSTALL_DIR=<prefix>"
	@rm -f /tmp/.s3lvol_awstest.c /tmp/.s3lvol_awstest.o
	@echo ""
	@echo "=== pkg-config expansion ==="
	@echo "SPDK_LIBS = $(if $(SPDK_LIBS),$(SPDK_LIBS),(empty -- SPDK may not be built))"
