// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CryptoKit
import CubeVZCore
import Foundation

struct LoginRequest: Decodable {
  let username: String
  let password: String
}

struct ChangePasswordRequest: Decodable {
  let username: String
  let oldPassword: String
  let newPassword: String
}

struct LoginResponse: Encodable {
  let token: String
  let username: String
  let expiresInSecs: Int
}

struct SessionResponse: Encodable {
  let authRequired: Bool
  let authenticated: Bool
  let username: String?
}

/// Durable, local authentication for the bundled WebUI.  CubeAPI normally
/// stores this state in AgentHub's SQL tables; the native macOS backend has no
/// MySQL requirement, so it keeps the equivalent tiny control-plane state in a
/// 0600 JSON file next to its sandbox state.
@MainActor
final class LocalAuthManager {
  private static let sessionLifetimeSeconds = 24 * 60 * 60
  private static let passwordIterations = 120_000

  private struct PasswordRecord: Codable {
    let salt: String
    let digest: String
  }

  private struct SessionRecord: Codable {
    let username: String
    let expiresAt: Date
  }

  private struct State: Codable {
    var users: [String: PasswordRecord]
    var sessions: [String: SessionRecord]
  }

  private let stateURL: URL
  private var state: State

  init(directory: URL, initialPassword: String? = nil) throws {
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    stateURL = directory.appendingPathComponent("auth.json")
    if FileManager.default.fileExists(atPath: stateURL.path) {
      let data = try Data(contentsOf: stateURL)
      state = try JSONDecoder().decode(State.self, from: data)
      removeExpiredSessions()
      persist()
      return
    }

    let password = initialPassword?.trimmingCharacters(in: .whitespacesAndNewlines)
    let adminPassword = password?.isEmpty == false ? password! : "admin"
    state = State(
      users: ["admin": Self.passwordRecord(password: adminPassword)],
      sessions: [:]
    )
    persist()
  }

  func login(_ request: LoginRequest) throws -> LoginResponse {
    let username = request.username.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !username.isEmpty, !request.password.isEmpty else {
      throw CubeVZError.invalidArguments("username and password are required")
    }
    guard let stored = state.users[username], Self.passwordMatches(stored, request.password) else {
      throw CubeVZError.runtime("authentication rejected: invalid credentials")
    }
    removeExpiredSessions()
    let token = Self.randomToken()
    state.sessions[token] = SessionRecord(
      username: username,
      expiresAt: Date().addingTimeInterval(TimeInterval(Self.sessionLifetimeSeconds))
    )
    persist()
    return LoginResponse(
      token: token,
      username: username,
      expiresInSecs: Self.sessionLifetimeSeconds
    )
  }

  func session(token: String?) -> SessionResponse {
    removeExpiredSessions()
    guard let token = token?.trimmingCharacters(in: .whitespacesAndNewlines),
      let record = state.sessions[token], record.expiresAt > Date()
    else {
      persist()
      return SessionResponse(authRequired: true, authenticated: false, username: nil)
    }
    persist()
    return SessionResponse(authRequired: true, authenticated: true, username: record.username)
  }

  func logout(token: String?) {
    guard let token = token?.trimmingCharacters(in: .whitespacesAndNewlines), !token.isEmpty else {
      return
    }
    state.sessions.removeValue(forKey: token)
    persist()
  }

  func changePassword(_ request: ChangePasswordRequest) throws {
    let username = request.username.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !username.isEmpty, !request.oldPassword.isEmpty, !request.newPassword.isEmpty else {
      throw CubeVZError.invalidArguments("username, oldPassword and newPassword are required")
    }
    guard request.newPassword.count >= 4 else {
      throw CubeVZError.invalidArguments("new password must be at least 4 characters")
    }
    guard let stored = state.users[username], Self.passwordMatches(stored, request.oldPassword) else {
      throw CubeVZError.runtime("authentication rejected: current password is incorrect or user not found")
    }
    state.users[username] = Self.passwordRecord(password: request.newPassword)
    // Password rotation invalidates every browser session for the user.
    state.sessions = state.sessions.filter { $0.value.username != username }
    persist()
  }

  private func removeExpiredSessions() {
    let now = Date()
    state.sessions = state.sessions.filter { $0.value.expiresAt > now }
  }

  private func persist() {
    do {
      let encoder = JSONEncoder()
      encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
      let data = try encoder.encode(state)
      try data.write(to: stateURL, options: .atomic)
      try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: stateURL.path)
    } catch {
      FileHandle.standardError.write(
        Data("cube-vz-api: cannot persist local auth state: \(error)\n".utf8)
      )
    }
  }

  private static func passwordRecord(password: String) -> PasswordRecord {
    let salt = randomBytes(count: 32)
    return PasswordRecord(
      salt: salt.base64EncodedString(),
      digest: passwordDigest(password: password, salt: salt).base64EncodedString()
    )
  }

  private static func passwordMatches(_ record: PasswordRecord, _ password: String) -> Bool {
    guard let salt = Data(base64Encoded: record.salt),
      let expected = Data(base64Encoded: record.digest)
    else { return false }
    return constantTimeEqual(expected, passwordDigest(password: password, salt: salt))
  }

  private static func passwordDigest(password: String, salt: Data) -> Data {
    let passwordData = Data(password.utf8)
    var material = salt + passwordData
    for _ in 0..<passwordIterations {
      material = Data(SHA256.hash(data: material + salt + passwordData))
    }
    return material
  }

  private static func constantTimeEqual(_ left: Data, _ right: Data) -> Bool {
    guard left.count == right.count else { return false }
    var difference: UInt8 = 0
    for (lhs, rhs) in zip(left, right) { difference |= lhs ^ rhs }
    return difference == 0
  }

  private static func randomToken() -> String {
    randomBytes(count: 32).base64EncodedString()
      .replacingOccurrences(of: "+", with: "-")
      .replacingOccurrences(of: "/", with: "_")
      .replacingOccurrences(of: "=", with: "")
  }

  private static func randomBytes(count: Int) -> Data {
    var generator = SystemRandomNumberGenerator()
    return Data((0..<count).map { _ in UInt8.random(in: .min ... .max, using: &generator) })
  }
}
