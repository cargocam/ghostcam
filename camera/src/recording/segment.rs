use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::Result;
use tokio::io::AsyncWriteExt;

/// Metadata from a finalized segment.
#[derive(Debug, Clone)]
pub struct SegmentMetadata {
    pub segment_id: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
    pub path: PathBuf,
}

/// A buffered video sample.
struct VideoSample {
    /// NAL data in AVCC format (4-byte length prefix per NAL)
    data: Vec<u8>,
    /// Duration of this sample in 90kHz ticks
    duration_ticks: u32,
    /// Is this an IDR (keyframe)?
    is_idr: bool,
}

/// A buffered audio sample.
struct AudioSample {
    data: Vec<u8>,
    /// Duration in 48kHz ticks (960 = 20ms frame)
    duration_ticks: u32,
}

/// Writes a single fMP4 segment (.m4s) to disk.
///
/// Buffers video NALs and audio frames in memory, then writes a proper
/// ISO BMFF segment (styp + moof + mdat) on finalize.
pub struct SegmentWriter {
    segment_id: String,
    start_ts: u64,
    path: PathBuf,
    video_samples: Vec<VideoSample>,
    audio_samples: Vec<AudioSample>,
    last_video_ts: Duration,
    last_audio_ts: Duration,
    sequence_number: u32,
}

impl SegmentWriter {
    /// Create a new segment writer.
    pub async fn create(dir: &Path, segment_id: &str, start_ts: u64) -> Result<Self> {
        let path = dir.join(format!("{segment_id}.m4s"));

        Ok(Self {
            segment_id: segment_id.to_string(),
            start_ts,
            path,
            video_samples: Vec::new(),
            audio_samples: Vec::new(),
            last_video_ts: Duration::ZERO,
            last_audio_ts: Duration::ZERO,
            sequence_number: (start_ts / 1000) as u32, // Use timestamp as sequence
        })
    }

    /// Buffer a video NAL unit.
    pub async fn write_video(&mut self, nal: &[u8], timestamp: Duration) -> Result<()> {
        if nal.is_empty() {
            return Ok(());
        }

        let nal_type = nal[0] & 0x1F;
        let is_idr = nal_type == 5;

        // Convert to AVCC format (4-byte length prefix)
        let mut avcc_data = Vec::with_capacity(4 + nal.len());
        avcc_data.extend_from_slice(&(nal.len() as u32).to_be_bytes());
        avcc_data.extend_from_slice(nal);

        // Calculate duration since last sample (in 90kHz ticks)
        let duration_ticks = if self.video_samples.is_empty() {
            3000 // Default 33ms at 90kHz for first sample
        } else {
            let delta = timestamp.saturating_sub(self.last_video_ts);
            let ticks = (delta.as_micros() as u64 * 90000 / 1_000_000) as u32;
            if ticks == 0 { 3000 } else { ticks }
        };

        self.last_video_ts = timestamp;
        self.video_samples.push(VideoSample {
            data: avcc_data,
            duration_ticks,
            is_idr,
        });

        Ok(())
    }

    /// Buffer an audio frame.
    pub async fn write_audio(&mut self, frame: &[u8], timestamp: Duration) -> Result<()> {
        // Opus: 20ms frames = 960 samples at 48kHz
        let duration_ticks = 960u32;
        self.last_audio_ts = timestamp;
        self.audio_samples.push(AudioSample {
            data: frame.to_vec(),
            duration_ticks,
        });
        Ok(())
    }

    /// Current duration of the segment.
    pub fn duration(&self) -> Duration {
        self.last_video_ts
    }

    /// Finalize the segment: write proper fMP4 (styp + moof + mdat) to disk.
    pub async fn finalize(self) -> Result<SegmentMetadata> {
        let buf = self.build_fmp4_segment();
        let size = buf.len() as u64;

        let mut file = tokio::fs::File::create(&self.path).await?;
        file.write_all(&buf).await?;
        file.flush().await?;
        file.shutdown().await?;

        let end_ts = self.start_ts + self.last_video_ts.as_millis() as u64;

        Ok(SegmentMetadata {
            segment_id: self.segment_id,
            start_ts: self.start_ts,
            end_ts,
            size_bytes: size,
            path: self.path,
        })
    }

    fn build_fmp4_segment(&self) -> Vec<u8> {
        let mut buf = Vec::new();

        // styp box
        write_box(&mut buf, b"styp", |b| {
            b.extend_from_slice(b"msdh"); // major brand
            b.extend_from_slice(&0u32.to_be_bytes()); // minor version
            b.extend_from_slice(b"msdh");
            b.extend_from_slice(b"msix");
        });

        // Collect all mdat payloads and compute offsets
        let mut mdat_payload = Vec::new();
        let mut video_payload_size = 0u32;

        // Video samples first
        for sample in &self.video_samples {
            mdat_payload.extend_from_slice(&sample.data);
            video_payload_size += sample.data.len() as u32;
        }
        // Then audio samples
        for sample in &self.audio_samples {
            mdat_payload.extend_from_slice(&sample.data);
        }

        // We need the moof size to calculate data_offset in trun.
        // Build moof into a temporary buffer first to get its size.
        let moof_buf = self.build_moof(mdat_payload.len() as u32);

        // Now the data_offset in trun should point from traf start to mdat payload.
        // We need to rebuild with correct offsets. The data_offset field in trun is
        // relative to the start of the containing moof box + 8 (mdat header).
        // Actually, data_offset in trun is the offset from the start of the containing
        // moof to the first byte of data in mdat.
        // Let's just rebuild moof with the correct data_offset.
        let moof_size = moof_buf.len();
        let mdat_header_size = 8u32; // box size (4) + type (4)
        let video_data_offset_from_moof = moof_size as u32 + mdat_header_size;
        let audio_data_offset_from_moof = video_data_offset_from_moof + video_payload_size;

        // Rebuild moof with correct data_offsets
        let moof_buf = self.build_moof_with_offsets(
            video_data_offset_from_moof,
            audio_data_offset_from_moof,
        );

        buf.extend_from_slice(&moof_buf);

        // mdat box
        let mdat_size = (8 + mdat_payload.len()) as u32;
        buf.extend_from_slice(&mdat_size.to_be_bytes());
        buf.extend_from_slice(b"mdat");
        buf.extend_from_slice(&mdat_payload);

        buf
    }

    fn build_moof(&self, _mdat_payload_size: u32) -> Vec<u8> {
        self.build_moof_with_offsets(0, 0)
    }

    fn build_moof_with_offsets(
        &self,
        video_data_offset: u32,
        audio_data_offset: u32,
    ) -> Vec<u8> {
        let mut buf = Vec::new();

        write_box(&mut buf, b"moof", |moof| {
            // mfhd
            write_box(moof, b"mfhd", |b| {
                b.extend_from_slice(&[0; 4]); // version + flags
                b.extend_from_slice(&self.sequence_number.to_be_bytes());
            });

            // Video traf (track 1)
            if !self.video_samples.is_empty() {
                write_box(moof, b"traf", |traf| {
                    // tfhd
                    write_box(traf, b"tfhd", |b| {
                        // flags: default-base-is-moof (0x020000)
                        b.extend_from_slice(&[0, 0x02, 0x00, 0x00]);
                        b.extend_from_slice(&1u32.to_be_bytes()); // track_ID
                    });

                    // tfdt (track fragment decode time)
                    write_box(traf, b"tfdt", |b| {
                        b.push(1); // version=1 (64-bit decode time)
                        b.extend_from_slice(&[0; 3]); // flags
                        let base_decode_time = 0u64;
                        b.extend_from_slice(&base_decode_time.to_be_bytes());
                    });

                    // trun
                    self.write_video_trun(traf, video_data_offset);
                });
            }
            // Audio traf (track 2)
            if !self.audio_samples.is_empty() {
                write_box(moof, b"traf", |traf| {
                    // tfhd
                    write_box(traf, b"tfhd", |b| {
                        // flags: default-base-is-moof (0x020000)
                        b.extend_from_slice(&[0, 0x02, 0x00, 0x00]);
                        b.extend_from_slice(&2u32.to_be_bytes()); // track_ID
                    });

                    // tfdt (track fragment decode time)
                    write_box(traf, b"tfdt", |b| {
                        b.push(1); // version=1 (64-bit decode time)
                        b.extend_from_slice(&[0; 3]); // flags
                        let base_decode_time = 0u64;
                        b.extend_from_slice(&base_decode_time.to_be_bytes());
                    });

                    // trun
                    self.write_audio_trun(traf, audio_data_offset);
                });
            }

        });

        buf
    }

    fn write_video_trun(&self, traf: &mut Vec<u8>, data_offset: u32) {
        write_box(traf, b"trun", |b| {
            // flags: data-offset-present (0x01) | sample-duration (0x100) |
            //        sample-size (0x200) | sample-flags (0x400)
            let flags: u32 = 0x000001 | 0x000100 | 0x000200 | 0x000400;
            b.push(0); // version
            b.extend_from_slice(&flags.to_be_bytes()[1..4]); // 3-byte flags
            b.extend_from_slice(&(self.video_samples.len() as u32).to_be_bytes());
            b.extend_from_slice(&data_offset.to_be_bytes());

            for sample in &self.video_samples {
                b.extend_from_slice(&sample.duration_ticks.to_be_bytes());
                b.extend_from_slice(&(sample.data.len() as u32).to_be_bytes());
                // Sample flags: bit 16 = is_non_sync_sample
                let flags: u32 = if sample.is_idr {
                    0x02000000 // depends_on_nothing
                } else {
                    0x01010000 // depends_on_other | is_non_sync_sample
                };
                b.extend_from_slice(&flags.to_be_bytes());
            }
        });
    }

    fn write_audio_trun(&self, traf: &mut Vec<u8>, data_offset: u32) {
        write_box(traf, b"trun", |b| {
            // flags: data-offset-present (0x01) | sample-duration (0x100) | sample-size (0x200)
            let flags: u32 = 0x000001 | 0x000100 | 0x000200;
            b.push(0);
            b.extend_from_slice(&flags.to_be_bytes()[1..4]);
            b.extend_from_slice(&(self.audio_samples.len() as u32).to_be_bytes());
            b.extend_from_slice(&data_offset.to_be_bytes());

            for sample in &self.audio_samples {
                b.extend_from_slice(&sample.duration_ticks.to_be_bytes());
                b.extend_from_slice(&(sample.data.len() as u32).to_be_bytes());
            }
        });
    }
}

/// Write an ISO BMFF box: [4-byte size][4-byte type][content].
fn write_box(buf: &mut Vec<u8>, box_type: &[u8; 4], content: impl FnOnce(&mut Vec<u8>)) {
    let start = buf.len();
    buf.extend_from_slice(&[0; 4]); // placeholder for size
    buf.extend_from_slice(box_type);
    content(buf);
    let size = (buf.len() - start) as u32;
    buf[start..start + 4].copy_from_slice(&size.to_be_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn create_segment() {
        let dir = tempfile::tempdir().unwrap();
        let _sw = SegmentWriter::create(dir.path(), "test-seg", 1000)
            .await
            .unwrap();
        // File is not created until finalize
    }

    #[tokio::test]
    async fn write_and_finalize_video_only() {
        let dir = tempfile::tempdir().unwrap();
        let mut sw = SegmentWriter::create(dir.path(), "seg1", 0)
            .await
            .unwrap();
        // IDR NAL (type 5)
        sw.write_video(&[0x65, 0x00, 0x01, 0x02], Duration::from_millis(0))
            .await
            .unwrap();
        // Non-IDR (type 1)
        sw.write_video(&[0x41, 0x00, 0x01], Duration::from_millis(33))
            .await
            .unwrap();

        let meta = sw.finalize().await.unwrap();
        assert_eq!(meta.segment_id, "seg1");
        assert!(meta.size_bytes > 0);
        assert!(meta.path.exists());

        // Verify it starts with styp box
        let data = tokio::fs::read(&meta.path).await.unwrap();
        assert_eq!(&data[4..8], b"styp");
    }

    #[tokio::test]
    async fn finalize_contains_moof_mdat() {
        let dir = tempfile::tempdir().unwrap();
        let mut sw = SegmentWriter::create(dir.path(), "seg1", 5000)
            .await
            .unwrap();
        sw.write_video(&[0x65, 0xFF], Duration::from_millis(0))
            .await
            .unwrap();
        sw.write_audio(&[0xF8, 0xFF, 0xFE], Duration::from_millis(0))
            .await
            .unwrap();

        let meta = sw.finalize().await.unwrap();
        let data = tokio::fs::read(&meta.path).await.unwrap();

        // Check for moof and mdat box types
        let has_moof = data.windows(4).any(|w| w == b"moof");
        let has_mdat = data.windows(4).any(|w| w == b"mdat");
        assert!(has_moof, "segment should contain moof box");
        assert!(has_mdat, "segment should contain mdat box");
    }

    #[tokio::test]
    async fn finalize_returns_correct_timestamps() {
        let dir = tempfile::tempdir().unwrap();
        let mut sw = SegmentWriter::create(dir.path(), "seg1", 5000)
            .await
            .unwrap();
        sw.write_video(b"\x65\x00", Duration::from_secs(0))
            .await
            .unwrap();
        sw.write_video(b"\x41\x00", Duration::from_secs(10))
            .await
            .unwrap();

        let meta = sw.finalize().await.unwrap();
        assert_eq!(meta.start_ts, 5000);
        assert_eq!(meta.end_ts, 15000); // 5000 + 10000ms
    }
}
