use super::ring_buffer::SegmentInfo;

/// Maximum segments to include in the HLS manifest.
/// The full ring buffer may contain thousands of segments; the manifest only
/// needs the most recent ones for live playback. The coverage API provides
/// the complete segment list for the timeline scrubber.
const MAX_MANIFEST_SEGMENTS: usize = 120; // ~10 minutes at 5s segments

/// Generate an HLS v7 manifest from the current ring buffer contents.
/// Only includes the most recent `MAX_MANIFEST_SEGMENTS` to keep the
/// manifest small (~12KB instead of ~400KB).
pub fn generate_manifest(segments: &[SegmentInfo]) -> String {
    let recent = if segments.len() > MAX_MANIFEST_SEGMENTS {
        &segments[segments.len() - MAX_MANIFEST_SEGMENTS..]
    } else {
        segments
    };

    let mut m = String::new();
    let init_version = recent.last().map(|s| s.start_ts).unwrap_or(0);
    m.push_str("#EXTM3U\n");
    m.push_str("#EXT-X-VERSION:7\n");
    m.push_str("#EXT-X-TARGETDURATION:10\n");
    m.push_str(&format!("#EXT-X-MAP:URI=\"init.mp4?v={init_version}\"\n"));

    for seg in recent {
        let duration = (seg.end_ts - seg.start_ts) as f64 / 1000.0;
        m.push_str(&format!("#EXTINF:{:.1},\n", duration));
        m.push_str(&format!("./{}.m4s\n", seg.segment_id));
    }

    // No #EXT-X-ENDLIST — open-ended rolling buffer
    m
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn make_seg(id: &str, start: u64, end: u64, _size: u64) -> SegmentInfo {
        SegmentInfo {
            segment_id: id.to_string(),
            start_ts: start,
            end_ts: end,
            path: PathBuf::from(format!("{id}.m4s")),
        }
    }

    #[test]
    fn empty_segments() {
        let m = generate_manifest(&[]);
        assert!(m.contains("#EXTM3U"));
        assert!(m.contains("#EXT-X-VERSION:7"));
        assert!(!m.contains("#EXTINF"));
    }

    #[test]
    fn single_segment() {
        let segs = [make_seg("cam-01:1000", 1000, 11000, 500000)];
        let m = generate_manifest(&segs);
        assert!(m.contains("#EXTINF:10.0,"));
        assert!(m.contains("./cam-01:1000.m4s"));
    }

    #[test]
    fn multiple_segments() {
        let segs = (0..5)
            .map(|i| {
                let start = i * 10000;
                make_seg(&format!("cam:{}", start), start, start + 10000, 500000)
            })
            .collect::<Vec<_>>();
        let m = generate_manifest(&segs);
        let count = m.matches("#EXTINF").count();
        assert_eq!(count, 5);
    }

    #[test]
    fn manifest_has_version_7() {
        let m = generate_manifest(&[]);
        assert!(m.contains("#EXT-X-VERSION:7"));
    }

    #[test]
    fn manifest_has_target_duration() {
        let m = generate_manifest(&[]);
        assert!(m.contains("#EXT-X-TARGETDURATION:10"));
    }

    #[test]
    fn manifest_has_init_map() {
        let m = generate_manifest(&[]);
        assert!(m.contains("#EXT-X-MAP:URI=\"init.mp4?v=0\""));
    }

    #[test]
    fn manifest_no_endlist() {
        let segs = [make_seg("s1", 0, 10000, 100)];
        let m = generate_manifest(&segs);
        assert!(!m.contains("#EXT-X-ENDLIST"));
    }

    #[test]
    fn segment_ids_as_filenames() {
        let segs = [
            make_seg("seg-001", 0, 10000, 100),
            make_seg("seg-002", 10000, 20000, 100),
        ];
        let m = generate_manifest(&segs);
        assert!(m.contains("./seg-001.m4s"));
        assert!(m.contains("./seg-002.m4s"));
    }

    #[test]
    fn segment_ids_with_colons_are_explicitly_relative() {
        let segs = [make_seg("cam-01:1774114781812", 0, 10000, 100)];
        let m = generate_manifest(&segs);
        assert!(m.contains("./cam-01:1774114781812.m4s"));
    }
}
