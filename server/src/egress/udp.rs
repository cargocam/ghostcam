use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::Result;
use tokio::net::UdpSocket;
use tokio::sync::{mpsc, Mutex};

/// A raw UDP packet: (source address, payload bytes).
pub type UdpPacket = (SocketAddr, Vec<u8>);

/// A single shared UDP socket for all WebRTC sessions.
///
/// Incoming packets are demultiplexed by STUN `USERNAME` fragment to the
/// correct session's receiver channel. After ICE connectivity is established
/// the source → ufrag mapping is cached for a fast path on subsequent packets.
pub struct SharedWebRtcSocket {
    pub socket: Arc<UdpSocket>,
    pub local_addr: SocketAddr,
    /// local_ufrag → channel for incoming packets to that session
    routing: Arc<Mutex<HashMap<String, mpsc::UnboundedSender<UdpPacket>>>>,
    /// source_addr → local_ufrag (fast path after ICE connected)
    connected: Arc<Mutex<HashMap<SocketAddr, String>>>,
}

impl SharedWebRtcSocket {
    /// Bind a UDP socket on `0.0.0.0:<port>`.
    pub async fn bind(port: u16) -> Result<Arc<Self>> {
        let socket = UdpSocket::bind(format!("0.0.0.0:{port}")).await?;
        let local_addr = socket.local_addr()?;
        Ok(Arc::new(Self {
            socket: Arc::new(socket),
            local_addr,
            routing: Arc::new(Mutex::new(HashMap::new())),
            connected: Arc::new(Mutex::new(HashMap::new())),
        }))
    }

    /// Register a session by its local ICE ufrag. Returns a receiver for
    /// incoming packets addressed to that session.
    pub async fn register(&self, ufrag: String) -> mpsc::UnboundedReceiver<UdpPacket> {
        let (tx, rx) = mpsc::unbounded_channel();
        self.routing.lock().await.insert(ufrag, tx);
        rx
    }

    /// Unregister a session and remove all cached source-address mappings for it.
    pub async fn unregister(&self, ufrag: &str) {
        self.routing.lock().await.remove(ufrag);
        // Clean up fast-path entries that pointed to this ufrag.
        let mut connected = self.connected.lock().await;
        connected.retain(|_, u| u != ufrag);
    }

    /// Cache a confirmed source-address → ufrag mapping for future fast-path lookups.
    pub async fn connect(&self, src: SocketAddr, ufrag: String) {
        self.connected.lock().await.insert(src, ufrag);
    }

    /// Spawn the dispatch loop in its own Tokio task.
    pub fn spawn_dispatch(self: Arc<Self>) {
        tokio::spawn(async move {
            self.dispatch_loop().await;
        });
    }

    /// Send a packet to a specific destination via the shared socket.
    pub async fn send_to(&self, data: &[u8], dest: SocketAddr) -> std::io::Result<usize> {
        self.socket.send_to(data, dest).await
    }

    async fn dispatch_loop(self: Arc<Self>) {
        tracing::info!(local_addr = %self.local_addr, "WebRTC dispatch loop started");
        let mut buf = vec![0u8; 2000];
        loop {
            let Ok((len, src)) = self.socket.recv_from(&mut buf).await else {
                tracing::warn!("shared WebRTC socket recv error, dispatch loop exiting");
                break;
            };
            let data = buf[..len].to_vec();
            tracing::debug!(src = %src, len, "udp packet received");

            // Fast path: known source address.
            let ufrag_opt = self.connected.lock().await.get(&src).cloned();

            let ufrag = if let Some(u) = ufrag_opt {
                tracing::debug!(src = %src, ufrag = %u, "fast-path routing");
                u
            } else {
                // Slow path: parse STUN USERNAME to extract the local ufrag.
                match parse_stun_local_ufrag(&data) {
                    Some(u) => {
                        tracing::debug!(src = %src, ufrag = %u, "stun routing, caching");
                        // Cache for future fast-path lookups.
                        self.connected.lock().await.insert(src, u.clone());
                        u
                    }
                    None => {
                        tracing::debug!(src = %src, "non-stun packet from unknown source, dropping");
                        continue;
                    }
                }
            };

            let routing = self.routing.lock().await;
            if let Some(tx) = routing.get(&ufrag) {
                let _ = tx.send((src, data));
            } else {
                tracing::debug!(ufrag = %ufrag, "no session for ufrag, dropping");
            }
        }
    }
}

/// Parse the local ICE ufrag from a STUN Binding Request `USERNAME` attribute.
///
/// The `USERNAME` value has the form `"<remote_ufrag>:<local_ufrag>"`.
/// The local ufrag is the part after the colon — it is the credential that
/// matches the value advertised in the server's SDP answer (`a=ice-ufrag:`).
fn parse_stun_local_ufrag(data: &[u8]) -> Option<String> {
    if data.len() < 20 {
        return None;
    }
    // STUN magic cookie must appear at bytes 4–7.
    if data[4..8] != [0x21, 0x12, 0xa4, 0x42] {
        return None;
    }
    // TLV attributes start at byte 20.
    let mut pos = 20;
    while pos + 4 <= data.len() {
        let attr_type = u16::from_be_bytes([data[pos], data[pos + 1]]);
        let attr_len = u16::from_be_bytes([data[pos + 2], data[pos + 3]]) as usize;
        pos += 4;
        if pos + attr_len > data.len() {
            break;
        }
        if attr_type == 0x0006 {
            // USERNAME attribute
            let username = std::str::from_utf8(&data[pos..pos + attr_len]).ok()?;
            // USERNAME = "<server_ufrag>:<browser_ufrag>" (responding:controlling per RFC 8445).
            // We route by the server's ufrag (first token), which matches a=ice-ufrag: in our SDP answer.
            return username.split(':').next().map(str::to_owned);
        }
        // Attributes are padded to 4-byte alignment.
        pos += (attr_len + 3) & !3;
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_stun_binding_request(username: &str) -> Vec<u8> {
        // Minimal STUN Binding Request with a USERNAME attribute.
        let attr_val = username.as_bytes();
        let attr_len = attr_val.len() as u16;
        let padded = (attr_val.len() + 3) & !3;
        let msg_len = (4 + padded) as u16;

        let mut pkt = Vec::new();
        // Type = 0x0001 (Binding Request), Length
        pkt.extend_from_slice(&[0x00, 0x01]);
        pkt.extend_from_slice(&msg_len.to_be_bytes());
        // Magic cookie
        pkt.extend_from_slice(&[0x21, 0x12, 0xa4, 0x42]);
        // Transaction ID (12 bytes)
        pkt.extend_from_slice(&[0u8; 12]);
        // USERNAME attribute
        pkt.extend_from_slice(&[0x00, 0x06]);
        pkt.extend_from_slice(&attr_len.to_be_bytes());
        pkt.extend_from_slice(attr_val);
        // Padding
        pkt.extend(std::iter::repeat_n(0x00, padded - attr_val.len()));
        pkt
    }

    #[test]
    fn parses_local_ufrag() {
        // USERNAME = "serverUfrag:browserUfrag"; we want the server's ufrag (first token).
        let pkt = make_stun_binding_request("serverUfrag:browserUfrag");
        assert_eq!(parse_stun_local_ufrag(&pkt), Some("serverUfrag".to_owned()));
    }

    #[test]
    fn returns_none_for_non_stun() {
        let pkt = vec![0u8; 40];
        assert_eq!(parse_stun_local_ufrag(&pkt), None);
    }

    #[test]
    fn returns_none_for_short_packet() {
        assert_eq!(parse_stun_local_ufrag(&[0u8; 10]), None);
    }
}
