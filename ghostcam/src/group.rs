use serde::{Deserialize, Serialize};
use std::fmt;

/// Hierarchical group identifier, e.g. `usr-alice:perimeter`.
/// Segments are separated by `:`.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(transparent)]
pub struct GroupId(pub String);

impl GroupId {
    pub fn new(s: impl Into<String>) -> Self {
        Self(s.into())
    }

    /// Returns true if `self` is an ancestor of (or equal to) `other`.
    /// `usr-alice` is ancestor of `usr-alice:perimeter`.
    pub fn is_ancestor_of(&self, other: &GroupId) -> bool {
        other.0 == self.0 || other.0.starts_with(&format!("{}:", self.0))
    }

    /// Return the parent group, or None if this is a root group.
    pub fn parent(&self) -> Option<GroupId> {
        self.0.rfind(':').map(|i| GroupId(self.0[..i].to_string()))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for GroupId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl From<&str> for GroupId {
    fn from(s: &str) -> Self {
        Self(s.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ancestor() {
        let root = GroupId::new("usr-alice");
        let child = GroupId::new("usr-alice:perimeter");
        assert!(root.is_ancestor_of(&child));
        assert!(root.is_ancestor_of(&root));
        assert!(!child.is_ancestor_of(&root));
    }

    #[test]
    fn parent() {
        let g = GroupId::new("usr-alice:perimeter:north");
        assert_eq!(g.parent(), Some(GroupId::new("usr-alice:perimeter")));
        assert_eq!(
            g.parent().unwrap().parent(),
            Some(GroupId::new("usr-alice"))
        );
        assert_eq!(GroupId::new("root").parent(), None);
    }
}
