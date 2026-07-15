// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

private let usage = """
  Usage:
    cube-vz doctor
    cube-vz create --vm-dir DIR --kernel FILE --disk RAW_FILE [options]
    cube-vz run --vm-dir DIR

  create options:
    --initrd FILE             Optional initial ramdisk
    --cpus N                  vCPU count (default: 2)
    --memory-mib N            Guest memory in MiB (default: 2048)
    --cmdline TEXT            Linux kernel command line
    --no-network              Disable the virtio NAT network device
    --no-vsock                Disable the host/guest virtio socket device
    --allow-full-copy         Fall back to a full disk copy if APFS clonefile fails

  run behavior:
    Boots the VM from its kernel and disk. SIGINT or SIGTERM stops it.
  """

private struct Arguments {
  var values: [String: String] = [:]
  var flags: Set<String> = []

  init(_ raw: ArraySlice<String>) throws {
    var index = raw.startIndex
    while index < raw.endIndex {
      let argument = raw[index]
      guard argument.hasPrefix("--") else {
        throw CubeVZError.invalidArguments("unexpected value: \(argument)")
      }
      if ["--no-network", "--no-vsock", "--allow-full-copy"].contains(argument) {
        flags.insert(argument)
        index = raw.index(after: index)
        continue
      }
      let valueIndex = raw.index(after: index)
      guard valueIndex < raw.endIndex, !raw[valueIndex].hasPrefix("--") else {
        throw CubeVZError.invalidArguments("missing value for \(argument)")
      }
      values[argument] = raw[valueIndex]
      index = raw.index(after: valueIndex)
    }
  }

  func required(_ name: String) throws -> String {
    guard let value = values[name], !value.isEmpty else {
      throw CubeVZError.invalidArguments("\(name) is required")
    }
    return value
  }

  func validate(allowedValues: Set<String>, allowedFlags: Set<String> = []) throws {
    if let unknown = values.keys.first(where: { !allowedValues.contains($0) }) {
      throw CubeVZError.invalidArguments("unknown option: \(unknown)")
    }
    if let unknown = flags.first(where: { !allowedFlags.contains($0) }) {
      throw CubeVZError.invalidArguments("unknown flag: \(unknown)")
    }
  }
}

@main
private struct CubeVZMain {
  @MainActor
  static func main() async {
    do {
      try await execute(Array(CommandLine.arguments.dropFirst()))
    } catch {
      FileHandle.standardError.write(Data("cube-vz: \(error)\n\n\(usage)\n".utf8))
      exit(2)
    }
  }

  @MainActor
  private static func execute(_ arguments: [String]) async throws {
    guard let command = arguments.first else {
      throw CubeVZError.invalidArguments("a command is required")
    }
    let parsed = try Arguments(arguments.dropFirst())

    switch command {
    case "doctor":
      guard arguments.count == 1 else {
        throw CubeVZError.invalidArguments("doctor does not accept options")
      }
      let report = DoctorReport.current()
      print("architecture=\(report.architecture)")
      print("os=\(report.osVersion)")
      print("virtualization_supported=\(report.virtualizationSupported)")
      print("virtualization_entitlement=\(report.virtualizationEntitlement)")
      print("nested_virtualization_supported=\(report.nestedVirtualizationSupported)")
      print("single_layer_ready=\(report.isReady)")
      if !report.isReady {
        throw CubeVZError.unsupported("this Mac cannot run the CubeVZ backend")
      }

    case "create":
      try parsed.validate(
        allowedValues: [
          "--vm-dir", "--kernel", "--disk", "--initrd", "--cpus", "--memory-mib", "--cmdline",
        ],
        allowedFlags: ["--no-network", "--no-vsock", "--allow-full-copy"]
      )
      let memoryMiB = try parseUInt64(parsed.values["--memory-mib"] ?? "2048", "--memory-mib")
      let cpuCount = try parseInt(parsed.values["--cpus"] ?? "2", "--cpus")
      let request = CreateVMRequest(
        destination: fileURL(try parsed.required("--vm-dir")),
        kernel: fileURL(try parsed.required("--kernel")),
        disk: fileURL(try parsed.required("--disk")),
        initrd: parsed.values["--initrd"].map(fileURL),
        cpuCount: cpuCount,
        memoryMiB: memoryMiB,
        commandLine: parsed.values["--cmdline"] ?? "console=hvc0 root=/dev/vda rw",
        networkEnabled: !parsed.flags.contains("--no-network"),
        vsockEnabled: !parsed.flags.contains("--no-vsock"),
        allowFullCopy: parsed.flags.contains("--allow-full-copy")
      )
      let directory = try VMDirectoryCreator.create(request)
      print("created \(directory.url.path)")

    case "run":
      try parsed.validate(allowedValues: ["--vm-dir"])
      let directory = VMDirectory(url: fileURL(try parsed.required("--vm-dir")))
      let manifest = try directory.loadManifest()
      try directory.validateFiles(for: manifest)
      let runtime = try VMRuntime(directory: directory, manifest: manifest)
      try await runtime.run()

    case "help", "--help", "-h":
      print(usage)

    default:
      throw CubeVZError.invalidArguments("unknown command: \(command)")
    }
  }

  private static func fileURL(_ path: String) -> URL {
    URL(fileURLWithPath: path).standardizedFileURL
  }

  private static func parseInt(_ value: String, _ option: String) throws -> Int {
    guard let parsed = Int(value), parsed > 0 else {
      throw CubeVZError.invalidArguments("\(option) must be a positive integer")
    }
    return parsed
  }

  private static func parseUInt64(_ value: String, _ option: String) throws -> UInt64 {
    guard let parsed = UInt64(value), parsed > 0 else {
      throw CubeVZError.invalidArguments("\(option) must be a positive integer")
    }
    return parsed
  }
}
