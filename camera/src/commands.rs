use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use anyhow::Result;
use ghostcam::wire::alert::Alert;
use ghostcam::wire::command::Command;
use ghostcam::wire::framing;
use tokio::sync::{mpsc, Mutex};

use crate::recording::uploads::UploadCommand;

/// Signals from the command handler to the main loop.
#[derive(Debug)]
pub enum CommandSignal {
    /// Camera was unregistered — clear enrollment and re-enter registration
    Unregistered,
}

/// Read commands from the server and dispatch them.
pub async fn run_command_reader(
    commands_rx: &mut quinn::RecvStream,
    video_enabled: Arc<AtomicBool>,
    audio_enabled: Arc<AtomicBool>,
    alerts_tx: &Mutex<quinn::SendStream>,
    upload_tx: Option<mpsc::Sender<UploadCommand>>,
    data_dir: PathBuf,
    signal_tx: mpsc::Sender<CommandSignal>,
) -> Result<()> {
    loop {
        let cmd: Command = framing::read_json(commands_rx)
            .await
            .map_err(|e| anyhow::anyhow!("command read error: {e}"))?
            .ok_or_else(|| anyhow::anyhow!("commands stream closed"))?;

        tracing::debug!(?cmd, "received command");

        match cmd {
            Command::StartVideo { .. } => {
                video_enabled.store(true, Ordering::SeqCst);
                tracing::info!("video streaming enabled");
            }
            Command::StopVideo { .. } => {
                video_enabled.store(false, Ordering::SeqCst);
                tracing::info!("video streaming disabled");
            }
            Command::StartAudio { .. } => {
                audio_enabled.store(true, Ordering::SeqCst);
                tracing::info!("audio streaming enabled");
            }
            Command::StopAudio { .. } => {
                audio_enabled.store(false, Ordering::SeqCst);
                tracing::info!("audio streaming disabled");
            }
            Command::Reboot { seq } => {
                send_ack(alerts_tx, "reboot", seq).await?;
                tracing::info!("reboot requested");
                #[cfg(target_os = "linux")]
                {
                    let _ = std::process::Command::new("systemctl")
                        .arg("reboot")
                        .spawn();
                }
                #[cfg(not(target_os = "linux"))]
                tracing::warn!("reboot not supported on this platform");
            }
            Command::Unregister { seq } => {
                send_ack(alerts_tx, "unregister", seq).await?;
                tracing::info!("unregister command received");
                crate::enrollment::clear_enrollment(&data_dir).await?;
                let _ = signal_tx.send(CommandSignal::Unregistered).await;
                return Ok(());
            }
            Command::UpdateAvailable {
                seq,
                version,
                url,
                sha256,
                ..
            } => {
                send_ack(alerts_tx, "update_available", seq).await?;
                let alerts = alerts_tx;
                let dd = data_dir.clone();
                // Run firmware update (this may exit the process on success)
                if let Err(e) =
                    crate::firmware::handle_firmware_update(&url, &sha256, &version, &dd, alerts)
                        .await
                {
                    tracing::error!("firmware update failed: {e}");
                }
            }
            Command::CertRefresh {
                seq,
                cert_pem,
                ca_pem,
            } => {
                send_ack(alerts_tx, "cert_refresh", seq).await?;
                tracing::info!("cert refresh received");

                // Write the new user certificate
                if let Err(e) =
                    tokio::fs::write(data_dir.join("user.crt"), &cert_pem).await
                {
                    tracing::error!("failed to write refreshed cert: {e}");
                }
                if let Some(ca) = ca_pem {
                    if let Err(e) =
                        tokio::fs::write(data_dir.join("ca.crt"), &ca).await
                    {
                        tracing::error!("failed to write refreshed CA cert: {e}");
                    }
                }
                tracing::info!("certificate refreshed — will use on next connection");
            }
            Command::UploadSegment {
                seq, segment_id, ..
            } => {
                send_ack(alerts_tx, "upload_segment", seq).await?;
                if let Some(tx) = &upload_tx {
                    let _ = tx.send(UploadCommand::Segment { seq, segment_id }).await;
                }
            }
            Command::UploadInit { seq } => {
                send_ack(alerts_tx, "upload_init", seq).await?;
                if let Some(tx) = &upload_tx {
                    let _ = tx.send(UploadCommand::Init { seq }).await;
                }
            }
            Command::NetworkConfig { seq, ssid, psk } => {
                crate::network::handle_network_config(&ssid, &psk, alerts_tx, seq).await?;
            }
            Command::RemoveNetwork { seq, ssid } => {
                crate::network::handle_remove_network(&ssid, alerts_tx, seq).await?;
            }
            Command::ListNetworks { seq } => {
                crate::network::handle_list_networks(alerts_tx, seq).await?;
            }
        }
    }
}

/// Send an ack alert on the alerts stream.
async fn send_ack(
    alerts_tx: &Mutex<quinn::SendStream>,
    cmd: &str,
    seq: u64,
) -> Result<()> {
    let ack = Alert::Ack {
        cmd: cmd.to_string(),
        seq,
    };
    let mut stream = alerts_tx.lock().await;
    framing::write_json(&mut *stream, &ack)
        .await
        .map_err(|e| anyhow::anyhow!("ack write error: {e}"))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::AtomicBool;

    use super::*;

    #[test]
    fn start_video_flag() {
        let flag = Arc::new(AtomicBool::new(false));
        flag.store(true, Ordering::SeqCst);
        assert!(flag.load(Ordering::SeqCst));
    }

    #[test]
    fn stop_video_flag() {
        let flag = Arc::new(AtomicBool::new(true));
        flag.store(false, Ordering::SeqCst);
        assert!(!flag.load(Ordering::SeqCst));
    }
}
