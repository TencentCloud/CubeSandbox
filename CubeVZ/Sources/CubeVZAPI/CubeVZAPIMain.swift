// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation

private let usage = """
  Usage:
    cube-vz-api --template-dir DIR --sandboxes-dir DIR [options]

  options:
    --template-id ID    CubeAPI template ID (default: cube-vz)
    --port N            Loopback HTTP port (default: 3000)
  """

@main
private struct CubeVZAPIMain {
  @MainActor
  static func main() async {
    do {
      let options = try parseOptions(Array(CommandLine.arguments.dropFirst()))
      guard let templatePath = options["--template-dir"] else {
        throw CubeVZError.invalidArguments("--template-dir is required")
      }
      guard let sandboxesPath = options["--sandboxes-dir"] else {
        throw CubeVZError.invalidArguments("--sandboxes-dir is required")
      }
      let portString = options["--port"] ?? "3000"
      guard let port = UInt16(portString), port > 0 else {
        throw CubeVZError.invalidArguments("--port must be between 1 and 65535")
      }
      let manager = try SandboxManager(
        templateID: options["--template-id"] ?? "cube-vz",
        templateDirectory: URL(fileURLWithPath: templatePath).standardizedFileURL,
        sandboxesDirectory: URL(fileURLWithPath: sandboxesPath).standardizedFileURL
      )
      let server = HTTPServer(port: port, manager: manager)
      try server.start()
      print("cube-vz-api: listening on http://127.0.0.1:\(port)")
      print("cube-vz-api: template=\(options["--template-id"] ?? "cube-vz")")
      while !Task.isCancelled {
        try await Task.sleep(for: .seconds(3_600))
      }
    } catch {
      FileHandle.standardError.write(Data("cube-vz-api: \(error)\n\n\(usage)\n".utf8))
      exit(2)
    }
  }

  private static func parseOptions(_ arguments: [String]) throws -> [String: String] {
    let allowed = Set(["--template-dir", "--sandboxes-dir", "--template-id", "--port"])
    var result: [String: String] = [:]
    var index = 0
    while index < arguments.count {
      let option = arguments[index]
      guard allowed.contains(option) else {
        throw CubeVZError.invalidArguments("unknown option: \(option)")
      }
      guard index + 1 < arguments.count else {
        throw CubeVZError.invalidArguments("missing value for \(option)")
      }
      result[option] = arguments[index + 1]
      index += 2
    }
    return result
  }
}
