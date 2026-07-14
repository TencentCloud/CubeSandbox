// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation
@preconcurrency import Virtualization

public enum ManagedVMStartMode: Sendable {
  case coldBoot
  case restored
}

private final class ControlConnectionDelegate: NSObject, VZVirtioSocketListenerDelegate,
  @unchecked Sendable
{
  private let lock = NSLock()
  private var pending: [VZVirtioSocketConnection] = []

  func listener(
    _ listener: VZVirtioSocketListener,
    shouldAcceptNewConnection connection: VZVirtioSocketConnection,
    from socketDevice: VZVirtioSocketDevice
  ) -> Bool {
    lock.lock()
    pending.append(connection)
    lock.unlock()
    return true
  }

  func takeConnection() -> VZVirtioSocketConnection? {
    lock.lock()
    defer { lock.unlock() }
    guard !pending.isEmpty else { return nil }
    return pending.removeFirst()
  }
}

@MainActor
public final class ManagedVM {
  private let directory: VMDirectory
  private let manifest: VMManifest
  private let virtualMachine: VZVirtualMachine
  private var controlConnection: VZVirtioSocketConnection?
  private var controlConnectionDelegate: ControlConnectionDelegate?
  private var controlListener: VZVirtioSocketListener?

  public init(directory: VMDirectory, manifest: VMManifest) throws {
    self.directory = directory
    self.manifest = manifest
    let configuration = try VMConfigurationBuilder.build(
      directory: directory,
      manifest: manifest,
      consoleInput: nil,
      consoleOutput: nil
    )
    virtualMachine = VZVirtualMachine(configuration: configuration)

    if manifest.vsockEnabled,
      let socketDevice = virtualMachine.socketDevices.first as? VZVirtioSocketDevice
    {
      let delegate = ControlConnectionDelegate()
      let listener = VZVirtioSocketListener()
      listener.delegate = delegate
      socketDevice.setSocketListener(
        listener,
        forPort: manifest.controlPort ?? VMManifest.defaultControlPort
      )
      controlConnectionDelegate = delegate
      controlListener = listener
    }
  }

  public var state: VZVirtualMachine.State {
    virtualMachine.state
  }

  public func start(restoreIfPresent: Bool = true) async throws -> ManagedVMStartMode {
    let stateExists = FileManager.default.fileExists(atPath: directory.stateURL.path)
    if stateExists && restoreIfPresent {
      try await virtualMachine.restoreMachineStateFrom(url: directory.stateURL)
      try await virtualMachine.resume()
      do {
        try FileManager.default.removeItem(at: directory.stateURL)
      } catch {
        try? await virtualMachine.stop()
        throw CubeVZError.filesystem(
          "restored the VM but could not consume \(directory.stateURL.path): \(error)"
        )
      }
      return .restored
    }
    if stateExists {
      throw CubeVZError.runtime(
        "saved state exists at \(directory.stateURL.path); refusing cold boot"
      )
    }
    try await virtualMachine.start()
    return .coldBoot
  }

  public func waitUntilReady(timeout: Duration = .seconds(10)) async throws {
    let clock = ContinuousClock()
    let deadline = clock.now.advanced(by: timeout)
    var lastError = "guest did not open a vsock control connection"

    while clock.now < deadline {
      guard virtualMachine.state == .running else {
        throw CubeVZError.runtime(
          "VM stopped before readiness; state=\(virtualMachine.state.rawValue)"
        )
      }
      if let connection = controlConnectionDelegate?.takeConnection() {
        do {
          let response = try await Self.readResponse(descriptor: connection.fileDescriptor)
          if response == "READY\n" {
            controlConnection = connection
            return
          }
          lastError = "unexpected response \(response.debugDescription)"
        } catch {
          lastError = error.localizedDescription
        }
        connection.close()
      }
      try await Task.sleep(for: .milliseconds(5))
    }
    throw CubeVZError.runtime("guest readiness timed out: \(lastError)")
  }

  public func saveStateAndStop() async throws {
    if virtualMachine.state == .running {
      try await virtualMachine.pause()
    }
    guard virtualMachine.state == .paused else {
      throw CubeVZError.runtime(
        "cannot save VM while state is \(virtualMachine.state.rawValue)"
      )
    }

    let temporaryState = directory.stateURL.appendingPathExtension("partial")
    try? FileManager.default.removeItem(at: temporaryState)
    do {
      try await virtualMachine.saveMachineStateTo(url: temporaryState)
      if FileManager.default.fileExists(atPath: directory.stateURL.path) {
        try FileManager.default.removeItem(at: directory.stateURL)
      }
      try FileManager.default.moveItem(at: temporaryState, to: directory.stateURL)
      try await virtualMachine.stop()
    } catch {
      try? FileManager.default.removeItem(at: temporaryState)
      throw error
    }
  }

  public func shutdown(timeout: Duration = .seconds(2)) async throws {
    guard virtualMachine.state != .stopped else { return }
    if virtualMachine.state == .running, manifest.vsockEnabled {
      _ = try? await exchange(command: "SHUTDOWN\n")
      let clock = ContinuousClock()
      let deadline = clock.now.advanced(by: timeout)
      while clock.now < deadline, virtualMachine.state != .stopped {
        try await Task.sleep(for: .milliseconds(10))
      }
    }
    if virtualMachine.state != .stopped {
      try await virtualMachine.stop()
    }
  }

  private func exchange(command: String) async throws -> String {
    guard let connection = controlConnection else {
      throw CubeVZError.runtime("guest control connection is unavailable")
    }
    let descriptor = connection.fileDescriptor

    return try await Task.detached(priority: .userInitiated) {
      try Self.performExchange(command: command, descriptor: descriptor)
    }.value
  }

  nonisolated private static func readResponse(descriptor: Int32) async throws -> String {
    try await Task.detached(priority: .userInitiated) {
      try readResponseBlocking(descriptor: descriptor)
    }.value
  }

  nonisolated private static func performExchange(
    command: String,
    descriptor: Int32
  ) throws -> String {
    var remaining = Array(command.utf8)[...]
    while !remaining.isEmpty {
      let written = remaining.withUnsafeBytes {
        Darwin.write(descriptor, $0.baseAddress, $0.count)
      }
      guard written > 0 else {
        throw CubeVZError.runtime("vsock write failed: \(String(cString: strerror(errno)))")
      }
      remaining = remaining.dropFirst(written)
    }

    return try readResponseBlocking(descriptor: descriptor)
  }

  nonisolated private static func readResponseBlocking(descriptor: Int32) throws -> String {
    var pollDescriptor = pollfd(fd: descriptor, events: Int16(POLLIN), revents: 0)
    guard Darwin.poll(&pollDescriptor, 1, 2_000) > 0 else {
      throw CubeVZError.runtime("vsock response timed out")
    }
    var buffer = [UInt8](repeating: 0, count: 64)
    let count = buffer.withUnsafeMutableBytes {
      Darwin.read(descriptor, $0.baseAddress, $0.count)
    }
    guard count > 0 else {
      throw CubeVZError.runtime("vsock read failed: \(String(cString: strerror(errno)))")
    }
    return String(decoding: buffer.prefix(count), as: UTF8.self)
  }
}
