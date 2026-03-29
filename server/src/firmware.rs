use std::sync::Arc;

use ghostcam::firmware::FirmwareRelease;
use ghostcam::wire::command::Command;
use rand::Rng;

use crate::ingest::registry::RoutingRegistry;

/// Schedule staggered reboot commands to all connected cameras.
///
/// Each camera gets a random delay within `0..stagger_secs` to avoid
/// thundering herd on the firmware CDN and surveillance gaps.
pub fn schedule_staggered_reboot(registry: Arc<RoutingRegistry>, stagger_secs: u64) {
    tokio::spawn(async move {
        let slots = registry.all_slots().await;
        if slots.is_empty() {
            return;
        }

        tracing::info!(
            cameras = slots.len(),
            stagger_secs,
            "scheduling staggered reboot for firmware update"
        );

        let mut rng = rand::thread_rng();

        for slot in slots {
            let delay_secs = rng.gen_range(0..stagger_secs.max(1));
            let slot = slot.clone();
            tokio::spawn(async move {
                tokio::time::sleep(std::time::Duration::from_secs(delay_secs)).await;
                let seq = slot.next_seq();
                if let Err(e) = slot.send_command(Command::Reboot { seq }).await {
                    tracing::warn!(
                        device_id = %slot.device_id,
                        "failed to send reboot command: {e}"
                    );
                } else {
                    tracing::info!(
                        device_id = %slot.device_id,
                        delay_secs,
                        "sent reboot command for firmware update"
                    );
                }
            });
        }
    });
}

/// Check if a camera's firmware version is stale compared to the latest known
/// release, and send a Reboot command if so.
pub async fn check_and_reboot_if_stale(
    registry: &RoutingRegistry,
    device_id: &ghostcam::types::DeviceId,
    fw_version: &str,
    latest: &Option<FirmwareRelease>,
) {
    let Some(ref release) = latest else {
        return;
    };

    if ghostcam::firmware::is_newer_version(fw_version, &release.version) {
        tracing::info!(
            device_id = %device_id,
            camera_version = fw_version,
            latest_version = %release.version,
            "camera firmware is stale — sending reboot"
        );

        if let Some(slot) = registry.get_slot(device_id).await {
            let seq = slot.next_seq();
            if let Err(e) = slot.send_command(Command::Reboot { seq }).await {
                tracing::warn!(
                    device_id = %device_id,
                    "failed to send reboot for stale firmware: {e}"
                );
            }
        }
    }
}
