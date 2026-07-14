// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation
@preconcurrency import Virtualization

private enum RuntimeEvent: Sendable {
  case snapshotAndExit
  case terminate
  case guestStopped
  case failed(String)
}

private final class VMDelegate: NSObject, VZVirtualMachineDelegate, @unchecked Sendable {
  let stream: AsyncStream<RuntimeEvent>
  private let continuation: AsyncStream<RuntimeEvent>.Continuation

  override init() {
    var captured: AsyncStream<RuntimeEvent>.Continuation?
    stream = AsyncStream { continuation in
      captured = continuation
    }
    continuation = captured!
    super.init()
  }

  func guestDidStop(_ virtualMachine: VZVirtualMachine) {
    continuation.yield(.guestStopped)
  }

  func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
    continuation.yield(.failed(error.localizedDescription))
  }

  deinit {
    continuation.finish()
  }
}

private final class SignalMonitor: @unchecked Sendable {
  let stream: AsyncStream<RuntimeEvent>
  private let continuation: AsyncStream<RuntimeEvent>.Continuation
  private let snapshotSource: DispatchSourceSignal
  private let terminateSource: DispatchSourceSignal
  private let interruptSource: DispatchSourceSignal

  init() {
    signal(SIGUSR1, SIG_IGN)
    signal(SIGTERM, SIG_IGN)
    signal(SIGINT, SIG_IGN)

    var captured: AsyncStream<RuntimeEvent>.Continuation?
    stream = AsyncStream { continuation in
      captured = continuation
    }
    continuation = captured!

    snapshotSource = DispatchSource.makeSignalSource(signal: SIGUSR1, queue: .main)
    terminateSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
    interruptSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)

    snapshotSource.setEventHandler { [continuation] in
      continuation.yield(.snapshotAndExit)
    }
    terminateSource.setEventHandler { [continuation] in
      continuation.yield(.terminate)
    }
    interruptSource.setEventHandler { [continuation] in
      continuation.yield(.terminate)
    }
    snapshotSource.resume()
    terminateSource.resume()
    interruptSource.resume()
  }

  deinit {
    snapshotSource.cancel()
    terminateSource.cancel()
    interruptSource.cancel()
    continuation.finish()
  }
}

@MainActor
public final class VMRuntime {
  private let directory: VMDirectory
  private let virtualMachine: VZVirtualMachine
  private let delegate: VMDelegate
  private let signals: SignalMonitor

  public init(directory: VMDirectory, manifest: VMManifest) throws {
    self.directory = directory
    let configuration = try VMConfigurationBuilder.build(
      directory: directory,
      manifest: manifest
    )
    delegate = VMDelegate()
    signals = SignalMonitor()
    virtualMachine = VZVirtualMachine(configuration: configuration)
    virtualMachine.delegate = delegate
  }

  public func run(restoreIfPresent: Bool = true) async throws {
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
      FileHandle.standardError.write(
        Data("cube-vz: restored \(directory.stateURL.path)\n".utf8)
      )
    } else if stateExists {
      throw CubeVZError.runtime(
        "saved state exists at \(directory.stateURL.path); run reset-state before a cold boot"
      )
    } else {
      try await virtualMachine.start()
      FileHandle.standardError.write(Data("cube-vz: cold boot started\n".utf8))
    }

    FileHandle.standardError.write(
      Data(
        "cube-vz: pid \(ProcessInfo.processInfo.processIdentifier); "
          .appending("send SIGUSR1 to save state and exit\n").utf8
      )
    )

    let event = await waitForEvent()
    switch event {
    case .snapshotAndExit:
      try await snapshotAndStop()
    case .terminate:
      try await stopWithoutSnapshot()
    case .guestStopped:
      return
    case .failed(let message):
      throw CubeVZError.runtime(message)
    }
  }

  private func waitForEvent() async -> RuntimeEvent {
    await withTaskGroup(of: RuntimeEvent.self) { group in
      group.addTask { [signals] in
        for await event in signals.stream {
          return event
        }
        return .terminate
      }
      group.addTask { [delegate] in
        for await event in delegate.stream {
          return event
        }
        return .guestStopped
      }
      let event = await group.next() ?? .terminate
      group.cancelAll()
      return event
    }
  }

  private func snapshotAndStop() async throws {
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
    FileHandle.standardError.write(
      Data("cube-vz: saved \(directory.stateURL.path)\n".utf8)
    )
  }

  private func stopWithoutSnapshot() async throws {
    guard virtualMachine.state != .stopped else { return }
    try await virtualMachine.stop()
  }
}
