// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation

private enum SelfTestError: Error, CustomStringConvertible {
  case failed(String)

  var description: String {
    switch self {
    case .failed(let message): message
    }
  }
}

@main
private enum CubeVZSelfTest {
  static func main() throws {
    let tests: [(String, () throws -> Void)] = [
      ("manifest round trip", testManifestRoundTrip),
      ("manifest path confinement", testManifestPathConfinement),
      ("APFS copy-on-write clone", testAPFSClone),
      ("VM directory creation", testVMDirectoryCreation),
      ("template clone", testTemplateClone),
      ("Virtualization.framework configuration", testVMConfiguration),
    ]

    for (name, test) in tests {
      try test()
      print("PASS \(name)")
    }
    print("PASS all \(tests.count) cube-vz self-tests")
  }

  private static func testManifestRoundTrip() throws {
    let expected = VMManifest(
      cpuCount: 2,
      memoryMiB: 2_048,
      commandLine: "console=hvc0 root=/dev/vda rw",
      initrdFile: "initrd"
    )
    let data = try JSONEncoder().encode(expected)
    let actual = try JSONDecoder().decode(VMManifest.self, from: data)
    try require(actual == expected, "manifest changed during JSON round trip")
    try actual.validate()
  }

  private static func testManifestPathConfinement() throws {
    for filename in ["../kernel", "/kernel", "nested/kernel", ".", "..", ""] {
      let manifest = VMManifest(
        cpuCount: 2,
        memoryMiB: 2_048,
        commandLine: "console=hvc0",
        kernelFile: filename
      )
      do {
        try manifest.validate()
        throw SelfTestError.failed("manifest accepted unsafe filename \(filename.debugDescription)")
      } catch is CubeVZError {
        continue
      }
    }
  }

  private static func testAPFSClone() throws {
    try withTemporaryDirectory { directory in
      let source = directory.appendingPathComponent("source.raw")
      let destination = directory.appendingPathComponent("destination.raw")
      try Data("cube-vz".utf8).write(to: source)

      try FileCloner.clone(from: source, to: destination, allowFullCopy: false)
      try require(
        try Data(contentsOf: destination) == Data("cube-vz".utf8),
        "cloned file contents differ"
      )

      try Data("changed".utf8).write(to: destination)
      try require(
        try Data(contentsOf: source) == Data("cube-vz".utf8),
        "writing the clone changed the source"
      )
    }
  }

  private static func testVMDirectoryCreation() throws {
    try withTemporaryDirectory { directory in
      let kernel = directory.appendingPathComponent("source-kernel")
      let disk = directory.appendingPathComponent("source-rootfs.raw")
      let destination = directory.appendingPathComponent("sandbox")
      try Data("kernel".utf8).write(to: kernel)
      try Data("rootfs".utf8).write(to: disk)

      let created = try VMDirectoryCreator.create(
        CreateVMRequest(destination: destination, kernel: kernel, disk: disk)
      )
      let manifest = try created.loadManifest()
      try created.validateFiles(for: manifest)
      try require(manifest.cpuCount == 2, "unexpected default vCPU count")
      try require(manifest.memoryMiB == 2_048, "unexpected default memory")
      try require(manifest.macAddress != nil, "networked VM has no stable MAC address")
      try require(
        manifest.controlPort == VMManifest.defaultControlPort,
        "vsock VM has no control port"
      )
      try require(
        try Data(contentsOf: created.fileURL(named: manifest.diskFile))
          == Data("rootfs".utf8),
        "VM root disk contents differ"
      )
    }
  }

  private static func testTemplateClone() throws {
    try withTemporaryDirectory { directory in
      let kernel = directory.appendingPathComponent("source-kernel")
      let disk = directory.appendingPathComponent("source-rootfs.raw")
      let templateURL = directory.appendingPathComponent("template")
      let cloneURL = directory.appendingPathComponent("sandbox")
      try Data("kernel".utf8).write(to: kernel)
      try Data("rootfs".utf8).write(to: disk)

      let template = try VMDirectoryCreator.create(
        CreateVMRequest(destination: templateURL, kernel: kernel, disk: disk)
      )
      let clone = try VMTemplateCloner.cloneCold(
        template: template,
        to: cloneURL,
        macAddress: "02:00:00:00:00:02"
      )
      let manifest = try clone.loadManifest()
      try clone.validateFiles(for: manifest)
      try require(
        try Data(contentsOf: clone.fileURL(named: manifest.diskFile))
          == Data("rootfs".utf8),
        "sandbox disk contents differ"
      )

      try Data("changed".utf8).write(to: clone.fileURL(named: manifest.diskFile))
      try require(
        try Data(contentsOf: template.fileURL(named: manifest.diskFile)) == Data("rootfs".utf8),
        "writing a sandbox disk changed its template"
      )
    }
  }

  private static func testVMConfiguration() throws {
    try withTemporaryDirectory { directory in
      let kernel = directory.appendingPathComponent("source-kernel")
      let disk = directory.appendingPathComponent("source-rootfs.raw")
      let destination = directory.appendingPathComponent("sandbox")
      try Data("kernel".utf8).write(to: kernel)
      try Data(repeating: 0, count: 1_024 * 1_024).write(to: disk)

      let created = try VMDirectoryCreator.create(
        CreateVMRequest(destination: destination, kernel: kernel, disk: disk)
      )
      let manifest = try created.loadManifest()
      _ = try VMConfigurationBuilder.build(
        directory: created,
        manifest: manifest,
        consoleInput: nil,
        consoleOutput: nil
      )
    }
  }

  private static func withTemporaryDirectory(
    _ body: (URL) throws -> Void
  ) throws {
    let manager = FileManager.default
    let directory = manager.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try manager.createDirectory(at: directory, withIntermediateDirectories: true)
    defer { try? manager.removeItem(at: directory) }
    try body(directory)
  }

  private static func require(_ condition: @autoclosure () throws -> Bool, _ message: String) throws
  {
    guard try condition() else {
      throw SelfTestError.failed(message)
    }
  }
}
