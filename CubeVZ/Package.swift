// swift-tools-version: 6.2
// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import PackageDescription

let package = Package(
  name: "CubeVZ",
  platforms: [
    .macOS(.v14)
  ],
  products: [
    .executable(name: "cube-vz", targets: ["CubeVZCLI"]),
    .executable(name: "cube-vz-api", targets: ["CubeVZAPI"]),
    .executable(name: "cube-vz-selftest", targets: ["CubeVZSelfTest"]),
    .library(name: "CubeVZCore", targets: ["CubeVZCore"]),
  ],
  targets: [
    .target(name: "CubeVZCore"),
    .executableTarget(
      name: "CubeVZCLI",
      dependencies: ["CubeVZCore"]
    ),
    .executableTarget(
      name: "CubeVZAPI",
      dependencies: ["CubeVZCore"]
    ),
    .executableTarget(
      name: "CubeVZSelfTest",
      dependencies: ["CubeVZCore"]
    ),
  ]
)
