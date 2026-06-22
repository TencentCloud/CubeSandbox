// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Secret protection for data stored in the AgentHub database.
//
//  - WebUI passwords are hashed one-way with bcrypt.
//  - Reversible secrets that must be used later (the DeepSeek API key and the
//    WeCom bot secret) are encrypted with AES-256-GCM and stored as
//    `enc:v1:<base64(nonce|ciphertext)>`.
//
// The symmetric key (the "master key") is generated once per installation and
// persisted in the AgentHub database (`t_agenthub_setting`, key
// `secret_master_key`). CubeAPI bootstraps it on first startup (see
// `db::AgentHubStore::connect`) and caches the decoded bytes in process memory,
// so the runtime encrypt/decrypt path is a pure in-memory read with no database
// round-trip. There is no environment variable and no built-in fallback key:
// secrets cannot be encrypted or decrypted until the master key is installed.

use std::sync::OnceLock;

use aes_gcm::{
    aead::{Aead, KeyInit},
    Aes256Gcm, Key, Nonce,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};

const ENC_PREFIX: &str = "enc:v1:";
const NONCE_LEN: usize = 12;
const MASTER_KEY_LEN: usize = 32;

/// Process-global master key, installed once at startup from the database.
static MASTER_KEY: OnceLock<[u8; MASTER_KEY_LEN]> = OnceLock::new();

/// Generates a fresh random master key encoded as base64, suitable for storing
/// in the database and later installing with [`install_master_key`].
pub fn generate_master_key_b64() -> String {
    let mut bytes = [0u8; MASTER_KEY_LEN];
    getrandom::getrandom(&mut bytes).expect("OS CSPRNG must be available");
    BASE64.encode(bytes)
}

/// Installs the process-wide master key from its base64 representation.
///
/// Idempotent: installing the same key again (e.g. from tests) is a no-op. The
/// value must decode to exactly 32 bytes.
pub fn install_master_key(b64: &str) -> anyhow::Result<()> {
    let bytes = BASE64
        .decode(b64.trim())
        .map_err(|e| anyhow::anyhow!("master key is not valid base64: {e}"))?;
    let key: [u8; MASTER_KEY_LEN] = bytes
        .as_slice()
        .try_into()
        .map_err(|_| anyhow::anyhow!("master key must be exactly {MASTER_KEY_LEN} bytes"))?;
    // The key is installed once per process lifetime. OnceLock::set returns
    // Err if already initialised — that is normal (e.g. tests calling it
    // multiple times). Log only when the *value differs*, as that signals a
    // real misconfiguration (two sources disagree on the master key).
    if let Err(_) = MASTER_KEY.set(key) {
        if MASTER_KEY.get() != Some(&key) {
            tracing::debug!("ignoring attempt to install a different AgentHub master key");
        }
    }
    Ok(())
}

/// Returns the installed master key, or an error when it has not been
/// bootstrapped yet (no database configured / startup init not run).
fn load_key() -> anyhow::Result<[u8; MASTER_KEY_LEN]> {
    MASTER_KEY
        .get()
        .copied()
        .ok_or_else(|| anyhow::anyhow!("AgentHub master key is not initialized"))
}

/// Encrypts a UTF-8 secret, returning an `enc:v1:` tagged, base64 payload.
pub fn encrypt_secret(plaintext: &str) -> anyhow::Result<String> {
    let key = load_key()?;
    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(&key));
    // 96-bit nonce from a fresh UUIDv4 (random) — unique per message.
    let uuid_bytes = *uuid::Uuid::new_v4().as_bytes();
    let nonce = Nonce::from_slice(&uuid_bytes[..NONCE_LEN]);
    let ciphertext = cipher
        .encrypt(nonce, plaintext.as_bytes())
        .map_err(|e| anyhow::anyhow!("encrypt failed: {e}"))?;
    let mut payload = Vec::with_capacity(NONCE_LEN + ciphertext.len());
    payload.extend_from_slice(&uuid_bytes[..NONCE_LEN]);
    payload.extend_from_slice(&ciphertext);
    Ok(format!("{ENC_PREFIX}{}", BASE64.encode(payload)))
}

/// Decrypts an `enc:v1:` payload produced by [`encrypt_secret`].
pub fn decrypt_secret(stored: &str) -> anyhow::Result<String> {
    let encoded = stored
        .strip_prefix(ENC_PREFIX)
        .ok_or_else(|| anyhow::anyhow!("value is not encrypted"))?;
    let raw = BASE64.decode(encoded)?;
    if raw.len() <= NONCE_LEN {
        anyhow::bail!("ciphertext too short");
    }
    let (nonce_bytes, ciphertext) = raw.split_at(NONCE_LEN);
    let key = load_key()?;
    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(&key));
    let plaintext = cipher
        .decrypt(Nonce::from_slice(nonce_bytes), ciphertext)
        .map_err(|e| anyhow::anyhow!("decrypt failed: {e}"))?;
    Ok(String::from_utf8(plaintext)?)
}

/// Returns whether a stored value is in the encrypted envelope format.
pub fn is_encrypted(stored: &str) -> bool {
    stored.starts_with(ENC_PREFIX)
}

/// Decrypts a stored secret for use.
///
/// - Values without the `enc:v1:` envelope are legacy plaintext rows and are
///   returned unchanged.
/// - Encrypted values that decrypt successfully return their plaintext.
/// - Encrypted values that CANNOT be decrypted (e.g. encrypted under a previous
///   master key) are treated as **unset** and return an empty string, rather
///   than leaking the ciphertext downstream as if it were a valid secret. This
///   makes callers (which check for emptiness) report "not configured" so the
///   operator is prompted to re-enter the secret instead of hitting a silent
///   authentication failure later.
pub fn decrypt_or_passthrough(stored: &str) -> String {
    if is_encrypted(stored) {
        decrypt_secret(stored).unwrap_or_else(|err| {
            tracing::warn!(error = %err, "failed to decrypt AgentHub secret; treating as unset");
            String::new()
        })
    } else {
        stored.to_string()
    }
}

/// Hashes a password with bcrypt.
pub fn hash_password(password: &str) -> anyhow::Result<String> {
    bcrypt::hash(password, bcrypt::DEFAULT_COST).map_err(|e| anyhow::anyhow!("hash failed: {e}"))
}

/// Verifies a candidate password against a stored value. bcrypt hashes start
/// with `$2`; anything else is treated as a legacy plaintext password.
pub fn verify_password(stored: &str, candidate: &str) -> bool {
    if stored.starts_with("$2") {
        bcrypt::verify(candidate, stored).unwrap_or(false)
    } else {
        stored == candidate
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Installs a fixed 32-byte test key. All tests share the same value so the
    /// idempotent `OnceLock` install is order-independent across parallel tests.
    fn install_test_key() {
        const TEST_KEY: [u8; MASTER_KEY_LEN] = *b"0123456789abcdef0123456789ABCDEF";
        install_master_key(&BASE64.encode(TEST_KEY)).expect("install test key");
    }

    #[test]
    fn encrypt_decrypt_roundtrip() {
        install_test_key();

        let encrypted = encrypt_secret("wecom-secret").expect("encrypt should succeed");

        assert!(is_encrypted(&encrypted));
        assert_ne!(encrypted, "wecom-secret");
        assert_eq!(
            decrypt_secret(&encrypted).expect("decrypt should succeed"),
            "wecom-secret"
        );
    }

    #[test]
    fn decrypt_or_passthrough_handles_plaintext_and_bad_ciphertext() {
        install_test_key();

        // Legacy plaintext (no envelope) passes through unchanged.
        assert_eq!(decrypt_or_passthrough("legacy-secret"), "legacy-secret");
        // Encrypted envelope with non-base64 body cannot be decrypted -> unset.
        assert_eq!(decrypt_or_passthrough("enc:v1:not-base64"), "");
        // Well-formed envelope that does not authenticate (e.g. encrypted under a
        // different/previous master key) also fails closed to unset, instead of
        // leaking the ciphertext string downstream.
        let undecryptable = format!("enc:v1:{}", BASE64.encode([0u8; 32]));
        assert_eq!(decrypt_or_passthrough(&undecryptable), "");
    }

    #[test]
    fn install_master_key_rejects_wrong_length() {
        assert!(install_master_key(&BASE64.encode(b"too-short")).is_err());
        assert!(install_master_key("not-base64!!!").is_err());
    }

    #[test]
    fn password_hash_verification_supports_bcrypt_and_legacy_plaintext() {
        let hashed = hash_password("correct horse").expect("hash should succeed");

        assert!(verify_password(&hashed, "correct horse"));
        assert!(!verify_password(&hashed, "wrong horse"));
        assert!(verify_password("legacy-password", "legacy-password"));
        assert!(!verify_password("legacy-password", "wrong"));
    }
}
