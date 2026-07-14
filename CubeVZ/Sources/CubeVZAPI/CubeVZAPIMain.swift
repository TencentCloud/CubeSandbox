// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation

private let usage = """
  Usage:
    cube-vz-api --template-dir DIR --sandboxes-dir DIR [options]

  options:
    --template-id ID    CubeAPI template ID (default: cube-vz)
    --bind-address IP   IPv4 listen address (default: 127.0.0.1)
    --port N            Loopback HTTP port (default: 3000)
    --node-id ID        Scheduler node ID (default: cube-vz-local)
    --advertise-url URL URL other nodes use to reach this node
    --workers LIST      Comma-separated nodeID=http://IPv4:port workers
    --coordinator-url   Coordinator URL for periodic worker registration
    --api-key KEY       Static management-plane key (Authorization: Bearer or X-API-Key)
    --auth-callback-url URL
                        Optional callback URL for delegated management auth
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
      let bindAddress = options["--bind-address"] ?? "127.0.0.1"
      let nodeID = options["--node-id"] ?? "cube-vz-local"
      let defaultAdvertiseAddress = bindAddress == "0.0.0.0" ? "127.0.0.1" : bindAddress
      guard
        let advertiseURL = URL(
          string: options["--advertise-url"] ?? "http://\(defaultAdvertiseAddress):\(port)"
        )
      else {
        throw CubeVZError.invalidArguments("--advertise-url is invalid")
      }
      let scheduler = ClusterScheduler(
        localNodeID: nodeID,
        advertiseURL: advertiseURL,
        manager: manager
      )
      for entry in (options["--workers"] ?? "").split(separator: ",") {
        let parts = entry.split(separator: "=", maxSplits: 1)
        guard parts.count == 2 else {
          throw CubeVZError.invalidArguments("--workers entries must be nodeID=http://IPv4:port")
        }
        try scheduler.register(
          ClusterRegistrationRequest(
            nodeID: String(parts[0]),
            url: String(parts[1]),
            activeSandboxes: 0
          )
        )
      }
      let server = HTTPServer(
        bindAddress: bindAddress,
        port: port,
        manager: manager,
        scheduler: scheduler,
        authCallbackURL: try parseOptionalURL(
          options["--auth-callback-url"]
            ?? ProcessInfo.processInfo.environment["CUBE_AUTH_CALLBACK_URL"]
        ),
        apiKey: options["--api-key"]
          ?? ProcessInfo.processInfo.environment["CUBE_API_KEY"]
          ?? ProcessInfo.processInfo.environment["E2B_API_KEY"]
      )
      try server.start()
      print("cube-vz-api: listening on http://\(bindAddress):\(port)")
      print("cube-vz-api: template=\(options["--template-id"] ?? "cube-vz")")
      print("cube-vz-api: node=\(nodeID) advertise=\(advertiseURL.absoluteString)")
      if let coordinator = options["--coordinator-url"] {
        guard let coordinatorURL = URL(string: coordinator) else {
          throw CubeVZError.invalidArguments("--coordinator-url is invalid")
        }
        Task { @MainActor in
          while !Task.isCancelled {
            do {
              try await scheduler.registerWithCoordinator(coordinatorURL)
            } catch {
              FileHandle.standardError.write(
                Data("cube-vz-api: coordinator registration failed: \(error)\n".utf8)
              )
            }
            try? await Task.sleep(for: .seconds(5))
          }
        }
      }
      while !Task.isCancelled {
        try await Task.sleep(for: .seconds(3_600))
      }
    } catch {
      FileHandle.standardError.write(Data("cube-vz-api: \(error)\n\n\(usage)\n".utf8))
      exit(2)
    }
  }

  private static func parseOptions(_ arguments: [String]) throws -> [String: String] {
    let allowed = Set([
      "--template-dir", "--sandboxes-dir", "--template-id", "--bind-address", "--port",
      "--node-id", "--advertise-url", "--workers", "--coordinator-url",
      "--api-key", "--auth-callback-url",
    ])
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

  private static func parseOptionalURL(_ value: String?) throws -> URL? {
    guard let value, !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
      return nil
    }
    guard let url = URL(string: value), url.scheme == "http" || url.scheme == "https",
      url.host != nil
    else {
      throw CubeVZError.invalidArguments("--auth-callback-url must be an http(s) URL")
    }
    return url
  }
}
