# Copyright (c) 2021 Intel Corporation
#
# SPDX-License-Identifier: Apache-2.0
#

# It is not necessary to have a build target as this crate is built
# automatically by the consumers of it.
#
# However, it is essential that the crate be tested.
default: test

# It is essential to run these tests using *both* build profiles.
# See the `test_logger_levels()` test for further information.
test:
	@echo "INFO: testing log levels for development build"
	@cargo test
	@echo "INFO: testing log levels for release build"
	@cargo test --release
