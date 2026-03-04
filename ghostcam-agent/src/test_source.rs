use anyhow::Result;
use bytes::Bytes;
use std::path::Path;

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
}
