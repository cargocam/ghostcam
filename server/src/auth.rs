use anyhow::Result;
use argon2::password_hash::SaltString;
use argon2::{Argon2, PasswordHash, PasswordHasher, PasswordVerifier};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use ghostcam::types::{SessionId, TokenId};
use rand::rngs::OsRng;
use rand::RngCore;

/// Hash a password with Argon2id. Returns the PHC-formatted hash string.
pub fn hash_password(password: &str) -> Result<String> {
    let salt = SaltString::generate(&mut OsRng);
    let argon2 = Argon2::default();
    let hash = argon2
        .hash_password(password.as_bytes(), &salt)
        .map_err(|e| anyhow::anyhow!("password hashing failed: {e}"))?;
    Ok(hash.to_string())
}

/// Verify a password against an Argon2id PHC hash string.
pub fn verify_password(password: &str, hash: &str) -> Result<bool> {
    let parsed = PasswordHash::new(hash)
        .map_err(|e| anyhow::anyhow!("invalid password hash format: {e}"))?;
    Ok(Argon2::default()
        .verify_password(password.as_bytes(), &parsed)
        .is_ok())
}

/// Generate a cryptographically random password (16 alphanumeric chars).
pub fn generate_random_password() -> String {
    const CHARSET: &[u8] = b"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";
    let mut rng = OsRng;
    (0..16)
        .map(|_| {
            let idx = (rng.next_u32() as usize) % CHARSET.len();
            CHARSET[idx] as char
        })
        .collect()
}

/// Generate a cryptographically random session ID (32 bytes, URL-safe base64).
pub fn generate_session_id() -> SessionId {
    let mut bytes = [0u8; 32];
    OsRng.fill_bytes(&mut bytes);
    SessionId(URL_SAFE_NO_PAD.encode(bytes))
}

/// Generate a cryptographically random API token (32 bytes, URL-safe base64).
/// Returns (token_id, raw_token) — raw_token is shown once and never stored.
pub fn generate_api_token() -> (TokenId, String) {
    let token_id = TokenId(uuid::Uuid::new_v4().to_string());
    let mut bytes = [0u8; 32];
    OsRng.fill_bytes(&mut bytes);
    let raw_token = URL_SAFE_NO_PAD.encode(bytes);
    (token_id, raw_token)
}

/// Compute HMAC-SHA256 of a raw API token using the server secret.
/// Returns hex-encoded hash for storage and comparison.
pub fn hmac_token(raw_token: &str, secret: &[u8]) -> String {
    let key = ring::hmac::Key::new(ring::hmac::HMAC_SHA256, secret);
    let tag = ring::hmac::sign(&key, raw_token.as_bytes());
    tag.as_ref().iter().map(|b| format!("{b:02x}")).collect()
}

/// Generate a 32-byte random HMAC secret.
pub fn generate_hmac_secret() -> Vec<u8> {
    let mut bytes = [0u8; 32];
    OsRng.fill_bytes(&mut bytes);
    bytes.to_vec()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hash_and_verify_password() {
        let hash = hash_password("password123").unwrap();
        assert!(verify_password("password123", &hash).unwrap());
    }

    #[test]
    fn verify_wrong_password() {
        let hash = hash_password("password123").unwrap();
        assert!(!verify_password("wrong", &hash).unwrap());
    }

    #[test]
    fn hash_is_argon2id() {
        let hash = hash_password("test").unwrap();
        assert!(hash.starts_with("$argon2id$"));
    }

    #[test]
    fn hash_is_unique() {
        let h1 = hash_password("same").unwrap();
        let h2 = hash_password("same").unwrap();
        assert_ne!(h1, h2); // Different salts
    }

    #[test]
    fn generate_random_password_length() {
        let pw = generate_random_password();
        assert_eq!(pw.len(), 16);
    }

    #[test]
    fn generate_random_password_alphanumeric() {
        let pw = generate_random_password();
        assert!(pw.chars().all(|c| c.is_ascii_alphanumeric()));
    }

    #[test]
    fn generate_random_password_unique() {
        let p1 = generate_random_password();
        let p2 = generate_random_password();
        assert_ne!(p1, p2);
    }

    #[test]
    fn generate_session_id_length() {
        let sid = generate_session_id();
        let decoded = URL_SAFE_NO_PAD.decode(sid.0.as_bytes()).unwrap();
        assert_eq!(decoded.len(), 32);
    }

    #[test]
    fn generate_session_id_unique() {
        let s1 = generate_session_id();
        let s2 = generate_session_id();
        assert_ne!(s1, s2);
    }

    #[test]
    fn generate_api_token_unique() {
        let (id1, t1) = generate_api_token();
        let (id2, t2) = generate_api_token();
        assert_ne!(id1, id2);
        assert_ne!(t1, t2);
    }

    #[test]
    fn hmac_token_deterministic() {
        let secret = b"test-secret";
        let h1 = hmac_token("my-token", secret);
        let h2 = hmac_token("my-token", secret);
        assert_eq!(h1, h2);
    }

    #[test]
    fn hmac_token_different_secret() {
        let h1 = hmac_token("my-token", b"secret-a");
        let h2 = hmac_token("my-token", b"secret-b");
        assert_ne!(h1, h2);
    }

    #[test]
    fn generate_hmac_secret_length() {
        let secret = generate_hmac_secret();
        assert_eq!(secret.len(), 32);
    }

    #[test]
    fn generate_hmac_secret_unique() {
        let s1 = generate_hmac_secret();
        let s2 = generate_hmac_secret();
        assert_ne!(s1, s2);
    }
}
