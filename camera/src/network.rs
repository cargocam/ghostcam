use anyhow::Result;
use ghostcam::wire::alert::{Alert, NetworkEntry};

/// Ensure WiFi is connected if configured.
pub async fn ensure_wifi(ssid: &str, psk: Option<&str>) -> Result<()> {
    // Check if already connected
    let status = tokio::process::Command::new("nmcli")
        .args(["connection", "show", "--active"])
        .output()
        .await;

    match status {
        Ok(output) => {
            let stdout = String::from_utf8_lossy(&output.stdout);
            if stdout.contains(ssid) {
                tracing::debug!(ssid, "already connected to WiFi");
                return Ok(());
            }
        }
        Err(e) => {
            tracing::warn!("nmcli not available: {e}");
            return Ok(()); // nmcli not available, skip
        }
    }

    tracing::info!(ssid, "connecting to WiFi network");

    let mut cmd = tokio::process::Command::new("nmcli");
    cmd.args(["device", "wifi", "connect", ssid]);
    if let Some(psk) = psk {
        cmd.args(["password", psk]);
    }

    let output = cmd.output().await?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        anyhow::bail!("WiFi connection failed: {stderr}");
    }

    tracing::info!(ssid, "WiFi connected");
    Ok(())
}

/// Handle NetworkConfig command: connect to a WiFi network.
pub async fn handle_network_config(
    ssid: &str,
    psk: &str,
    alerts_tx: &tokio::sync::Mutex<quinn::SendStream>,
    seq: u64,
) -> Result<()> {
    send_ack(alerts_tx, "network_config", seq).await?;

    match ensure_wifi(ssid, Some(psk)).await {
        Ok(()) => {
            tracing::info!(ssid, "network configured via command");
        }
        Err(e) => {
            tracing::warn!(ssid, "network config failed: {e}");
        }
    }
    Ok(())
}

/// Handle RemoveNetwork command: delete a saved WiFi network.
pub async fn handle_remove_network(
    ssid: &str,
    alerts_tx: &tokio::sync::Mutex<quinn::SendStream>,
    seq: u64,
) -> Result<()> {
    send_ack(alerts_tx, "remove_network", seq).await?;

    let output = tokio::process::Command::new("nmcli")
        .args(["connection", "delete", ssid])
        .output()
        .await;

    match output {
        Ok(o) if o.status.success() => {
            tracing::info!(ssid, "network removed");
        }
        Ok(o) => {
            let stderr = String::from_utf8_lossy(&o.stderr);
            tracing::warn!(ssid, "network removal failed: {stderr}");
        }
        Err(e) => {
            tracing::warn!(ssid, "nmcli error: {e}");
        }
    }
    Ok(())
}

/// Handle ListNetworks command: scan for available WiFi networks.
pub async fn handle_list_networks(
    alerts_tx: &tokio::sync::Mutex<quinn::SendStream>,
    seq: u64,
) -> Result<()> {
    send_ack(alerts_tx, "list_networks", seq).await?;

    let output = tokio::process::Command::new("nmcli")
        .args(["-t", "-f", "SSID,SIGNAL", "device", "wifi", "list"])
        .output()
        .await;

    let networks = match output {
        Ok(o) if o.status.success() => {
            let stdout = String::from_utf8_lossy(&o.stdout);
            parse_nmcli_networks(&stdout)
        }
        _ => vec![],
    };

    let alert = Alert::Networks { networks };
    let mut stream = alerts_tx.lock().await;
    let _ = ghostcam::wire::framing::write_json(&mut *stream, &alert).await;
    Ok(())
}

/// Parse nmcli terse output (SSID:SIGNAL) into NetworkEntry list.
fn parse_nmcli_networks(output: &str) -> Vec<NetworkEntry> {
    output
        .lines()
        .filter(|l| !l.is_empty())
        .filter_map(|line| {
            let (ssid, signal) = line.split_once(':')?;
            if ssid.is_empty() {
                return None;
            }
            let signal_dbm = signal.parse::<i8>().ok();
            Some(NetworkEntry {
                ssid: ssid.to_string(),
                signal_dbm,
            })
        })
        .collect()
}

async fn send_ack(
    alerts_tx: &tokio::sync::Mutex<quinn::SendStream>,
    cmd: &str,
    seq: u64,
) -> Result<()> {
    let ack = Alert::Ack {
        cmd: cmd.to_string(),
        seq,
    };
    let mut stream = alerts_tx.lock().await;
    ghostcam::wire::framing::write_json(&mut *stream, &ack)
        .await
        .map_err(|e| anyhow::anyhow!("ack write error: {e}"))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_nmcli_output() {
        let output = "HomeNet:75\nGuestWiFi:42\n:0\n";
        let networks = parse_nmcli_networks(output);
        assert_eq!(networks.len(), 2);
        assert_eq!(networks[0].ssid, "HomeNet");
        assert_eq!(networks[0].signal_dbm, Some(75));
        assert_eq!(networks[1].ssid, "GuestWiFi");
        assert_eq!(networks[1].signal_dbm, Some(42));
    }

    #[test]
    fn parse_empty_nmcli_output() {
        let networks = parse_nmcli_networks("");
        assert!(networks.is_empty());
    }

    #[test]
    fn parse_nmcli_no_signal() {
        let output = "TestNet:abc\n";
        let networks = parse_nmcli_networks(output);
        assert_eq!(networks.len(), 1);
        assert_eq!(networks[0].ssid, "TestNet");
        assert_eq!(networks[0].signal_dbm, None);
    }
}
