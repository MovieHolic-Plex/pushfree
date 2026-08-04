//! End-to-end-encryption field decryption (plan todo 44).
//!
//! Reverses the server's opaque field format (defined in
//! `server/internal/e2ee/e2ee.go`, todo 43 — the wire-format source of truth):
//!
//! ```text
//! gzip(plaintext)
//!   -> AES-256-CBC (random 16-byte IV, PKCS7 padding)
//!   -> HMAC-SHA256(key, IV || ciphertext)
//!   -> base64( IV || ciphertext || hmac )
//! ```
//!
//! The 256-bit key is a 64-character hex string provisioned out-of-band; the
//! server never receives it and never decrypts. Each of message/title/url/
//! url_title is encrypted independently with its own fresh IV + HMAC, so each
//! field is decrypted independently here.
//!
//! # Failure policy (research EB/W2-E2EE; plan todo 44)
//! A wrong key, a tampered HMAC, bad padding, or a corrupt gzip stream MUST
//! surface as a safe placeholder — never a panic, never ciphertext, never
//! garbage plaintext. This mirrors the Go reference's padding-oracle hygiene:
//! every structural failure collapses into [`E2eeError::InvalidBlob`] with no
//! detail distinguishing a MAC failure from a padding failure. The MAC is
//! verified BEFORE any CBC / PKCS7 work (encrypt-then-MAC).
//!
//! # Why manual CBC + PKCS7 (no `cbc` crate)
//! The `aes` block cipher is chained by hand so the CBC XOR + PKCS7 logic is
//! byte-faithful to the Go reference and the dependency set stays minimal.

use std::io::Read;

use aes::cipher::generic_array::GenericArray;
use aes::cipher::{BlockDecrypt, KeyInit};
use aes::Aes256;
use base64::Engine;
use flate2::read::GzDecoder;
use hmac::{Hmac, Mac};
use sha2::Sha256;

use crate::ws::ServerMessage;

/// HMAC-SHA256 keyed over the AES key, matching the upstream scheme.
type HmacSha256 = Hmac<Sha256>;

// Layout constants — must match the Go reference exactly. Exported so callers
// (and tests) can reason about the byte layout without magic numbers.
pub const KEY_LEN: usize = 32;
pub const KEY_HEX_LEN: usize = 64;
pub const IV_LEN: usize = 16;
pub const HMAC_LEN: usize = 32;
const BLOCK_LEN: usize = 16;
/// Smallest legal raw blob: IV + exactly one AES block + HMAC.
const MIN_BLOB_LEN: usize = IV_LEN + BLOCK_LEN + HMAC_LEN;

/// Placeholder shown for an encrypted field that cannot be decrypted. Never the
/// ciphertext, never partially-decrypted garbage.
pub const DECRYPT_FAILED_PLACEHOLDER: &str = "[undecryptable]";

/// A decryption failure. Distinct variants exist only for length-class (which
/// is observable from the field anyway) and key-format problems; every MAC /
/// padding / gzip failure collapses into [`E2eeError::InvalidBlob`] to avoid
/// leaking a padding oracle on the legacy CBC construction.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum E2eeError {
    /// The key is not exactly 64 hex characters (32 bytes).
    InvalidKey,
    /// The blob is too short to hold IV + one block + HMAC.
    ShortBlob,
    /// Base64 / structural / MAC / padding / gzip failure (deliberately fused).
    InvalidBlob,
}

impl std::fmt::Display for E2eeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            E2eeError::InvalidKey => write!(f, "e2ee: key must be 64 hex chars (32 bytes)"),
            E2eeError::ShortBlob => write!(f, "e2ee: encrypted blob is too short"),
            E2eeError::InvalidBlob => write!(f, "e2ee: invalid or tampered encrypted blob"),
        }
    }
}

impl std::error::Error for E2eeError {}

/// Decode the 64-character hex form of an E2EE key into 32 raw bytes.
pub fn parse_key(hex_key: &str) -> Result<[u8; KEY_LEN], E2eeError> {
    if hex_key.len() != KEY_HEX_LEN {
        return Err(E2eeError::InvalidKey);
    }
    let bytes = hex::decode(hex_key).map_err(|_| E2eeError::InvalidKey)?;
    let mut out = [0u8; KEY_LEN];
    if bytes.len() != KEY_LEN {
        return Err(E2eeError::InvalidKey);
    }
    out.copy_from_slice(&bytes);
    Ok(out)
}

/// Decrypt a single base64 field blob under a 32-byte key. See the module docs
/// for the verify-then-decrypt ordering and the error-collapse policy.
pub fn decrypt(key: &[u8], blob: &str) -> Result<Vec<u8>, E2eeError> {
    if key.len() != KEY_LEN {
        return Err(E2eeError::InvalidKey);
    }
    let raw = base64::engine::general_purpose::STANDARD
        .decode(blob)
        .map_err(|_| E2eeError::InvalidBlob)?;
    if raw.len() < MIN_BLOB_LEN {
        return Err(E2eeError::ShortBlob);
    }
    let ct_len = raw.len() - IV_LEN - HMAC_LEN;
    if ct_len == 0 || ct_len % BLOCK_LEN != 0 {
        return Err(E2eeError::ShortBlob);
    }
    let mac_off = raw.len() - HMAC_LEN;
    let iv = &raw[..IV_LEN];
    let ct = &raw[IV_LEN..mac_off];
    let mac = &raw[mac_off..];

    // Verify-then-decrypt. Mac::verify_slice is constant-time.
    let mut verifier =
        <HmacSha256 as Mac>::new_from_slice(key).map_err(|_| E2eeError::InvalidKey)?;
    verifier.update(&raw[..mac_off]);
    verifier
        .verify_slice(mac)
        .map_err(|_| E2eeError::InvalidBlob)?;

    let cipher = Aes256::new_from_slice(key).map_err(|_| E2eeError::InvalidKey)?;
    let padded = cbc_decrypt(&cipher, iv, ct);

    // PKCS7 unpad with full validation. Any anomaly -> InvalidBlob (never a
    // distinct padding error).
    let pad_len = *padded.last().unwrap_or(&0) as usize;
    if pad_len == 0 || pad_len > BLOCK_LEN {
        return Err(E2eeError::InvalidBlob);
    }
    let pad_start = padded.len() - pad_len;
    if padded[pad_start..].iter().any(|b| *b as usize != pad_len) {
        return Err(E2eeError::InvalidBlob);
    }

    let compressed = &padded[..pad_start];
    let mut dec = GzDecoder::new(compressed);
    let mut out = Vec::new();
    dec.read_to_end(&mut out)
        .map_err(|_| E2eeError::InvalidBlob)?;
    Ok(out)
}

/// String-in/string-out convenience around [`decrypt`].
pub fn decrypt_field(hex_key: &str, blob: &str) -> Result<String, E2eeError> {
    let key = parse_key(hex_key)?;
    let pt = decrypt(&key, blob)?;
    String::from_utf8(pt).map_err(|_| E2eeError::InvalidBlob)
}

/// AES-256-CBC decryption with explicit IV chaining. Each ciphertext block is
/// decrypted with the raw AES block cipher (ECB on one block), then XORed with
/// the previous CIPHERTEXT block (or the IV for the first block).
fn cbc_decrypt(cipher: &Aes256, iv: &[u8], ct: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(ct.len());
    let mut prev: [u8; BLOCK_LEN] = [0u8; BLOCK_LEN];
    prev.copy_from_slice(iv);
    for chunk in ct.chunks_exact(BLOCK_LEN) {
        let mut block = GenericArray::clone_from_slice(chunk);
        cipher.decrypt_block(&mut block);
        let mut plain = [0u8; BLOCK_LEN];
        for i in 0..BLOCK_LEN {
            plain[i] = block[i] ^ prev[i];
        }
        out.extend_from_slice(&plain);
        prev.copy_from_slice(chunk);
    }
    out
}

/// Decrypt the encryptable string fields of a server message into display
/// plaintext, applying the failure policy at message granularity.
///
/// - `encrypted == false` -> the message is already plaintext; returned
///   unchanged (the most common path, so it short-circuits cheaply).
/// - `encrypted == true` -> each non-empty text field is decrypted under the
///   supplied key. An empty field stays empty (it carried no ciphertext). Any
///   failure (missing/invalid key, MAC mismatch, corruption) replaces the
///   field with [`DECRYPT_FAILED_PLACEHOLDER`]; the message is still delivered
///   — never dropped — but the UI shows a clear error state, never ciphertext.
/// - The returned message has `encrypted` cleared so downstream code never
///   re-attempts decryption or mistakes a placeholder for ciphertext.
pub fn decrypt_message(msg: &ServerMessage, hex_key: Option<&str>) -> ServerMessage {
    if !msg.encrypted {
        return msg.clone();
    }
    // Resolve a usable key. A missing OR malformed key both collapse to "no
    // usable key" -> every field placeholders (error state, no crash).
    let key = hex_key.and_then(|k| parse_key(k).ok());

    let mut out = msg.clone();
    let decrypt_str = |blob: &str| -> String {
        if blob.is_empty() {
            return String::new();
        }
        match &key {
            Some(k) => match decrypt(k, blob) {
                Ok(pt) => String::from_utf8_lossy(&pt).into_owned(),
                Err(_) => DECRYPT_FAILED_PLACEHOLDER.to_string(),
            },
            None => DECRYPT_FAILED_PLACEHOLDER.to_string(),
        }
    };
    out.title = decrypt_str(&msg.title);
    out.message = decrypt_str(&msg.message);
    out.url = decrypt_str(&msg.url);
    out.url_title = decrypt_str(&msg.url_title);
    out.encrypted = false;
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    use serde::Deserialize;
    use std::path::PathBuf;

    // ---- shared fixture consumption ---------------------------------------

    /// One entry of the shared JSON fixture committed at
    /// `server/internal/e2ee/testdata/e2ee_vectors.json` (the single source of
    /// truth, also consumed by the Go and Android suites).
    #[derive(Debug, Deserialize)]
    struct VectorCase {
        name: String,
        key_hex: String,
        plaintext: String,
        blob: String,
        #[serde(default)]
        expect_error: bool,
    }

    #[derive(Debug, Deserialize)]
    struct VectorFile {
        vectors: Vec<VectorCase>,
    }

    /// Locate the shared fixture by walking up from this crate's manifest dir
    /// (CARGO_MANIFEST_DIR = .../desktop) looking for
    /// `server/internal/e2ee/testdata/e2ee_vectors.json`. Robust to the test
    /// being invoked from any working directory.
    fn fixture_path() -> PathBuf {
        let mut dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
        loop {
            let candidate = dir.join("server/internal/e2ee/testdata/e2ee_vectors.json");
            if candidate.exists() {
                return candidate;
            }
            if !dir.pop() {
                break;
            }
        }
        // Fallback: relative to the desktop crate's working dir.
        PathBuf::from("../server/internal/e2ee/testdata/e2ee_vectors.json")
    }

    fn load_vectors() -> Vec<VectorCase> {
        let path = fixture_path();
        let bytes = std::fs::read(&path)
            .unwrap_or_else(|e| panic!("read shared fixture {}: {e}", path.display()));
        let file: VectorFile = serde_json::from_slice(&bytes)
            .unwrap_or_else(|e| panic!("parse shared fixture {}: {e}", path.display()));
        assert!(!file.vectors.is_empty(), "fixture has no vectors");
        file.vectors
    }

    /// THE cross-platform vector test: the SAME fixture that the Go and Android
    /// suites consume must pass here. Positive cases decrypt to the expected
    /// plaintext; negative cases (wrong key, tampered HMAC) must error.
    #[test]
    fn shared_vectors_match_go_and_android() {
        let vectors = load_vectors();
        for v in &vectors {
            let result = decrypt_field(&v.key_hex, &v.blob);
            if v.expect_error {
                assert!(
                    result.is_err(),
                    "{}: expected error but got {:?}",
                    v.name,
                    result
                );
            } else {
                let pt = result.unwrap_or_else(|e| panic!("{}: decrypt failed: {e}", v.name));
                assert_eq!(pt, v.plaintext, "{}: plaintext mismatch", v.name);
            }
        }
        // Raw output line for evidence / misleading-success check: print every
        // decrypted plaintext so a human can eyeball that the bytes are real.
        eprintln!("[e2ee/vector-file] {}", fixture_path().display());
        for v in &vectors {
            if !v.expect_error {
                eprintln!("[e2ee/decrypt] {} -> {:?}", v.name, v.plaintext);
            } else {
                eprintln!("[e2ee/decrypt] {} -> (error, as expected)", v.name);
            }
        }
    }

    // ---- failure policy ---------------------------------------------------

    #[test]
    fn invalid_key_length_rejected() {
        assert_eq!(parse_key("toolong"), Err(E2eeError::InvalidKey));
        assert_eq!(parse_key(""), Err(E2eeError::InvalidKey));
        // 63 hex chars (one short)
        assert_eq!(parse_key(&"a".repeat(63)), Err(E2eeError::InvalidKey));
        // non-hex chars at the right length
        assert_eq!(parse_key(&"z".repeat(64)), Err(E2eeError::InvalidKey));
    }

    #[test]
    fn decrypt_rejects_wrong_key_size() {
        let vectors = load_vectors();
        let v = vectors.iter().find(|v| v.name == "canonical").unwrap();
        assert_eq!(decrypt(b"too-short", &v.blob), Err(E2eeError::InvalidKey));
    }

    #[test]
    fn decrypt_short_blob() {
        let key = [0u8; KEY_LEN];
        assert_eq!(decrypt(&key, "AAAAAAAA"), Err(E2eeError::ShortBlob));
    }

    #[test]
    fn decrypt_bad_base64() {
        let key = [0u8; KEY_LEN];
        // Not valid base64 (contains '!'); must be InvalidBlob, never a panic.
        assert_eq!(
            decrypt(&key, "!!!!not-base64!!!!"),
            Err(E2eeError::InvalidBlob)
        );
    }

    // ---- decrypt_message (notify-path integration) ------------------------

    fn enc_msg(title_blob: &str, body_blob: &str) -> ServerMessage {
        ServerMessage {
            id: 1,
            send_id: 1,
            priority: 0,
            sound: String::new(),
            title: title_blob.to_string(),
            message: body_blob.to_string(),
            url: String::new(),
            url_title: String::new(),
            html: false,
            monospace: false,
            timestamp: 1,
            ttl: 0,
            tag: String::new(),
            encrypted: true,
            receipt_id: String::new(),
        }
    }

    #[test]
    fn decrypt_message_plaintext_passes_through() {
        // Not encrypted -> returned verbatim, no decryption attempted (so a
        // missing key never degrades a plaintext message).
        let m = ServerMessage {
            encrypted: false,
            ..enc_msg("plain-title", "plain-body")
        };
        let out = decrypt_message(&m, None);
        assert_eq!(out.title, "plain-title");
        assert_eq!(out.message, "plain-body");
        assert!(!out.encrypted);
    }

    #[test]
    fn decrypt_message_with_correct_key_shows_plaintext() {
        let vectors = load_vectors();
        let canonical = vectors.iter().find(|v| v.name == "canonical").unwrap();
        let second = vectors.iter().find(|v| v.name == "short").unwrap();
        let m = enc_msg(&canonical.blob, &second.blob);
        let out = decrypt_message(&m, Some(&canonical.key_hex));
        assert_eq!(out.title, "Pushfree E2EE test vector");
        assert_eq!(out.message, "hi");
        assert!(
            !out.encrypted,
            "decrypted message must clear the encrypted flag"
        );
    }

    #[test]
    fn decrypt_message_wrong_key_shows_placeholder_never_garbage() {
        let vectors = load_vectors();
        let canonical = vectors.iter().find(|v| v.name == "canonical").unwrap();
        let m = enc_msg(&canonical.blob, &canonical.blob);
        let wrong_key = "f".repeat(64);
        let out = decrypt_message(&m, Some(&wrong_key));
        assert_eq!(out.title, DECRYPT_FAILED_PLACEHOLDER);
        assert_eq!(out.message, DECRYPT_FAILED_PLACEHOLDER);
        // CRITICAL: ciphertext never reaches the UI on failure.
        assert_ne!(out.title, canonical.blob);
    }

    #[test]
    fn decrypt_message_no_key_shows_placeholder() {
        let vectors = load_vectors();
        let canonical = vectors.iter().find(|v| v.name == "canonical").unwrap();
        let m = enc_msg(&canonical.blob, &canonical.blob);
        // No key configured at all.
        let out = decrypt_message(&m, None);
        assert_eq!(out.title, DECRYPT_FAILED_PLACEHOLDER);
        assert_eq!(out.message, DECRYPT_FAILED_PLACEHOLDER);
    }

    #[test]
    fn decrypt_message_empty_field_stays_empty() {
        let vectors = load_vectors();
        let canonical = vectors.iter().find(|v| v.name == "canonical").unwrap();
        // title absent (empty), body present.
        let m = ServerMessage {
            title: String::new(),
            message: canonical.blob.clone(),
            ..enc_msg("", "")
        };
        let out = decrypt_message(&m, Some(&canonical.key_hex));
        assert_eq!(out.title, "", "absent field stays empty");
        assert_eq!(out.message, "Pushfree E2EE test vector");
    }
}
