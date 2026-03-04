use serde::{Deserialize, Serialize};

use crate::group::GroupId;

/// Sent by device on the control stream immediately after QUIC connection.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeviceHello {
    pub device_id: String,
    pub group_id: GroupId,
    #[serde(default)]
    pub capabilities: Vec<String>,
}
