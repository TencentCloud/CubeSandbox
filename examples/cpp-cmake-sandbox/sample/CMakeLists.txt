cmake_minimum_required(VERSION 3.22)

project(cpp_cmake_sandbox VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(greeting src/greeting.cpp)
target_include_directories(greeting PUBLIC include)

add_executable(cpp-cmake-demo src/main.cpp)
target_link_libraries(cpp-cmake-demo PRIVATE greeting)

enable_testing()
add_test(NAME greeting COMMAND cpp-cmake-demo CubeSandbox)
