use anyhow::Result;
use bytes::Bytes;
use std::path::Path;

const NAL_BUFFER_LIMIT: usize = 2 * 1024 * 1024; // 2MB

/// Streaming NAL parser: buffers incoming byte chunks from a pipe (e.g. rpicam-vid stdout)
/// and emits complete NAL units as they are delimited by Annex-B start codes.
pub struct NalParser {
    buf: Vec<u8>,
    /// Position in buf where the current NAL data starts (after start code)
    nal_start: Option<usize>,
}

impl NalParser {
    pub fn new() -> Self {
        Self {
            buf: Vec::with_capacity(64 * 1024),
            nal_start: None,
        }
    }

    /// Feed a chunk of data and return any complete NAL units found.
    /// NALs are returned without start codes.
    pub fn feed(&mut self, data: &[u8]) -> Vec<Bytes> {
        self.buf.extend_from_slice(data);

        // Safety: reset on overflow to prevent unbounded growth
        if self.buf.len() > NAL_BUFFER_LIMIT {
            tracing::warn!(buf_len = self.buf.len(), "NAL parser buffer overflow, resetting");
            self.buf.clear();
            self.nal_start = None;
            return Vec::new();
        }

        let mut nals = Vec::new();
        let mut search_from = if self.nal_start.is_some() {
            // We have an in-progress NAL; search for the next start code
            // after the current NAL start, but we need at least a few bytes
            self.nal_start.unwrap().max(1)
        } else {
            0
        };

        loop {
            match find_start_code(&self.buf, search_from) {
                Some((sc_pos, sc_len)) => {
                    if let Some(nal_start) = self.nal_start {
                        // We found a new start code — everything from nal_start to sc_pos is a complete NAL
                        let nal_data = &self.buf[nal_start..sc_pos];
                        if !nal_data.is_empty() {
                            nals.push(Bytes::copy_from_slice(nal_data));
                        }
                    }
                    // New NAL starts after this start code
                    self.nal_start = Some(sc_pos + sc_len);
                    search_from = sc_pos + sc_len;
                }
                None => break,
            }
        }

        // Compact buffer: keep only from nal_start (or from search_from if no NAL in progress)
        if let Some(ns) = self.nal_start {
            if ns > 0 {
                self.buf.drain(..ns);
                self.nal_start = Some(0);
            }
        } else if !self.buf.is_empty() {
            // No start code found yet — keep last 3 bytes for partial start code detection
            let keep = self.buf.len().min(3);
            let drain_to = self.buf.len() - keep;
            if drain_to > 0 {
                self.buf.drain(..drain_to);
            }
        }

        nals
    }

    /// Flush any remaining NAL data (e.g. at end of stream).
    pub fn flush(&mut self) -> Option<Bytes> {
        if let Some(nal_start) = self.nal_start.take() {
            let nal_data = &self.buf[nal_start..];
            if !nal_data.is_empty() {
                let nal = Bytes::copy_from_slice(nal_data);
                self.buf.clear();
                return Some(nal);
            }
        }
        self.buf.clear();
        None
    }
}

/// Parse a raw H.264 Annex-B file into individual NAL units.
/// Scans for start codes (00 00 00 01 or 00 00 01) and splits.
pub fn parse_h264_file(path: &Path) -> Result<Vec<Bytes>> {
    let data = std::fs::read(path)?;
    let mut nals = Vec::new();

    // Find first start code
    let first = find_start_code(&data, 0);
    if first.is_none() {
        anyhow::bail!("no H.264 start codes found in file");
    }

    let (start_pos, sc_len) = first.unwrap();
    let mut nal_start = start_pos + sc_len;

    loop {
        match find_start_code(&data, nal_start) {
            Some((next_sc_pos, next_sc_len)) => {
                // NAL data is from nal_start to next_sc_pos
                let nal_data = &data[nal_start..next_sc_pos];
                if !nal_data.is_empty() {
                    nals.push(Bytes::copy_from_slice(nal_data));
                }
                nal_start = next_sc_pos + next_sc_len;
            }
            None => {
                // Last NAL extends to end of file
                let nal_data = &data[nal_start..];
                if !nal_data.is_empty() {
                    nals.push(Bytes::copy_from_slice(nal_data));
                }
                break;
            }
        }
    }

    Ok(nals)
}

/// Find next Annex-B start code (00 00 00 01 or 00 00 01) starting at `from`.
/// Returns (position, start_code_length).
fn find_start_code(data: &[u8], from: usize) -> Option<(usize, usize)> {
    let mut i = from;
    while i + 2 < data.len() {
        if data[i] == 0 && data[i + 1] == 0 {
            if i + 3 < data.len() && data[i + 2] == 0 && data[i + 3] == 1 {
                return Some((i, 4));
            }
            if data[i + 2] == 1 {
                return Some((i, 3));
            }
        }
        i += 1;
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_nals() {
        // Two NALs with 4-byte start codes
        let data = vec![
            0, 0, 0, 1, 0x67, 0x42, // SPS NAL
            0, 0, 0, 1, 0x68, 0x00, // PPS NAL
        ];
        let tmp = std::env::temp_dir().join("test_parse_nals.h264");
        std::fs::write(&tmp, &data).unwrap();
        let nals = parse_h264_file(&tmp).unwrap();
        assert_eq!(nals.len(), 2);
        assert_eq!(nals[0][0] & 0x1F, 7); // SPS
        assert_eq!(nals[1][0] & 0x1F, 8); // PPS
        std::fs::remove_file(&tmp).ok();
    }

    #[test]
    fn nal_parser_single_feed() {
        let mut parser = NalParser::new();
        // Feed a complete stream: two NALs
        let data = vec![
            0, 0, 0, 1, 0x67, 0x42, 0x00, // SPS
            0, 0, 0, 1, 0x68, 0xCE,        // PPS
        ];
        let nals = parser.feed(&data);
        // First NAL is complete (delimited by second start code)
        assert_eq!(nals.len(), 1);
        assert_eq!(nals[0][0] & 0x1F, 7); // SPS

        // Flush to get the last NAL
        let last = parser.flush();
        assert!(last.is_some());
        assert_eq!(last.unwrap()[0] & 0x1F, 8); // PPS
    }

    #[test]
    fn nal_parser_partial_chunks() {
        let mut parser = NalParser::new();

        // Feed data in small chunks that split across start codes
        let nals1 = parser.feed(&[0, 0, 0, 1, 0x67, 0x42]); // start code + partial SPS
        assert!(nals1.is_empty()); // no complete NAL yet

        let nals2 = parser.feed(&[0x00, 0x0A, 0, 0]); // more SPS data + partial start code
        assert!(nals2.is_empty());

        let nals3 = parser.feed(&[0, 1, 0x68, 0xCE]); // complete start code + PPS
        assert_eq!(nals3.len(), 1);
        assert_eq!(nals3[0][0] & 0x1F, 7); // SPS complete

        let nals4 = parser.feed(&[0, 0, 0, 1, 0x65, 0xFF]); // IDR
        assert_eq!(nals4.len(), 1);
        assert_eq!(nals4[0][0] & 0x1F, 8); // PPS complete

        let last = parser.flush();
        assert!(last.is_some());
        assert_eq!(last.unwrap()[0] & 0x1F, 5); // IDR
    }

    #[test]
    fn nal_parser_three_byte_start_codes() {
        let mut parser = NalParser::new();
        let data = vec![
            0, 0, 1, 0x67, 0x42, // 3-byte start code + SPS
            0, 0, 1, 0x68, 0xCE, // 3-byte start code + PPS
        ];
        let nals = parser.feed(&data);
        assert_eq!(nals.len(), 1);
        assert_eq!(nals[0][0] & 0x1F, 7);
        let last = parser.flush().unwrap();
        assert_eq!(last[0] & 0x1F, 8);
    }
}
