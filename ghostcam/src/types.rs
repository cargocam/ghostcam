use serde::{Deserialize, Serialize};
use std::fmt;

macro_rules! newtype_string {
    ($name:ident, $doc:expr) => {
        #[doc = $doc]
        #[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
        pub struct $name(pub String);

        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str(&self.0)
            }
        }

        impl From<String> for $name {
            fn from(s: String) -> Self {
                Self(s)
            }
        }

        impl From<&str> for $name {
            fn from(s: &str) -> Self {
                Self(s.to_string())
            }
        }

        impl AsRef<str> for $name {
            fn as_ref(&self) -> &str {
                &self.0
            }
        }
    };
}

newtype_string!(DeviceId, "Server-assigned UUID for an enrolled camera.");
newtype_string!(UserId, "User identifier.");
newtype_string!(SessionId, "Cryptographically random session token.");
newtype_string!(TokenId, "API token identifier.");
newtype_string!(
    CertFingerprint,
    "SHA-256 fingerprint of the full DER-encoded certificate, hex-encoded."
);

/// Monotonically increasing command sequence number.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Seq(pub u64);

impl fmt::Display for Seq {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    #[test]
    fn device_id_display() {
        assert_eq!(DeviceId::from("abc").to_string(), "abc");
    }

    #[test]
    fn device_id_equality() {
        assert_eq!(DeviceId::from("x"), DeviceId::from("x"));
        assert_ne!(DeviceId::from("x"), DeviceId::from("y"));
    }

    #[test]
    fn device_id_hash() {
        let mut map = HashMap::new();
        map.insert(DeviceId::from("cam-01"), 42);
        assert_eq!(map.get(&DeviceId::from("cam-01")), Some(&42));
    }

    #[test]
    fn all_newtypes_clone() {
        let _ = DeviceId::from("a").clone();
        let _ = UserId::from("a").clone();
        let _ = SessionId::from("a").clone();
        let _ = TokenId::from("a").clone();
        let _ = CertFingerprint::from("a").clone();
        let _ = Seq(1);
    }

    #[test]
    fn all_newtypes_serde() {
        let id = DeviceId::from("cam-01");
        let json = serde_json::to_string(&id).unwrap();
        let back: DeviceId = serde_json::from_str(&json).unwrap();
        assert_eq!(id, back);

        let seq = Seq(42);
        let json = serde_json::to_string(&seq).unwrap();
        let back: Seq = serde_json::from_str(&json).unwrap();
        assert_eq!(seq, back);
    }
}
