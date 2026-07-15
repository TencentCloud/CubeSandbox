// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation
@preconcurrency import Virtualization

public final class VMStreamConnection: @unchecked Sendable {
  private let connection: VZVirtioSocketConnection

  fileprivate init(connection: VZVirtioSocketConnection) {
    self.connection = connection
  }

  public var fileDescriptor: Int32 {
    connection.fileDescriptor
  }

  public func close() {
    connection.close()
  }
}

private final class PendingControlConnection: @unchecked Sendable {
  let connection: VZVirtioSocketConnection

  init(_ connection: VZVirtioSocketConnection) {
    self.connection = connection
  }
}

private enum ControlWaitEvent: Sendable {
  case connection(PendingControlConnection)
  case timedOut
  case streamClosed
  case cancelled
}

private final class ControlConnectionDelegate: NSObject, VZVirtioSocketListenerDelegate,
  @unchecked Sendable
{
  private let lock = NSLock()
  private let stream: AsyncStream<PendingControlConnection>
  private let continuation: AsyncStream<PendingControlConnection>.Continuation
  private var accepting = true
  private var pending: PendingControlConnection?

  override init() {
    let pair = AsyncStream.makeStream(
      of: PendingControlConnection.self,
      bufferingPolicy: .bufferingNewest(1)
    )
    stream = pair.stream
    continuation = pair.continuation
    super.init()
  }

  func listener(
    _ listener: VZVirtioSocketListener,
    shouldAcceptNewConnection connection: VZVirtioSocketConnection,
    from socketDevice: VZVirtioSocketDevice
  ) -> Bool {
    lock.lock()
    guard accepting, pending == nil else {
      lock.unlock()
      return false
    }
    accepting = false
    let pending = PendingControlConnection(connection)
    self.pending = pending
    lock.unlock()
    continuation.yield(pending)
    continuation.finish()
    return true
  }

  func nextConnection(timeout: Duration) async throws -> VZVirtioSocketConnection {
    let event = await withTaskGroup(of: ControlWaitEvent.self) { group in
      group.addTask { [stream] in
        for await connection in stream {
          return .connection(connection)
        }
        return .streamClosed
      }
      group.addTask {
        do {
          try await Task.sleep(for: timeout)
          return .timedOut
        } catch {
          return .cancelled
        }
      }
      let first = await group.next() ?? .streamClosed
      group.cancelAll()
      return first
    }

    switch event {
    case .connection(let pending):
      lock.withLock {
        if self.pending === pending {
          self.pending = nil
        }
      }
      return pending.connection
    case .timedOut:
      stopAccepting()
      throw CubeVZError.runtime("guest readiness timed out")
    case .streamClosed:
      throw CubeVZError.runtime("guest control listener stopped before readiness")
    case .cancelled:
      throw CancellationError()
    }
  }

  func stopAccepting() {
    lock.lock()
    accepting = false
    let connection = pending
    pending = nil
    lock.unlock()
    continuation.finish()
    connection?.connection.close()
  }

  deinit {
    continuation.finish()
  }
}

@MainActor
public final class ManagedVM {
  private let manifest: VMManifest
  private let virtualMachine: VZVirtualMachine
  private var controlConnection: VZVirtioSocketConnection?
  private var controlConnectionDelegate: ControlConnectionDelegate?
  private var controlListener: VZVirtioSocketListener?
  public private(set) var readinessMetadata: String?

  public init(
    directory: VMDirectory,
    manifest: VMManifest
  ) throws {
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

  public func start() async throws {
    try await virtualMachine.start()
  }

  public func waitUntilReady(timeout: Duration = .seconds(10)) async throws {
    guard virtualMachine.state == .running else {
      throw CubeVZError.runtime(
        "VM stopped before readiness; state=\(virtualMachine.state.rawValue)"
      )
    }
    guard let delegate = controlConnectionDelegate else {
      throw CubeVZError.runtime("VM has no control vsock listener")
    }

    let connection = try await delegate.nextConnection(timeout: timeout)
    do {
      let response = try await Self.readResponse(descriptor: connection.fileDescriptor)
      guard response == "READY\n" || response.hasPrefix("READY ") else {
        throw CubeVZError.runtime(
          "unexpected guest readiness response: \(response.debugDescription)"
        )
      }
      delegate.stopAccepting()
      readinessMetadata =
        response
        .trimmingCharacters(in: .whitespacesAndNewlines)
        .dropFirst("READY".count)
        .trimmingCharacters(in: .whitespaces)
      controlConnection = connection
    } catch {
      connection.close()
      delegate.stopAccepting()
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

  public func discard() async throws {
    guard virtualMachine.state != .stopped else { return }
    try await virtualMachine.stop()
  }

  public func connect(toGuestPort port: UInt32) async throws -> VMStreamConnection {
    guard virtualMachine.state == .running else {
      throw CubeVZError.runtime("cannot connect to guest while VM is not running")
    }
    guard let socket = virtualMachine.socketDevices.first as? VZVirtioSocketDevice else {
      throw CubeVZError.runtime("VM has no virtio socket device")
    }
    return VMStreamConnection(connection: try await socket.connect(toPort: port))
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
      var written: Int
      repeat {
        written = remaining.withUnsafeBytes {
          Darwin.write(descriptor, $0.baseAddress, $0.count)
        }
      } while written < 0 && errno == EINTR
      guard written > 0 else {
        throw CubeVZError.runtime("vsock write failed: \(String(cString: strerror(errno)))")
      }
      remaining = remaining.dropFirst(written)
    }

    return try readResponseBlocking(descriptor: descriptor)
  }

  nonisolated private static func readResponseBlocking(descriptor: Int32) throws -> String {
    try SocketLineReader.readLine(from: descriptor)
  }
}
