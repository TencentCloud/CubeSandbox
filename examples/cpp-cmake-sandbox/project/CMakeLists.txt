cmake_minimum_required(VERSION 3.16)

project(cube_greeter LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
    set(CMAKE_BUILD_TYPE Release)
endif()

# Static library under test — zero third-party dependencies.
add_library(greeter STATIC src/greeter.cpp)
target_include_directories(greeter PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)

# Demo executable that prints a greeting.
add_executable(app src/main.cpp)
target_link_libraries(app PRIVATE greeter)

# CTest wiring — a single self-contained assertion binary, no GoogleTest.
enable_testing()
add_executable(test_greeter tests/test_greeter.cpp)
target_link_libraries(test_greeter PRIVATE greeter)
add_test(NAME greeter_returns_expected_message COMMAND test_greeter)
