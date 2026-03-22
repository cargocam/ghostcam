use std::sync::atomic::Ordering;
use std::sync::Arc;

use anyhow::Result;
use ghostcam::wire::command::Command;

use super::slot::IngestSlot;

/// Client viewing mode — determines whether the client needs live streams.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ClientMode {
    Live,
    Playback,
    Map,
}

/// Update subscriber demand counters and send start/stop commands as needed.
/// Call this when a client changes viewing mode for a camera.
pub async fn update_subscriber_demand(
    slot: &Arc<IngestSlot>,
    old_mode: Option<ClientMode>,
    new_mode: ClientMode,
) -> Result<()> {
    let was_live = old_mode == Some(ClientMode::Live);
    let is_live = new_mode == ClientMode::Live;

    if !was_live && is_live {
        // Joining live
        let prev_video = slot.video_subscribers.fetch_add(1, Ordering::SeqCst);
        let prev_audio = slot.audio_subscribers.fetch_add(1, Ordering::SeqCst);
        if prev_video == 0 {
            slot.send_command(Command::StartVideo {
                seq: slot.next_seq(),
            })
            .await?;
        }
        if prev_audio == 0 {
            slot.send_command(Command::StartAudio {
                seq: slot.next_seq(),
            })
            .await?;
        }
    } else if was_live && !is_live {
        // Leaving live
        let prev_video = slot.video_subscribers.fetch_sub(1, Ordering::SeqCst);
        let prev_audio = slot.audio_subscribers.fetch_sub(1, Ordering::SeqCst);
        if prev_video == 1 {
            slot.send_command(Command::StopVideo {
                seq: slot.next_seq(),
            })
            .await?;
        }
        if prev_audio == 1 {
            slot.send_command(Command::StopAudio {
                seq: slot.next_seq(),
            })
            .await?;
        }
    }
    // Playback↔Map transitions don't affect live subscriber counts

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ingest::slot::test_slot_with_commands;

    #[tokio::test]
    async fn first_live_sends_start() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");
        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();

        let cmd1 = rx.recv().await.unwrap();
        assert!(matches!(cmd1, Command::StartVideo { .. }));
        let cmd2 = rx.recv().await.unwrap();
        assert!(matches!(cmd2, Command::StartAudio { .. }));
    }

    #[tokio::test]
    async fn second_live_no_command() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        // First live viewer
        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();
        // Drain the start commands
        let _ = rx.recv().await;
        let _ = rx.recv().await;

        // Second live viewer
        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();

        // No more commands should be sent
        assert!(rx.try_recv().is_err());
    }

    #[tokio::test]
    async fn last_live_leaves_sends_stop() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();
        let _ = rx.recv().await; // StartVideo
        let _ = rx.recv().await; // StartAudio

        update_subscriber_demand(&slot, Some(ClientMode::Live), ClientMode::Playback)
            .await
            .unwrap();

        let cmd1 = rx.recv().await.unwrap();
        assert!(matches!(cmd1, Command::StopVideo { .. }));
        let cmd2 = rx.recv().await.unwrap();
        assert!(matches!(cmd2, Command::StopAudio { .. }));
    }

    #[tokio::test]
    async fn not_last_live_no_stop() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        // Two live viewers
        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();
        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();
        // Drain start commands
        let _ = rx.recv().await;
        let _ = rx.recv().await;

        // One leaves
        update_subscriber_demand(&slot, Some(ClientMode::Live), ClientMode::Playback)
            .await
            .unwrap();

        // No stop commands — still one live viewer
        assert!(rx.try_recv().is_err());
    }

    #[tokio::test]
    async fn playback_to_live() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        update_subscriber_demand(&slot, Some(ClientMode::Playback), ClientMode::Live)
            .await
            .unwrap();

        let cmd1 = rx.recv().await.unwrap();
        assert!(matches!(cmd1, Command::StartVideo { .. }));
        let cmd2 = rx.recv().await.unwrap();
        assert!(matches!(cmd2, Command::StartAudio { .. }));
    }

    #[tokio::test]
    async fn map_to_live() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        update_subscriber_demand(&slot, Some(ClientMode::Map), ClientMode::Live)
            .await
            .unwrap();

        let cmd1 = rx.recv().await.unwrap();
        assert!(matches!(cmd1, Command::StartVideo { .. }));
    }

    #[tokio::test]
    async fn live_to_map() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        update_subscriber_demand(&slot, None, ClientMode::Live)
            .await
            .unwrap();
        let _ = rx.recv().await;
        let _ = rx.recv().await;

        update_subscriber_demand(&slot, Some(ClientMode::Live), ClientMode::Map)
            .await
            .unwrap();

        let cmd1 = rx.recv().await.unwrap();
        assert!(matches!(cmd1, Command::StopVideo { .. }));
    }

    #[tokio::test]
    async fn playback_to_map_no_change() {
        let (slot, mut rx) = test_slot_with_commands("cam-1", "user-1");

        update_subscriber_demand(&slot, Some(ClientMode::Playback), ClientMode::Map)
            .await
            .unwrap();

        assert!(rx.try_recv().is_err());
    }
}
