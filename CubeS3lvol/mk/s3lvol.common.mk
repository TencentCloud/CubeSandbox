# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Common build variables for s3lvol.
#
#  SPDK is located in this order:
#    1. SPDK_ROOT=...        explicit, always wins
#    2. <repo>/deps/spdk     what ./setup_dep.sh builds
#    3. /opt/s3lvol-spdk     builder-image prebuilt (stamp-matched by setup_dep.sh)
#    4. <repo>/../spdk       sibling layout, the historical default
#
#  If SPDK is not anywhere above:
#    ./setup_dep.sh          downloads, patches and builds it into deps/spdk
#  or
#    make SPDK_ROOT=/opt/spdk
#

S3LVOL_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)

# deps/spdk wins over ../spdk because it is the copy this repository built
# itself, with a known version and patch state; ../spdk is whoever happened to
# put something there. Probe include/spdk/blob.h rather than the directory:
# setup_dep.sh interrupted mid-clone leaves an empty directory, and the search
# should keep going instead of locking onto a path that cannot be used and then
# reporting a pile of confusing errors.
ifeq ($(origin SPDK_ROOT),undefined)
ifneq ($(wildcard $(S3LVOL_ROOT)/deps/spdk/include/spdk/blob.h),)
SPDK_ROOT := $(S3LVOL_ROOT)/deps/spdk
else ifneq ($(wildcard /opt/s3lvol-spdk/include/spdk/blob.h),)
SPDK_ROOT := /opt/s3lvol-spdk
else
SPDK_ROOT := $(abspath $(S3LVOL_ROOT)/../spdk)
endif
endif
SPDK_ROOT   ?= $(abspath $(S3LVOL_ROOT)/../spdk)

# ---------------------------------------------------------------------------
# SPDK patch guard.
#
# The patches under patches/ are not optional. Missing one shows up either as
# an implicit declaration deep in a build log or as an undefined symbol at link
# time -- neither tells you to go look at patches/. So probe a header up front
# and stop with a clear instruction when it is missing.
#
# The probe asks "does the header carry the declaration", not "is the patch
# file there": what decides whether this builds is the state of the SPDK tree.
# grep is used rather than pkg-config because it holds for both static and
# shared builds.
#
# Only checked when actually compiling: clean / help must not be blocked by an
# unpatched SPDK.
# ---------------------------------------------------------------------------
ifeq ($(filter clean help,$(MAKECMDGOALS)),)
SPDK_PATCH_PROBE := $(shell grep -c spdk_blob_get_io_unit_lba \
           $(SPDK_ROOT)/include/spdk/blob.h 2>/dev/null)
ifeq ($(SPDK_PATCH_PROBE),0)
$(error SPDK at $(SPDK_ROOT) is missing the patches in patches/. Run \
        '$(S3LVOL_ROOT)/patches/apply.sh' and then rebuild SPDK \
        ('make -C $(SPDK_ROOT) -j'), or let './setup_dep.sh' do both. \
        See patches/README.md)
endif
ifeq ($(SPDK_PATCH_PROBE),)
$(error No SPDK headers at $(SPDK_ROOT)/include. Run './setup_dep.sh' to build \
        one under deps/, or set SPDK_ROOT to an existing checkout)
endif
endif

# ---------------------------------------------------------------------------
# SPDK header and library paths.
# Variable names match spdk/test/external_code/Makefile so they can be
# compared against the official example.
# ---------------------------------------------------------------------------
SPDK_HEADER_DIR     ?= $(SPDK_ROOT)/include
SPDK_LIB_DIR        ?= $(SPDK_ROOT)/build/lib
DPDK_LIB_DIR        ?= $(SPDK_ROOT)/dpdk/build/lib
ISAL_LIB_DIR        ?= $(SPDK_ROOT)/isa-l/.libs
ISAL_CRYPTO_LIB_DIR ?= $(SPDK_ROOT)/isa-l-crypto/.libs
VFIO_LIB_DIR        ?= $(SPDK_ROOT)/build/libvfio-user/usr/local/lib

# ---------------------------------------------------------------------------
# aws-c-s3 (AWS CRT)
#
# Default to deps/aws or the builder prebuilt; never a system prefix.
#
# === Why no fallback ===
#
# A fallback looks friendly but is a silent reproducibility hole, and it has
# already bitten once: /usr/local/aws on this machine was installed by another
# project, and its aws-c-s3 carries an uncommitted local change (excluding the
# 404 from the "Meta request cannot recover" ERROR log). The same tag v0.8.7
# therefore had different sources in the two places, and the build system
# picked one by prefix order without printing which.
#
# The result was behaviour varying per machine with no hint at all. While
# tracking it down I first blamed the wrong thing (CMAKE_BUILD_TYPE), because
# the same tag made me assume the sources were identical -- which is exactly
# what this fallback costs: "what is actually linked" turns into an
# archaeological question.
#
# An explicit AWS_INSTALL_DIR override still works (e.g. /usr/local/aws); it
# just has to be stated. The criterion is the header, not the directory:
# setup_dep.sh interrupted halfway leaves an empty directory, and pointing -I
# at a useless path only makes the errors harder to read.
# ---------------------------------------------------------------------------
AWS_BUILD_TYPE ?= Debug
aws_bt := $(shell printf '%s' '$(AWS_BUILD_TYPE)' | tr 'A-Z' 'a-z')
ifeq ($(aws_bt),debug)
S3LVOL_AWS_PREBUILT := /opt/s3lvol-aws-debug
else ifeq ($(aws_bt),relwithdebinfo)
S3LVOL_AWS_PREBUILT := /opt/s3lvol-aws-relwithdebinfo
else
S3LVOL_AWS_PREBUILT :=
endif

ifeq ($(origin AWS_INSTALL_DIR),undefined)
# Prefer a local deps/aws only when it exists and was built for this
# AWS_BUILD_TYPE (or has no stamp, the legacy "keep as-is" case).
ifneq ($(wildcard $(S3LVOL_ROOT)/deps/aws/include/aws/s3/s3_client.h),)
deps_aws_type := $(strip $(shell cat $(S3LVOL_ROOT)/deps/aws/.build_type 2>/dev/null))
ifeq ($(deps_aws_type),)
AWS_INSTALL_DIR := $(S3LVOL_ROOT)/deps/aws
else ifeq ($(deps_aws_type),$(AWS_BUILD_TYPE))
AWS_INSTALL_DIR := $(S3LVOL_ROOT)/deps/aws
endif
endif
ifeq ($(AWS_INSTALL_DIR),)
ifneq ($(S3LVOL_AWS_PREBUILT),)
ifneq ($(wildcard $(S3LVOL_AWS_PREBUILT)/include/aws/s3/s3_client.h),)
AWS_INSTALL_DIR := $(S3LVOL_AWS_PREBUILT)
endif
endif
endif
ifeq ($(filter clean help,$(MAKECMDGOALS)),)
ifeq ($(AWS_INSTALL_DIR),)
$(error No AWS CRT at $(S3LVOL_ROOT)/deps/aws or $(S3LVOL_AWS_PREBUILT). Run \
        './setup_dep.sh aws' to build one (ten CMake projects, a few minutes). \
        A prefix installed by something else is deliberately not picked up: \
        this machine has one whose aws-c-s3 carries a local modification, and \
        choosing between them silently is how the same commit ends up behaving \
        differently on two machines. To use one anyway, say so: \
        make AWS_INSTALL_DIR=/usr/local/aws)
endif
endif
endif
AWS_INSTALL_DIR ?=

ifneq ($(AWS_INSTALL_DIR),)
AWS_CFLAGS += -I$(AWS_INSTALL_DIR)/include

# Only a merged libaws.a is accepted, not -L plus a list of -l flags.
#
# Two reasons. The libraries depend on each other, so -l either needs the link
# order right or a --start-group; one archive makes the question go away. The
# more important one: `-L<prefix> -laws-c-s3` keeps searching the system
# library paths when the prefix is missing one of the libraries, turning this
# into "what is linked is unclear" again -- another form of the problem above.
# Naming the .a outright either hits or reports a specific error.
#
# Both lib and lib64 are considered: GNUInstallDirs installs to lib on Debian
# and lib64 on RHEL, decided per project.
ifneq ($(wildcard $(AWS_INSTALL_DIR)/lib64/libaws.a),)
AWS_LIBS := $(AWS_INSTALL_DIR)/lib64/libaws.a
else ifneq ($(wildcard $(AWS_INSTALL_DIR)/lib/libaws.a),)
AWS_LIBS := $(AWS_INSTALL_DIR)/lib/libaws.a
else
ifeq ($(filter clean help,$(MAKECMDGOALS)),)
$(error $(AWS_INSTALL_DIR) has no libaws.a (looked in lib64/ and lib/). The \
        ten aws-c-* archives have to be merged into one -- './setup_dep.sh \
        --force aws' does that with 'ar -M'. Linking them as -laws-c-s3 and \
        friends is not used here on purpose: a library missing from the prefix \
        would then be looked for in the system paths instead of being reported)
endif
endif
endif

# ---------------------------------------------------------------------------
# Compilation options.
#
# === build type ===
#
# S3LVOL_BUILD_TYPE = debug (default) | release, and it only controls this
# project's own code. There are three independent knobs and they neither move
# together nor should:
#
#   this project  S3LVOL_BUILD_TYPE=debug        (here)
#   SPDK          ./configure --enable-debug     (setup_dep.sh default)
#   AWS CRT       AWS_BUILD_TYPE=Debug           (setup_dep.sh default)
#
# debug uses the same flag set as SPDK's own CONFIG_DEBUG=y
# (spdk/mk/spdk.common.mk:295): -DDEBUG -g3 -O0 -fno-omit-frame-pointer.
# Matching it means the two sides' stacks and variables are equally visible
# when crossing the boundary in gdb.
#
# === -DDEBUG is the point, not -O0 ===
#
# SPDK's SPDK_DEBUGLOG / SPDK_LOGDUMP are gated on #ifdef DEBUG in
# include/spdk/log.h:183; without the macro they expand to empty statements.
# Which means that before this change, the SPDK_DEBUGLOG calls in this
# project's sources were never compiled into the binary at all, and no runtime
# flag could produce any output.
#
# That failure is hard to infer from symptoms: the code is there, the flag is
# accepted, logs just do not appear -- it looks like the path is never reached.
# Only the compile options reveal it.
#
# === assert is alive under both modes ===
#
# assert() is controlled by NDEBUG, and neither build type defines it, so the
# 65 asserts stay live under release too -- matching today's behaviour (today
# has neither -DDEBUG nor -DNDEBUG). This is deliberate: turning asserts off
# is a decision to review on its own, because once an assert has side effects,
# NDEBUG silently changes behaviour. If they ever need to go, do it then.
#
# === The macro is ABI-safe ===
#
# In SPDK's public headers #ifdef DEBUG appears in log.h only, affecting no
# structure layout. So the project and the SPDK libraries disagreeing on this
# macro is harmless -- conversely, the earlier combination of "SPDK built with
# -DDEBUG, this project without" was always safe too, it just threw the logs
# away.
# ---------------------------------------------------------------------------
S3LVOL_BUILD_TYPE ?= debug

# Normalise case: writing Debug should not earn an $(error).
s3lvol_bt := $(shell printf '%s' '$(S3LVOL_BUILD_TYPE)' | tr 'A-Z' 'a-z')

ifeq ($(s3lvol_bt),debug)
s3lvol_bt_cflags := -DDEBUG -g3 -O0 -fno-omit-frame-pointer
else ifeq ($(s3lvol_bt),release)
s3lvol_bt_cflags := -g -O2
else
$(error S3LVOL_BUILD_TYPE wants 'debug' or 'release', got '$(S3LVOL_BUILD_TYPE)')
endif

COMMON_CFLAGS += -I$(S3LVOL_ROOT)/include -I$(SPDK_HEADER_DIR)
COMMON_CFLAGS += -L$(SPDK_LIB_DIR) -L$(DPDK_LIB_DIR)
COMMON_CFLAGS += -L$(ISAL_LIB_DIR) -L$(ISAL_CRYPTO_LIB_DIR) -L$(VFIO_LIB_DIR)
COMMON_CFLAGS += $(AWS_CFLAGS)
COMMON_CFLAGS += -Wall -Wextra -Wno-unused-parameter -Werror
COMMON_CFLAGS += $(s3lvol_bt_cflags)
COMMON_CFLAGS += -D_GNU_SOURCE -fno-strict-aliasing

# ---------------------------------------------------------------------------
# ASan build (for debugging use-after-free). Off by default. When enabled every
# .o and the link carry -fsanitize=address. Note the sanitize flags are not part
# of the .o dependency tracking, so toggling it requires a make clean first.
# ---------------------------------------------------------------------------
ifneq ($(S3LVOL_ASAN),)
COMMON_CFLAGS += -fsanitize=address -fno-omit-frame-pointer
endif

# ---------------------------------------------------------------------------
# Switching the build type has to trigger a rebuild.
#
# .o files depend only on their .c and headers (those recorded by -MMD);
# the compile options are not part of the dependencies. So on an already
# built tree, `make S3LVOL_BUILD_TYPE=release` would do nothing, leaving a
# binary that claims to be release but is still made of the old .o files,
# without a word.
#
# This is the same class of problem as the deps/aws stamp (a switch that does
# not take effect), and the fix is the same idea: make the build type a real
# prerequisite. The difference is that the value is baked into the file name --
# switching makes the target not exist, the rule runs, the new stamp is newer
# than every .o, and everything rebuilds. That is one comparison fewer than
# "store a file and diff its content", and it cannot end up with content that
# does not match the name.
#
# Directories add the dependency with one line, `$(OBJS): $(S3LVOL_BUILD_STAMP)`,
# leaving their own rules untouched.
# ---------------------------------------------------------------------------
S3LVOL_BUILD_STAMP := $(S3LVOL_ROOT)/.build-$(s3lvol_bt)

$(S3LVOL_BUILD_STAMP):
	@rm -f $(S3LVOL_ROOT)/.build-*
	@touch $@

# This rule must not steal the default goal.
#
# common.mk is included before each Makefile's own targets, and make takes the
# first target it sees as the default. Up to this point common.mk has only
# variables and functions, no rules, so the default goal has always been each
# Makefile's first one (all at the top level, shared in lib, all in test).
# With the stamp rule above, `make` would become "create the stamp and stop":
# not a single .o built, and because the rule is silent (@), no output at all,
# exit code still 0.
#
# That fooled me once: after changing the compile options I ran make, got no
# error and no output, which looked like "nothing to rebuild" -- but nothing
# had been built at all, and I went hunting for why -DDEBUG did not take
# effect against the old .o files. The lesson is that "no output + exit 0" is
# not the same as "nothing happened"; for a build you have to watch how many
# times cc actually ran.
#
# Clearing .DEFAULT_GOAL makes make fall back to "the first target defined
# afterwards".
.DEFAULT_GOAL :=

# Header dependency tracking.
#
# Not an optimisation -- without it, editing a header rebuilds nothing, and the
# resulting binary mixes translation units compiled against two different
# versions of the same struct. That does not fail to build; it produces a program
# where one file reads a field at an offset another file writes something else to.
#
# It has already happened once: inserting a field into the middle of
# struct s3_lvs_opts moved everything after it, only the edited .c files were
# recompiled, and the stale ones went on reading opts->force from the old offset.
# The symptom was an attach refusing force=true, several layers away from the
# cause.
#
# -MMD writes a .d alongside each .o, listing the headers it included. -MP adds a
# phony target for each of those headers, so deleting or renaming one is a rebuild
# rather than a "No rule to make target" failure. Each directory's Makefile
# -includes the .d files it generates.
COMMON_CFLAGS += -MMD -MP

# ---------------------------------------------------------------------------
# SPDK libraries (expanded via pkg-config, so the dependency order is not
# maintained by hand)
# ---------------------------------------------------------------------------
PKG_CONFIG_PATH := $(SPDK_LIB_DIR)/pkgconfig

SPDK_LIBS := $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                pkg-config --libs spdk_event spdk_event_bdev spdk_bdev_lvol \
                                  spdk_blob spdk_lvol spdk_nvmf 2>/dev/null)
DPDK_LIBS := $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                pkg-config --libs spdk_env_dpdk 2>/dev/null)
SYS_LIBS  := $(shell PKG_CONFIG_PATH="$(PKG_CONFIG_PATH)" \
                pkg-config --libs --static spdk_syslibs 2>/dev/null)

# whole-archive wrapper: keeps the SPDK_RPC_REGISTER /
# SPDK_BDEV_MODULE_REGISTER constructors from being dropped by the linker
# (see docs/s3_bdev.md §12.0.1)
define add_whole_archive
-Wl,--whole-archive $(1) -Wl,--no-whole-archive
endef

# ---------------------------------------------------------------------------
# DPDK: link the archives, not the shared objects
#
# The binaries get released as a package and copied to machines that have no SPDK
# tree, so nothing may be resolved at run time from $(SPDK_ROOT). SPDK's own
# libraries are already static here -- build/lib holds 76 .a and no .so, because
# CONFIG_SHARED=n -- and libaws.a is an archive too. DPDK was the one part still
# linked dynamically, and not by anyone's choice:
#
# pkg-config --libs spdk_env_dpdk returns "-L<dpdk>/lib -lrte_eal -lrte_kvargs
#   ...", that directory holds both librte_eal.a and librte_eal.so, and -l prefers
#   the shared object. The result was 30 librte_*.so.26 dependencies pinned to an
#   absolute path by two -Wl,-rpath flags -- which is why it *worked* here and
#   would have failed on the first machine without this checkout.
#
# So the -lrte_* that pkg-config hands back are rewritten into full paths to the
# .a files, which is the same trick the app Makefile already documents for
# libs3lvol_bdev.a: naming the archive outright is the only way to keep the linker
# from preferring a .so that sits next to it.
#
# === These have to be inside --whole-archive ===
#
# Not for symmetry with SPDK's libraries but for the same reason: DPDK's buses,
# mempool handlers and power management drivers all register themselves from
# RTE_INIT constructors with nothing referencing them by symbol
# (librte_bus_pci.a, librte_mempool_ring.a, librte_power_*.a). A plain static link
# drops those objects, and DPDK's own libdpdk.pc says as much -- its Libs.private
# is one big -Wl,--whole-archive ... -l:librte_*.a list. SPDK does the same thing
# in lib/env_dpdk/env.mk:117 (DPDK_STATIC_LIB_LINKER_ARGS). This is also where the
# app Makefile's old comment was wrong: "DPDK's libraries do not need it, they are
# referenced explicitly" holds for the shared build, where the constructors run at
# dlopen time regardless, and not for this one.
#
# The failure mode is a run-time one, not a link error: EAL comes up and then
# cannot find a bus to probe, or a mempool ops named "ring".
#
# The three private dependencies (-lnuma -ldl, plus rte_eal wanting -lpthread)
# come from spdk_syslibs, which every link here already pulls in.
# ---------------------------------------------------------------------------

# Both take a pkg-config --libs output and split it in two: the -lrte_* become
# archive paths, everything else (the -lspdk_* and the -L flags) is passed through
# untouched, because SPDK's libraries are named as -l on purpose -- there is no .so
# beside them to be confused with.
dpdk_static_libs = $(patsubst -l%,$(DPDK_LIB_DIR)/lib%.a,$(filter -lrte_%,$(1)))
spdk_only_libs   = $(filter-out -lrte_%,$(1))

# ---------------------------------------------------------------------------
# OpenSSL: link the archives, not the shared objects
#
# The Ubuntu 20.04 builder has libssl.so.1.1. Newer distros (Ubuntu 22.04/24.04,
# Debian 12, many EL9 images without compat-openssl11) ship only OpenSSL 3.
# 1.1 and 3.x are different SONAMEs, so a binary that DT_NEEDED libssl.so.1.1
# will not start there. glibc is the other way around (old baseline, new host),
# which is why the builder stays on 20.04; OpenSSL is the library that breaks
# that rule.
#
# -lssl prefers libssl.so sitting next to libssl.a (same class of bug as DPDK
# above). -Wl,-Bstatic -lssl is worse: pkg-config --libs --static spdk_syslibs
# often emits -lssl -lcrypto again, and a later -Bdynamic would pull the .so
# back in -- or -Bstatic would leak onto glibc. Name the .a files outright, and
# drop -lssl/-lcrypto from any syslibs list that still carries them.
#
# libssl.a before libcrypto.a. Keep these *outside* --whole-archive: OpenSSL
# does not register constructors the way SPDK/DPDK do, and wrapping it would
# pull unused ENGINE objects. -ldl -pthread stay dynamic (OpenSSL 1.1 needs
# them).
# ---------------------------------------------------------------------------

OPENSSL_LIBDIR := $(strip $(shell pkg-config --variable=libdir openssl 2>/dev/null))
ifeq ($(OPENSSL_LIBDIR),)
OPENSSL_SSL_A_PROBE := $(shell $(CC) -print-file-name=libssl.a 2>/dev/null)
ifneq ($(filter /%,$(OPENSSL_SSL_A_PROBE)),)
OPENSSL_LIBDIR := $(patsubst %/,%,$(dir $(OPENSSL_SSL_A_PROBE)))
endif
endif
OPENSSL_STATIC_LIBS := $(OPENSSL_LIBDIR)/libssl.a $(OPENSSL_LIBDIR)/libcrypto.a

filter_ssl = $(filter-out -lssl -lcrypto,$(1))
SYS_LIBS := $(call filter_ssl,$(SYS_LIBS))

ifeq ($(filter clean help,$(MAKECMDGOALS)),)
ifeq ($(and $(wildcard $(OPENSSL_LIBDIR)/libssl.a),$(wildcard $(OPENSSL_LIBDIR)/libcrypto.a)),)
$(error No static OpenSSL at $(OPENSSL_LIBDIR) (need libssl.a and libcrypto.a). \
        Install libssl-dev (Debian/Ubuntu) or openssl-devel (RHEL/CentOS). \
        Release s3lvol_tgt links OpenSSL statically so the package does not \
        need libssl.so.1.1 on the target)
endif
endif

# $(call dpdk_link_args,<pkg-config --libs output>) -- the DPDK half, ready to
# link: archives, wrapped as they must be.
#
# Provided as one macro because every caller needs exactly this and the failure
# mode of getting it slightly wrong does not show up at build time. Nine link
# lines each spelling out --whole-archive around a start-group is nine chances for
# one of them to be written without it and still produce a binary that links,
# starts, and then cannot find a mempool ops named "ring".
define dpdk_link_args
-Wl,--whole-archive -Wl,--start-group $(call dpdk_static_libs,$(1)) \
-Wl,--end-group -Wl,--no-whole-archive
endef

# Guard against a DPDK built shared-only. Left alone, the link would fail with
# "cannot find /path/librte_eal.a", which does at least name the file, but not
# what to do about it.
ifeq ($(filter clean help,$(MAKECMDGOALS)),)
ifeq ($(wildcard $(DPDK_LIB_DIR)/librte_eal.a),)
$(error No static DPDK at $(DPDK_LIB_DIR) (librte_eal.a is missing). The \
        release binaries are linked statically; rebuild DPDK with its archives \
        enabled, or build SPDK with its bundled DPDK)
endif
endif

export
