/// NAL accumulator: buffers non-VCL NALs (SPS, PPS, SEI) until a VCL NAL
/// arrives, then yields all together as Annex-B.
///
/// str0m handles FU-A fragmentation — we feed it complete access units.
pub struct NalAccumulator {
    /// Buffered non-VCL NALs (without start codes).
    pending: Vec<Vec<u8>>,
}

impl NalAccumulator {
    pub fn new() -> Self {
        Self {
            pending: Vec::new(),
        }
    }

    /// Feed a NAL unit (without start code). Returns Some(access_unit) when a
    /// VCL NAL completes the access unit. The returned bytes are Annex-B
    /// formatted: `[00 00 00 01][NAL]...`
    pub fn push(&mut self, nal: &[u8]) -> Option<Vec<u8>> {
        if nal.is_empty() {
            return None;
        }

        let nal_type = nal[0] & 0x1F;

        // VCL NAL types: 1 (non-IDR slice), 5 (IDR slice)
        let is_vcl = nal_type == 1 || nal_type == 5;

        if !is_vcl {
            // Buffer non-VCL NALs (SPS=7, PPS=8, SEI=6, etc.)
            self.pending.push(nal.to_vec());
            return None;
        }

        // VCL NAL: emit all pending + this NAL as one Annex-B access unit
        let start_code = [0x00, 0x00, 0x00, 0x01];
        let mut au = Vec::new();

        for pending_nal in self.pending.drain(..) {
            au.extend_from_slice(&start_code);
            au.extend_from_slice(&pending_nal);
        }
        au.extend_from_slice(&start_code);
        au.extend_from_slice(nal);

        Some(au)
    }

}

impl Default for NalAccumulator {
    fn default() -> Self {
        Self::new()
    }
}

/// Extract individual NAL units from an Annex-B byte stream.
/// Returns a vec of NAL unit slices (without start codes).
pub fn parse_annex_b(data: &[u8]) -> Vec<&[u8]> {
    let mut nals = Vec::new();
    let mut i = 0;
    while i < data.len() {
        // Find start code (00 00 00 01 or 00 00 01)
        let sc_len = if i + 3 < data.len() && data[i] == 0 && data[i + 1] == 0 {
            if data[i + 2] == 1 {
                3
            } else if i + 3 < data.len() && data[i + 2] == 0 && data[i + 3] == 1 {
                4
            } else {
                i += 1;
                continue;
            }
        } else {
            i += 1;
            continue;
        };

        let nal_start = i + sc_len;
        // Find the next start code
        let mut nal_end = nal_start;
        while nal_end < data.len() {
            if nal_end + 2 < data.len()
                && data[nal_end] == 0
                && data[nal_end + 1] == 0
                && (data[nal_end + 2] == 1
                    || (nal_end + 3 < data.len()
                        && data[nal_end + 2] == 0
                        && data[nal_end + 3] == 1))
            {
                break;
            }
            nal_end += 1;
        }

        if nal_start < nal_end {
            nals.push(&data[nal_start..nal_end]);
        }
        i = nal_end;
    }
    nals
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nal_accumulator_vcl_only() {
        let mut acc = NalAccumulator::new();
        // NAL type 1 (non-IDR slice)
        let nal = vec![0x61, 0x01, 0x02];
        let au = acc.push(&nal).unwrap();
        assert!(au.starts_with(&[0, 0, 0, 1]));
    }

    #[test]
    fn nal_accumulator_sps_pps_idr() {
        let mut acc = NalAccumulator::new();
        let sps = vec![0x67, 0x01]; // NAL type 7
        let pps = vec![0x68, 0x01]; // NAL type 8
        let idr = vec![0x65, 0x01]; // NAL type 5

        assert!(acc.push(&sps).is_none());
        assert!(acc.push(&pps).is_none());
        let au = acc.push(&idr).unwrap();

        // Should contain SPS + PPS + IDR, each with start codes
        let nals = parse_annex_b(&au);
        assert_eq!(nals.len(), 3);
        assert_eq!(nals[0][0] & 0x1F, 7); // SPS
        assert_eq!(nals[1][0] & 0x1F, 8); // PPS
        assert_eq!(nals[2][0] & 0x1F, 5); // IDR
    }

}
