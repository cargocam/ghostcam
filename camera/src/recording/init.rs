use anyhow::Result;
use bytes::Bytes;

/// Generate an fMP4 init segment (moov box) from codec parameters.
///
/// This generates a minimal ISO BMFF init segment containing:
/// - ftyp box (isom, iso6)
/// - moov box with video track (H.264)
///
/// For now, this produces a simplified init segment. A full ISO BMFF
/// implementation with proper box construction will replace this.
pub fn generate_init_segment(sps: &[u8], pps: &[u8]) -> Result<Bytes> {
    let mut buf = Vec::new();

    // ftyp box
    write_box(&mut buf, b"ftyp", |b| {
        b.extend_from_slice(b"isom");   // major brand
        b.extend_from_slice(&0u32.to_be_bytes()); // minor version
        b.extend_from_slice(b"isom");   // compatible brand
        b.extend_from_slice(b"iso6");   // compatible brand
        b.extend_from_slice(b"msdh");   // compatible brand
    });

    // moov box
    write_box(&mut buf, b"moov", |moov| {
        // mvhd
        write_box(moov, b"mvhd", |b| {
            b.extend_from_slice(&[0; 4]); // version + flags
            b.extend_from_slice(&0u32.to_be_bytes()); // creation time
            b.extend_from_slice(&0u32.to_be_bytes()); // modification time
            b.extend_from_slice(&90000u32.to_be_bytes()); // timescale
            b.extend_from_slice(&0u32.to_be_bytes()); // duration
            b.extend_from_slice(&0x00010000u32.to_be_bytes()); // rate 1.0
            b.extend_from_slice(&0x0100u16.to_be_bytes()); // volume 1.0
            b.extend_from_slice(&[0; 10]); // reserved
            // Identity matrix
            for &val in &[
                0x00010000u32, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000,
            ] {
                b.extend_from_slice(&val.to_be_bytes());
            }
            b.extend_from_slice(&[0; 24]); // pre-defined
            b.extend_from_slice(&3u32.to_be_bytes()); // next_track_ID
        });

        // Video track
        write_video_trak(moov, sps, pps);
        // Audio track (Opus)
        write_audio_trak(moov);

        // mvex
        write_box(moov, b"mvex", |mvex| {
            // trex for video (track 1)
            write_box(mvex, b"trex", |b| {
                b.extend_from_slice(&[0; 4]); // version + flags
                b.extend_from_slice(&1u32.to_be_bytes()); // track_ID
                b.extend_from_slice(&1u32.to_be_bytes()); // default_sample_description_index
                b.extend_from_slice(&0u32.to_be_bytes()); // default_sample_duration
                b.extend_from_slice(&0u32.to_be_bytes()); // default_sample_size
                b.extend_from_slice(&0u32.to_be_bytes()); // default_sample_flags
            });
            // trex for audio (track 2)
            write_box(mvex, b"trex", |b| {
                b.extend_from_slice(&[0; 4]); // version + flags
                b.extend_from_slice(&2u32.to_be_bytes()); // track_ID
                b.extend_from_slice(&1u32.to_be_bytes()); // default_sample_description_index
                b.extend_from_slice(&960u32.to_be_bytes()); // default_sample_duration (20ms @ 48kHz)
                b.extend_from_slice(&0u32.to_be_bytes()); // default_sample_size
                b.extend_from_slice(&0u32.to_be_bytes()); // default_sample_flags
            });
        });
    });

    Ok(Bytes::from(buf))
}

fn write_video_trak(moov: &mut Vec<u8>, sps: &[u8], pps: &[u8]) {
    write_box(moov, b"trak", |trak| {
        // tkhd
        write_box(trak, b"tkhd", |b| {
            b.extend_from_slice(&[0, 0, 0, 3]); // version=0, flags=3 (enabled+in_movie)
            b.extend_from_slice(&0u32.to_be_bytes()); // creation time
            b.extend_from_slice(&0u32.to_be_bytes()); // modification time
            b.extend_from_slice(&1u32.to_be_bytes()); // track_ID
            b.extend_from_slice(&0u32.to_be_bytes()); // reserved
            b.extend_from_slice(&0u32.to_be_bytes()); // duration
            b.extend_from_slice(&[0; 8]); // reserved
            b.extend_from_slice(&0u16.to_be_bytes()); // layer
            b.extend_from_slice(&0u16.to_be_bytes()); // alternate_group
            b.extend_from_slice(&0u16.to_be_bytes()); // volume (0 for video)
            b.extend_from_slice(&0u16.to_be_bytes()); // reserved
            for &val in &[
                0x00010000u32, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000,
            ] {
                b.extend_from_slice(&val.to_be_bytes());
            }
            // width/height in fixed-point 16.16
            b.extend_from_slice(&((640u32) << 16).to_be_bytes());
            b.extend_from_slice(&((480u32) << 16).to_be_bytes());
        });

        // mdia
        write_box(trak, b"mdia", |mdia| {
            // mdhd
            write_box(mdia, b"mdhd", |b| {
                b.extend_from_slice(&[0; 4]); // version + flags
                b.extend_from_slice(&0u32.to_be_bytes()); // creation
                b.extend_from_slice(&0u32.to_be_bytes()); // modification
                b.extend_from_slice(&90000u32.to_be_bytes()); // timescale
                b.extend_from_slice(&0u32.to_be_bytes()); // duration
                b.extend_from_slice(&0x55C40000u32.to_be_bytes()); // language (und) + pre_defined
            });

            // hdlr
            write_box(mdia, b"hdlr", |b| {
                b.extend_from_slice(&[0; 4]); // version + flags
                b.extend_from_slice(&0u32.to_be_bytes()); // pre_defined
                b.extend_from_slice(b"vide"); // handler_type
                b.extend_from_slice(&[0; 12]); // reserved
                b.extend_from_slice(b"VideoHandler\0");
            });

            // minf → stbl
            write_box(mdia, b"minf", |minf| {
                // vmhd
                write_box(minf, b"vmhd", |b| {
                    b.extend_from_slice(&[0, 0, 0, 1]); // version=0, flags=1
                    b.extend_from_slice(&0u16.to_be_bytes()); // graphicsmode
                    b.extend_from_slice(&[0; 6]); // opcolor
                });

                // dinf → dref
                write_box(minf, b"dinf", |dinf| {
                    write_box(dinf, b"dref", |b| {
                        b.extend_from_slice(&[0; 4]); // version + flags
                        b.extend_from_slice(&1u32.to_be_bytes()); // entry_count
                        write_box(b, b"url ", |u| {
                            u.extend_from_slice(&[0, 0, 0, 1]); // flags=1 (self-contained)
                        });
                    });
                });

                // stbl
                write_box(minf, b"stbl", |stbl| {
                    // stsd with avc1
                    write_box(stbl, b"stsd", |b| {
                        b.extend_from_slice(&[0; 4]); // version + flags
                        b.extend_from_slice(&1u32.to_be_bytes()); // entry_count
                        write_avc1_entry(b, sps, pps);
                    });

                    // Empty required boxes
                    write_box(stbl, b"stts", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                    write_box(stbl, b"stsc", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                    write_box(stbl, b"stsz", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes()); // sample_size
                        b.extend_from_slice(&0u32.to_be_bytes()); // sample_count
                    });
                    write_box(stbl, b"stco", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                });
            });
        });
    });
}

fn write_avc1_entry(buf: &mut Vec<u8>, sps: &[u8], pps: &[u8]) {
    write_box(buf, b"avc1", |b| {
        b.extend_from_slice(&[0; 6]); // reserved
        b.extend_from_slice(&1u16.to_be_bytes()); // data_reference_index
        b.extend_from_slice(&[0; 16]); // pre_defined + reserved
        b.extend_from_slice(&640u16.to_be_bytes()); // width
        b.extend_from_slice(&480u16.to_be_bytes()); // height
        b.extend_from_slice(&0x00480000u32.to_be_bytes()); // horizresolution 72dpi
        b.extend_from_slice(&0x00480000u32.to_be_bytes()); // vertresolution 72dpi
        b.extend_from_slice(&0u32.to_be_bytes()); // reserved
        b.extend_from_slice(&1u16.to_be_bytes()); // frame_count
        b.extend_from_slice(&[0; 32]); // compressorname
        b.extend_from_slice(&0x0018u16.to_be_bytes()); // depth
        b.extend_from_slice(&(-1i16).to_be_bytes()); // pre_defined

        // avcC box
        write_box(b, b"avcC", |c| {
            c.push(1); // configurationVersion
            c.push(if sps.len() > 1 { sps[1] } else { 66 }); // AVCProfileIndication
            c.push(if sps.len() > 2 { sps[2] } else { 0 }); // profile_compatibility
            c.push(if sps.len() > 3 { sps[3] } else { 30 }); // AVCLevelIndication
            c.push(0xFF); // lengthSizeMinusOne = 3 (4-byte NAL lengths)
            c.push(0xE1); // numOfSequenceParameterSets = 1
            c.extend_from_slice(&(sps.len() as u16).to_be_bytes());
            c.extend_from_slice(sps);
            c.push(1); // numOfPictureParameterSets = 1
            c.extend_from_slice(&(pps.len() as u16).to_be_bytes());
            c.extend_from_slice(pps);
        });
    });
}

fn write_audio_trak(moov: &mut Vec<u8>) {
    write_box(moov, b"trak", |trak| {
        // tkhd
        write_box(trak, b"tkhd", |b| {
            b.extend_from_slice(&[0, 0, 0, 3]); // version=0, flags=3
            b.extend_from_slice(&0u32.to_be_bytes());
            b.extend_from_slice(&0u32.to_be_bytes());
            b.extend_from_slice(&2u32.to_be_bytes()); // track_ID = 2
            b.extend_from_slice(&0u32.to_be_bytes()); // reserved
            b.extend_from_slice(&0u32.to_be_bytes()); // duration
            b.extend_from_slice(&[0; 8]);
            b.extend_from_slice(&0u16.to_be_bytes()); // layer
            b.extend_from_slice(&0u16.to_be_bytes()); // alternate_group
            b.extend_from_slice(&0x0100u16.to_be_bytes()); // volume 1.0
            b.extend_from_slice(&0u16.to_be_bytes()); // reserved
            for &val in &[
                0x00010000u32, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000,
            ] {
                b.extend_from_slice(&val.to_be_bytes());
            }
            b.extend_from_slice(&0u32.to_be_bytes()); // width
            b.extend_from_slice(&0u32.to_be_bytes()); // height
        });

        // mdia
        write_box(trak, b"mdia", |mdia| {
            write_box(mdia, b"mdhd", |b| {
                b.extend_from_slice(&[0; 4]);
                b.extend_from_slice(&0u32.to_be_bytes());
                b.extend_from_slice(&0u32.to_be_bytes());
                b.extend_from_slice(&48000u32.to_be_bytes()); // timescale
                b.extend_from_slice(&0u32.to_be_bytes());
                b.extend_from_slice(&0x55C40000u32.to_be_bytes());
            });

            write_box(mdia, b"hdlr", |b| {
                b.extend_from_slice(&[0; 4]);
                b.extend_from_slice(&0u32.to_be_bytes());
                b.extend_from_slice(b"soun");
                b.extend_from_slice(&[0; 12]);
                b.extend_from_slice(b"AudioHandler\0");
            });

            write_box(mdia, b"minf", |minf| {
                write_box(minf, b"smhd", |b| {
                    b.extend_from_slice(&[0; 4]);
                    b.extend_from_slice(&0u16.to_be_bytes()); // balance
                    b.extend_from_slice(&0u16.to_be_bytes()); // reserved
                });

                write_box(minf, b"dinf", |dinf| {
                    write_box(dinf, b"dref", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&1u32.to_be_bytes());
                        write_box(b, b"url ", |u| {
                            u.extend_from_slice(&[0, 0, 0, 1]);
                        });
                    });
                });

                write_box(minf, b"stbl", |stbl| {
                    write_box(stbl, b"stsd", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&1u32.to_be_bytes());
                        // Opus sample entry
                        write_box(b, b"Opus", |o| {
                            o.extend_from_slice(&[0; 6]); // reserved
                            o.extend_from_slice(&1u16.to_be_bytes()); // data_reference_index
                            o.extend_from_slice(&[0; 8]); // reserved
                            o.extend_from_slice(&1u16.to_be_bytes()); // channelcount
                            o.extend_from_slice(&16u16.to_be_bytes()); // samplesize
                            o.extend_from_slice(&0u16.to_be_bytes()); // pre_defined
                            o.extend_from_slice(&0u16.to_be_bytes()); // reserved
                            o.extend_from_slice(&(48000u32 << 16).to_be_bytes()); // samplerate (16.16 fixed-point)

                            // dOps box
                            write_box(o, b"dOps", |d| {
                                d.push(0); // Version
                                d.push(1); // OutputChannelCount
                                d.extend_from_slice(&3840u16.to_be_bytes()); // PreSkip
                                d.extend_from_slice(&48000u32.to_be_bytes()); // InputSampleRate
                                d.extend_from_slice(&0i16.to_be_bytes()); // OutputGain
                                d.push(0); // ChannelMappingFamily
                            });
                        });
                    });

                    write_box(stbl, b"stts", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                    write_box(stbl, b"stsc", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                    write_box(stbl, b"stsz", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                    write_box(stbl, b"stco", |b| {
                        b.extend_from_slice(&[0; 4]);
                        b.extend_from_slice(&0u32.to_be_bytes());
                    });
                });
            });
        });
    });
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

    fn find_box(data: &[u8], box_type: &[u8; 4]) -> bool {
        // Simple window scan: look for the 4-byte box type tag anywhere in the data.
        // This works because box types are at offset +4 from each box start, but for
        // deeply nested boxes the recursive approach needs special handling for fullbox
        // headers. A byte-scan is sufficient for testing.
        data.windows(4).any(|w| w == box_type)
    }

    #[test]
    fn generate_init_from_sps_pps() {
        let sps = vec![0x67, 0x42, 0x00, 0x1E, 0xAB, 0x40];
        let pps = vec![0x68, 0xCE, 0x38, 0x80];
        let init = generate_init_segment(&sps, &pps).unwrap();
        assert!(!init.is_empty());
    }

    #[test]
    fn init_starts_with_ftyp() {
        let init = generate_init_segment(&[0x67, 0x42], &[0x68]).unwrap();
        assert_eq!(&init[4..8], b"ftyp");
    }

    #[test]
    fn init_contains_moov() {
        let init = generate_init_segment(&[0x67, 0x42], &[0x68]).unwrap();
        assert!(find_box(&init, b"moov"));
    }

    #[test]
    fn init_contains_video_track() {
        let init = generate_init_segment(&[0x67, 0x42], &[0x68]).unwrap();
        assert!(find_box(&init, b"avc1"));
    }

    #[test]
    fn init_contains_audio_track() {
        let init = generate_init_segment(&[0x67, 0x42], &[0x68]).unwrap();
        assert!(find_box(&init, b"Opus"));
        assert!(find_box(&init, b"soun"));
    }

    #[test]
    fn init_contains_mvex() {
        let init = generate_init_segment(&[0x67, 0x42], &[0x68]).unwrap();
        assert!(find_box(&init, b"mvex"));
        assert!(find_box(&init, b"trex"));
    }
}
