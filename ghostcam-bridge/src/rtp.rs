use bytes::Bytes;
use ghostcam_common::config::DEFAULT_MTU;

/// Convert microsecond timestamp to H.264 RTP timestamp (90kHz clock).
pub fn h264_rtp_timestamp(timestamp_us: u64) -> u32 {
    ((timestamp_us * 90_000 + 500_000) / 1_000_000) as u32
}

/// Convert microsecond timestamp to Opus RTP timestamp (48kHz clock).
pub fn opus_rtp_timestamp(timestamp_us: u64) -> u32 {
    ((timestamp_us * 48_000 + 500_000) / 1_000_000) as u32
}

/// Packetize a single H.264 NAL unit for RTP (RFC 6184).
/// Returns a list of RTP payloads (without RTP header — str0m handles that).
/// Each payload is either a Single NAL Unit Packet or an FU-A fragment.
pub fn packetize_h264_nal(nal: &[u8], is_last_nal_in_au: bool) -> Vec<(Bytes, bool)> {
    if nal.is_empty() {
        return vec![];
    }

    let max_payload = DEFAULT_MTU - 12; // RTP header is 12 bytes

    if nal.len() <= max_payload {
        // Single NAL Unit Packet
        return vec![(Bytes::copy_from_slice(nal), is_last_nal_in_au)];
    }

    // FU-A fragmentation
    let nal_header = nal[0];
    let nri = nal_header & 0x60;
    let nal_type = nal_header & 0x1F;

    let mut fragments = Vec::new();
    let fu_header_size = 2; // FU indicator + FU header
    let frag_payload_max = max_payload - fu_header_size;
    let nal_data = &nal[1..]; // Skip original NAL header

    let mut offset = 0;
    let mut is_first = true;

    while offset < nal_data.len() {
        let remaining = nal_data.len() - offset;
        let chunk_size = remaining.min(frag_payload_max);
        let is_last = offset + chunk_size >= nal_data.len();

        // FU indicator: same NRI as original, type = 28 (FU-A)
        let fu_indicator = nri | 28;

        // FU header: S bit (start), E bit (end), original NAL type
        let mut fu_header = nal_type;
        if is_first {
            fu_header |= 0x80; // S bit
        }
        if is_last {
            fu_header |= 0x40; // E bit
        }

        let mut payload = Vec::with_capacity(fu_header_size + chunk_size);
        payload.push(fu_indicator);
        payload.push(fu_header);
        payload.extend_from_slice(&nal_data[offset..offset + chunk_size]);

        let marker = is_last && is_last_nal_in_au;
        fragments.push((Bytes::from(payload), marker));

        offset += chunk_size;
        is_first = false;
    }

    fragments
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn small_nal_single_packet() {
        let nal = vec![0x67, 0x42, 0x00, 0x1e]; // SPS
        let packets = packetize_h264_nal(&nal, true);
        assert_eq!(packets.len(), 1);
        assert!(packets[0].1); // marker
        assert_eq!(&packets[0].0[..], &nal[..]);
    }

    #[test]
    fn large_nal_fu_a() {
        let mut nal = vec![0x65]; // IDR
        nal.extend(vec![0xAB; 5000]); // Large payload
        let packets = packetize_h264_nal(&nal, true);
        assert!(packets.len() > 1);
        // First should have S bit
        assert_eq!(packets[0].0[1] & 0x80, 0x80);
        // Last should have E bit and marker
        let last = packets.last().unwrap();
        assert_eq!(last.0[1] & 0x40, 0x40);
        assert!(last.1);
    }

    #[test]
    fn timestamp_conversion() {
        assert_eq!(h264_rtp_timestamp(0), 0);
        assert_eq!(h264_rtp_timestamp(1_000_000), 90_000);
        assert_eq!(h264_rtp_timestamp(33_333), 3_000); // ~30fps
    }
}
