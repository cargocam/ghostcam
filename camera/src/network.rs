use std::time::{Duration, Instant};

use anyhow::Result;
use tokio::sync::watch;

// ---------------------------------------------------------------------------
// Network monitor — detects default route interface changes
// ---------------------------------------------------------------------------

/// Read the default route interface from `/proc/net/route` (Linux only).
/// Returns `None` on non-Linux or if no default route exists.
#[cfg(target_os = "linux")]
pub fn read_default_interface() -> Option<String> {
    read_default_interface_from(&std::fs::read_to_string("/proc/net/route").ok()?)
}

/// On non-Linux (macOS dev) always return `Some("lo0")` so the camera never
/// blocks waiting for a route and the monitor never fires a change signal.
#[cfg(not(target_os = "linux"))]
pub fn read_default_interface() -> Option<String> {
    Some("lo0".to_string())
}

/// Parse `/proc/net/route` content. Exported for unit tests.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub fn read_default_interface_from(content: &str) -> Option<String> {
    for line in content.lines().skip(1) {
        let fields: Vec<&str> = line.split('\t').collect();
        // Destination 00000000 = default route
        if fields.len() >= 2 && fields[1] == "00000000" {
            return Some(fields[0].to_string());
        }
    }
    None
}

/// Wait until a default route exists. On macOS this returns immediately.
pub async fn wait_for_route() {
    if read_default_interface().is_some() {
        return;
    }
    tracing::info!("no default route, waiting for network...");
    let start = Instant::now();
    loop {
        tokio::time::sleep(Duration::from_millis(500)).await;
        if read_default_interface().is_some() {
            tracing::info!(
                elapsed_s = start.elapsed().as_secs_f64(),
                "default route appeared"
            );
            return;
        }
    }
}

/// Spawn a background task that polls `/proc/net/route` every 500ms and
/// signals on the returned `watch::Receiver` when the default interface
/// changes. On macOS this never signals.
pub fn spawn_network_monitor() -> watch::Receiver<u64> {
    let (tx, rx) = watch::channel(0u64);
    tokio::spawn(network_monitor_loop(tx));
    rx
}

async fn network_monitor_loop(change_tx: watch::Sender<u64>) {
    let mut last_signaled: Option<String> = read_default_interface();
    let mut generation: u64 = 0;

    tracing::info!(iface = ?last_signaled, "network monitor started");

    loop {
        tokio::time::sleep(Duration::from_millis(500)).await;

        let current = read_default_interface();

        if current == last_signaled {
            continue;
        }

        // Route changed — only care about transitions to a DIFFERENT real
        // interface. Drops to None are handled by the send timeout.
        if current.is_none() {
            // Route dropped — don't signal yet, wait for a new route.
            last_signaled = None;
            continue;
        }

        // A different real interface appeared. Debounce 1s to avoid acting
        // on transient flaps (NM cycling the modem).
        tracing::warn!(
            from = ?last_signaled,
            to = ?current,
            "default route change detected, debouncing 1s"
        );
        tokio::time::sleep(Duration::from_secs(1)).await;

        let settled = read_default_interface();

        if settled == last_signaled {
            tracing::debug!(iface = ?last_signaled, "route settled back, ignoring");
            continue;
        }
        if settled.is_none() {
            tracing::debug!("route went None during debounce, ignoring");
            continue;
        }

        // Definitive change to a different real interface.
        tracing::warn!(from = ?last_signaled, to = ?settled, "default route settled");
        last_signaled = settled;
        generation += 1;
        let _ = change_tx.send(generation);
    }
}

// ---------------------------------------------------------------------------
// WiFi / NetworkManager helpers (existing code)
// ---------------------------------------------------------------------------

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

#[cfg(test)]
mod tests {
    use super::*;

    // --- /proc/net/route parsing tests ---

    #[test]
    fn route_default_via_wlan0() {
        let content = "\
Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT
wlan0\t00000000\t0100A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0
wwan0\t0000FEA9\t00000000\t0001\t0\t0\t700\t0000FFFF\t0\t0\t0
";
        assert_eq!(
            read_default_interface_from(content),
            Some("wlan0".to_string())
        );
    }

    #[test]
    fn route_default_via_wwan0() {
        let content = "\
Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT
wwan0\t00000000\t0A000001\t0003\t0\t0\t700\t00000000\t0\t0\t0
";
        assert_eq!(
            read_default_interface_from(content),
            Some("wwan0".to_string())
        );
    }

    #[test]
    fn route_no_default() {
        let content = "\
Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT
wlan0\t0064A8C0\t00000000\t0001\t0\t0\t600\t00FFFFFF\t0\t0\t0
";
        assert_eq!(read_default_interface_from(content), None);
    }

    #[test]
    fn route_empty_table() {
        let content =
            "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n";
        assert_eq!(read_default_interface_from(content), None);
    }

    #[test]
    fn route_multiple_defaults_returns_first() {
        // Lower-metric route appears first in /proc/net/route
        let content = "\
Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT
wlan0\t00000000\t0100A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0
wwan0\t00000000\t0A000001\t0003\t0\t0\t700\t00000000\t0\t0\t0
";
        assert_eq!(
            read_default_interface_from(content),
            Some("wlan0".to_string())
        );
    }

    #[test]
    fn route_header_only() {
        let content =
            "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT";
        assert_eq!(read_default_interface_from(content), None);
    }

}
