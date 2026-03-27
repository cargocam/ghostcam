use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TierInfo {
    pub id: String,
    pub name: String,
    /// None = unlimited
    pub camera_limit: Option<u32>,
    /// None = unlimited
    pub storage_gb: Option<u64>,
    /// None = unlimited
    pub bandwidth_gb: Option<u64>,
    /// Monthly price in cents
    pub price_cents: u32,
}

/// A collection of subscription tiers, configurable at startup.
#[derive(Debug, Clone)]
pub struct TierRegistry {
    tiers: Vec<TierInfo>,
}

impl TierRegistry {
    /// Build from a list of tiers. Panics if no "free" tier is present.
    pub fn new(tiers: Vec<TierInfo>) -> Self {
        assert!(
            tiers.iter().any(|t| t.id == "free"),
            "tier list must include a 'free' tier"
        );
        Self { tiers }
    }

    pub fn get(&self, id: &str) -> Option<&TierInfo> {
        self.tiers.iter().find(|t| t.id == id)
    }

    pub fn camera_limit(&self, tier: &str) -> Option<u32> {
        self.get(tier).and_then(|t| t.camera_limit)
    }

    pub fn all(&self) -> &[TierInfo] {
        &self.tiers
    }
}

impl Default for TierRegistry {
    fn default() -> Self {
        Self::new(default_tiers())
    }
}

pub fn default_tiers() -> Vec<TierInfo> {
    vec![
        TierInfo {
            id: "free".into(),
            name: "Free".into(),
            camera_limit: Some(2),
            storage_gb: Some(0),
            bandwidth_gb: Some(5),
            price_cents: 0,
        },
        TierInfo {
            id: "starter".into(),
            name: "Starter".into(),
            camera_limit: Some(5),
            storage_gb: Some(50),
            bandwidth_gb: Some(100),
            price_cents: 999,
        },
        TierInfo {
            id: "pro".into(),
            name: "Pro".into(),
            camera_limit: Some(20),
            storage_gb: Some(500),
            bandwidth_gb: Some(1000),
            price_cents: 2999,
        },
        TierInfo {
            id: "enterprise".into(),
            name: "Enterprise".into(),
            camera_limit: None,
            storage_gb: None,
            bandwidth_gb: None,
            price_cents: 9999,
        },
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_tiers_have_unique_ids() {
        let registry = TierRegistry::default();
        let mut ids: Vec<&str> = registry.all().iter().map(|t| t.id.as_str()).collect();
        let orig_len = ids.len();
        ids.sort();
        ids.dedup();
        assert_eq!(ids.len(), orig_len);
    }

    #[test]
    fn free_tier_exists() {
        let registry = TierRegistry::default();
        let tier = registry.get("free").unwrap();
        assert_eq!(tier.price_cents, 0);
        assert_eq!(tier.camera_limit, Some(2));
    }

    #[test]
    fn enterprise_unlimited() {
        let registry = TierRegistry::default();
        let tier = registry.get("enterprise").unwrap();
        assert!(tier.camera_limit.is_none());
        assert!(tier.storage_gb.is_none());
        assert!(tier.bandwidth_gb.is_none());
    }

    #[test]
    fn camera_limit_for_unknown_tier() {
        let registry = TierRegistry::default();
        assert!(registry.camera_limit("nonexistent").is_none());
    }

    #[test]
    fn custom_tiers() {
        let registry = TierRegistry::new(vec![
            TierInfo {
                id: "free".into(),
                name: "Free".into(),
                camera_limit: Some(1),
                storage_gb: Some(0),
                bandwidth_gb: Some(1),
                price_cents: 0,
            },
            TierInfo {
                id: "premium".into(),
                name: "Premium".into(),
                camera_limit: Some(100),
                storage_gb: None,
                bandwidth_gb: None,
                price_cents: 4999,
            },
        ]);
        assert_eq!(registry.camera_limit("free"), Some(1));
        assert_eq!(registry.camera_limit("premium"), Some(100));
        assert!(registry.get("starter").is_none());
    }

    #[test]
    #[should_panic(expected = "free")]
    fn must_have_free_tier() {
        TierRegistry::new(vec![TierInfo {
            id: "premium".into(),
            name: "Premium".into(),
            camera_limit: None,
            storage_gb: None,
            bandwidth_gb: None,
            price_cents: 999,
        }]);
    }
}
