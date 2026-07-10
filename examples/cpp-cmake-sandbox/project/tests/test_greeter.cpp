// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Minimal CTest test: no GoogleTest, just plain assertions. A non-zero exit
// code marks the test as failed, so anyone can run `ctest` offline.

#include <iostream>
#include <string>

#include "greeter.hpp"

int main() {
    int failures = 0;

    const std::string actual = cube::greet("Cube");
    const std::string expected = "Hello, Cube!";
    if (actual != expected) {
        std::cerr << "FAIL: greet(\"Cube\") == \"" << actual
                  << "\", expected \"" << expected << "\"" << std::endl;
        ++failures;
    }

    if (cube::greet("").empty()) {
        std::cerr << "FAIL: greet(\"\") should not be empty" << std::endl;
        ++failures;
    }

    if (failures == 0) {
        std::cout << "OK: all greeter assertions passed" << std::endl;
        return 0;
    }
    std::cerr << failures << " assertion(s) failed" << std::endl;
    return 1;
}
