// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
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
    let arguments = Array(CommandLine.arguments.dropFirst())
    let allowedArguments = Set(["--skip-vz-configuration"])
    if let unknown = arguments.first(where: { !allowedArguments.contains($0) }) {
      throw SelfTestError.failed("unknown self-test option: \(unknown)")
    }

    var tests: [(String, () throws -> Void)] = [
      ("manifest round trip", testManifestRoundTrip),
      ("manifest path confinement", testManifestPathConfinement),
      ("HTTP content-length parsing", testHTTPContentLengthParsing),
      ("HTTP request parsing", testHTTPRequestParsing),
      ("data-plane route parsing", testDataPlaneRouteParsing),
      ("reusable MAC address pool", testReusableMACAddressPool),
      ("socket line framing", testSocketLineFraming),
      ("APFS copy-on-write clone", testAPFSClone),
      ("VM directory creation", testVMDirectoryCreation),
      ("template clone", testTemplateClone),
      ("Virtualization.framework configuration", testVMConfiguration),
    ]
    if arguments.contains("--skip-vz-configuration") {
      tests.removeAll { $0.0 == "Virtualization.framework configuration" }
      print("SKIP Virtualization.framework configuration (runner has no nested VZ)")
    }

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
    try requireEqual(actual, expected, "manifest JSON round trip")
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
      try requireEqual(
        try Data(contentsOf: destination),
        Data("cube-vz".utf8),
        "APFS clone contents"
      )

      try Data("changed".utf8).write(to: destination)
      try requireEqual(
        try Data(contentsOf: source),
        Data("cube-vz".utf8),
        "APFS clone source after destination write"
      )
    }
  }

  private static func testHTTPContentLengthParsing() throws {
    try requireEqual(
      try HTTPRequestParser.contentLength(from: "GET /health HTTP/1.1"),
      0,
      "missing Content-Length body size"
    )
    try requireEqual(
      try HTTPRequestParser.contentLength(
        from: "POST /sandboxes HTTP/1.1\r\ncOnTeNt-LeNgTh: 42"
      ),
      42,
      "case-insensitive Content-Length"
    )

    for value in ["", "-1", "invalid", "999999999999999999999999999999"] {
      do {
        _ = try HTTPRequestParser.contentLength(
          from: "POST /sandboxes HTTP/1.1\r\nContent-Length: \(value)"
        )
        throw SelfTestError.failed("accepted invalid Content-Length \(value.debugDescription)")
      } catch is CubeVZError {
        continue
      }
    }

    do {
      _ = try HTTPRequestParser.contentLength(
        from: "POST /sandboxes HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 1"
      )
      throw SelfTestError.failed("accepted duplicate Content-Length headers")
    } catch is CubeVZError {
      // Expected.
    }

    for transferEncoding in ["chunked", "gzip"] {
      do {
        _ = try HTTPRequestParser.contentLength(
          from:
            "POST /sandboxes HTTP/1.1\r\nTransfer-Encoding: \(transferEncoding)"
        )
        throw SelfTestError.failed(
          "accepted unsupported Transfer-Encoding \(transferEncoding.debugDescription)"
        )
      } catch is CubeVZError {
        continue
      }
    }
  }

  private static func testHTTPRequestParsing() throws {
    let body = Data("{\"templateID\":\"cube-vz\"}".utf8)
    let header = Data(
      "POST /sandboxes HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: \(body.count)\r\n\r\n"
        .utf8
    )
    let request = header + body
    try requireEqual(
      try HTTPRequestParser.expectedRequestSize(in: header, maximumBytes: 1_024),
      header.count + body.count,
      "HTTP expected request size"
    )
    let parsed = try HTTPRequestParser.parse(request, maximumBytes: 1_024)
    try requireEqual(parsed.method, "POST", "HTTP request method")
    try requireEqual(parsed.path, "/sandboxes", "HTTP request path")
    try requireEqual(parsed.body, body, "HTTP request body")

    let incomplete = request.dropLast()
    do {
      _ = try HTTPRequestParser.parse(Data(incomplete), maximumBytes: 1_024)
      throw SelfTestError.failed(
        "accepted incomplete HTTP body; actual=\(incomplete.count), expected=\(request.count)"
      )
    } catch is CubeVZError {
      // Expected.
    }

    for requestLine in [
      "POST /sandboxes",
      "POST /sandboxes FTP/1.0",
      "POST /sandboxes HTTP/1.1 extra",
    ] {
      do {
        _ = try HTTPRequestParser.parse(
          Data("\(requestLine)\r\nContent-Length: 0\r\n\r\n".utf8),
          maximumBytes: 1_024
        )
        throw SelfTestError.failed("accepted invalid request line \(requestLine.debugDescription)")
      } catch is CubeVZError {
        continue
      }
    }

    do {
      _ = try HTTPRequestParser.expectedRequestSize(
        in: Data("POST / HTTP/1.1\r\nContent-Length: 1000\r\n\r\n".utf8),
        maximumBytes: 64
      )
      throw SelfTestError.failed("accepted HTTP Content-Length beyond 64-byte request limit")
    } catch is CubeVZError {
      // Expected.
    }
  }

  private static func testDataPlaneRouteParsing() throws {
    let route = try DataPlaneRoute(
      header: Data(
        "GET / HTTP/1.1\r\nHost: 49983-sb-example.cube.local\r\n\r\n".utf8
      )
    )
    try requireEqual(route.port, 49_983, "data-plane guest port")
    try requireEqual(route.sandboxID, "sb-example", "data-plane sandbox ID")

    for header in [
      "GET / HTTP/1.1\r\n\r\n",
      "GET / HTTP/1.1\r\nHost: 0-sb-example.cube.local\r\n\r\n",
      "GET / HTTP/1.1\r\nHost: 49983-.cube.local\r\n\r\n",
      "GET / HTTP/1.1\r\nHost: 49983-sb-a.cube.local\r\nHost: 49983-sb-b.cube.local\r\n\r\n",
    ] {
      do {
        _ = try DataPlaneRoute(header: Data(header.utf8))
        throw SelfTestError.failed("accepted invalid data-plane header \(header.debugDescription)")
      } catch is CubeVZError {
        continue
      }
    }
  }

  private static func testReusableMACAddressPool() throws {
    var pool = ReusableMACAddressPool(capacity: 2)
    pool.recycle("02:00:00:00:00:01")
    pool.recycle("02:00:00:00:00:01")
    try requireEqual(pool.count, 1, "deduplicated reusable MAC count")
    pool.recycle("02:00:00:00:00:02")
    pool.recycle("02:00:00:00:00:03")
    try requireEqual(pool.count, 2, "bounded reusable MAC count")
    try requireEqual(pool.take(), "02:00:00:00:00:03", "most recent reusable MAC")
    try requireEqual(pool.take(), "02:00:00:00:00:02", "oldest retained reusable MAC")
    try requireEqual(pool.take(), nil, "empty reusable MAC pool")
  }

  private static func testSocketLineFraming() throws {
    var descriptors: [Int32] = [-1, -1]
    try require(
      Darwin.socketpair(AF_UNIX, SOCK_STREAM, 0, &descriptors) == 0,
      "cannot create socket pair; errno=\(errno)"
    )
    defer {
      Darwin.close(descriptors[0])
      Darwin.close(descriptors[1])
    }

    let writer = descriptors[1]
    DispatchQueue.global().async {
      _ = Darwin.write(writer, "REA", 3)
      usleep(10_000)
      _ = Darwin.write(writer, "DY init_s=0.1\n", 14)
    }
    let response = try SocketLineReader.readLine(from: descriptors[0])
    try requireEqual(response, "READY init_s=0.1\n", "fragmented socket response")
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
      try requireEqual(manifest.cpuCount, 2, "default vCPU count")
      try requireEqual(manifest.memoryMiB, 2_048, "default memory MiB")
      try require(
        manifest.macAddress != nil,
        "networked VM has no stable MAC address; actual=\(String(describing: manifest.macAddress))"
      )
      try requireEqual(
        manifest.controlPort,
        VMManifest.defaultControlPort,
        "default vsock control port"
      )
      try requireEqual(
        try Data(contentsOf: created.fileURL(named: manifest.diskFile)),
        Data("rootfs".utf8),
        "VM root disk contents"
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
      let secondCloneURL = directory.appendingPathComponent("sandbox-2")
      let clone = try VMTemplateCloner.cloneCold(
        template: template,
        to: cloneURL,
        macAddress: "02:00:00:00:00:02"
      )
      let secondClone = try VMTemplateCloner.cloneCold(
        template: template,
        to: secondCloneURL,
        macAddress: "02:00:00:00:00:03"
      )
      let manifest = try clone.loadManifest()
      let secondManifest = try secondClone.loadManifest()
      try clone.validateFiles(for: manifest)
      try secondClone.validateFiles(for: secondManifest)
      try requireEqual(manifest.macAddress, "02:00:00:00:00:02", "first clone MAC")
      try requireEqual(secondManifest.macAddress, "02:00:00:00:00:03", "second clone MAC")
      let templateMachineID = try Data(contentsOf: template.machineIdentifierURL)
      let cloneMachineID = try Data(contentsOf: clone.machineIdentifierURL)
      let secondCloneMachineID = try Data(contentsOf: secondClone.machineIdentifierURL)
      try require(
        cloneMachineID != templateMachineID,
        "clone reused template machine ID \(templateMachineID.base64EncodedString())"
      )
      try require(
        secondCloneMachineID != templateMachineID && secondCloneMachineID != cloneMachineID,
        "sandbox clone machine IDs are not unique; template=\(templateMachineID.base64EncodedString()), first=\(cloneMachineID.base64EncodedString()), second=\(secondCloneMachineID.base64EncodedString())"
      )
      try requireEqual(
        try Data(contentsOf: clone.fileURL(named: manifest.diskFile)),
        Data("rootfs".utf8),
        "sandbox disk contents"
      )

      try Data("changed".utf8).write(to: clone.fileURL(named: manifest.diskFile))
      try requireEqual(
        try Data(contentsOf: template.fileURL(named: manifest.diskFile)),
        Data("rootfs".utf8),
        "template disk after sandbox write"
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

  private static func requireEqual<T: Equatable>(
    _ actual: @autoclosure () throws -> T,
    _ expected: T,
    _ context: String
  ) throws {
    let actual = try actual()
    guard actual == expected else {
      throw SelfTestError.failed(
        "\(context) mismatch; actual=\(String(reflecting: actual)), expected=\(String(reflecting: expected))"
      )
    }
  }
}
