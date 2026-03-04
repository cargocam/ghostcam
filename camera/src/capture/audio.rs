use anyhow::{Context, Result};
use bytes::Bytes;
use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use std::sync::{Arc, Mutex};
use tokio::sync::mpsc;
use tracing::{info, warn};

use super::CaptureMessage;

const OPUS_SAMPLE_RATE: u32 = 48_000;
const OPUS_FRAME_SAMPLES: usize = 960; // 20ms at 48kHz

/// Start audio capture from the default input device.
/// Opus-encoded frames are sent on `tx` as `CaptureMessage::Audio`.
/// Returns Err if no audio device is available — caller should treat this as non-fatal.
pub fn start(tx: mpsc::Sender<CaptureMessage>) -> Result<()> {
    let host = cpal::default_host();
    let device = host
        .default_input_device()
        .context("no audio input device found")?;

    let device_name = device.name().unwrap_or_else(|_| "unknown".into());
    info!(device = %device_name, "using audio input device");

    let supported_config = device
        .default_input_config()
        .context("no supported input config")?;

    let source_sample_rate = supported_config.sample_rate().0;
    let source_channels = supported_config.channels() as usize;

    info!(
        sample_rate = source_sample_rate,
        channels = source_channels,
        format = ?supported_config.sample_format(),
        "audio input config"
    );

    // Opus encoder — created in the audio thread
    let accumulator = Arc::new(Mutex::new(AudioAccumulator::new(
        source_sample_rate,
        source_channels,
    )));

    let acc_for_callback = accumulator.clone();
    let tx_for_thread = tx.clone();

    // cpal streams are !Send, so we need a dedicated OS thread
    std::thread::Builder::new()
        .name("audio-capture".into())
        .spawn(move || {
            let encoder = match opus::Encoder::new(
                OPUS_SAMPLE_RATE,
                opus::Channels::Mono,
                opus::Application::Audio,
            ) {
                Ok(enc) => enc,
                Err(e) => {
                    warn!(error = %e, "failed to create Opus encoder");
                    return;
                }
            };
            let encoder = Arc::new(Mutex::new(encoder));
            let enc_for_callback = encoder.clone();

            let config = cpal::StreamConfig {
                channels: source_channels as u16,
                sample_rate: cpal::SampleRate(source_sample_rate),
                buffer_size: cpal::BufferSize::Default,
            };

            let stream = device
                .build_input_stream(
                    &config,
                    move |data: &[f32], _: &cpal::InputCallbackInfo| {
                        let mut acc = acc_for_callback.lock().unwrap();
                        acc.push_samples(data);

                        // Drain full frames
                        while let Some(frame_samples) = acc.take_frame() {
                            let mut enc = enc_for_callback.lock().unwrap();
                            let mut opus_buf = vec![0u8; 4000];
                            match enc.encode_float(&frame_samples, &mut opus_buf) {
                                Ok(len) => {
                                    let opus_data = Bytes::copy_from_slice(&opus_buf[..len]);
                                    // Non-blocking send — drop frame if channel is full
                                    let _ = tx_for_thread.try_send(CaptureMessage::Audio {
                                        opus_data,
                                    });
                                }
                                Err(e) => {
                                    warn!(error = %e, "Opus encode error");
                                }
                            }
                        }
                    },
                    move |err| {
                        warn!(error = %err, "audio input stream error");
                    },
                    None,
                )
                .expect("build_input_stream");

            stream.play().expect("play audio stream");

            // Keep thread alive as long as stream is active
            loop {
                std::thread::park();
            }
        })
        .context("failed to spawn audio thread")?;

    Ok(())
}

/// Accumulates audio samples, handles stereo→mono conversion and resampling.
struct AudioAccumulator {
    mono_buf: Vec<f32>,
    source_rate: u32,
    source_channels: usize,
}

impl AudioAccumulator {
    fn new(source_rate: u32, source_channels: usize) -> Self {
        Self {
            mono_buf: Vec::with_capacity(OPUS_FRAME_SAMPLES * 2),
            source_rate,
            source_channels,
        }
    }

    /// Push interleaved f32 samples, converting to mono.
    fn push_samples(&mut self, data: &[f32]) {
        let channels = self.source_channels;
        for chunk in data.chunks(channels) {
            let mono = chunk.iter().sum::<f32>() / channels as f32;
            self.mono_buf.push(mono);
        }
    }

    /// If we have enough mono samples for one Opus frame (possibly after resampling),
    /// take them and return 960 samples at 48kHz.
    fn take_frame(&mut self) -> Option<Vec<f32>> {
        // How many source samples we need for one 20ms frame at 48kHz
        let source_samples_needed = if self.source_rate == OPUS_SAMPLE_RATE {
            OPUS_FRAME_SAMPLES
        } else {
            // Proportional: source_rate / 48000 * 960
            ((self.source_rate as f64 / OPUS_SAMPLE_RATE as f64) * OPUS_FRAME_SAMPLES as f64)
                .ceil() as usize
        };

        if self.mono_buf.len() < source_samples_needed {
            return None;
        }

        let source_chunk: Vec<f32> = self.mono_buf.drain(..source_samples_needed).collect();

        if self.source_rate == OPUS_SAMPLE_RATE {
            Some(source_chunk)
        } else {
            // Simple linear resampling
            Some(resample_linear(
                &source_chunk,
                self.source_rate,
                OPUS_SAMPLE_RATE,
                OPUS_FRAME_SAMPLES,
            ))
        }
    }
}

/// Simple linear interpolation resampler.
fn resample_linear(input: &[f32], from_rate: u32, to_rate: u32, output_len: usize) -> Vec<f32> {
    let ratio = from_rate as f64 / to_rate as f64;
    let mut output = Vec::with_capacity(output_len);
    for i in 0..output_len {
        let src_pos = i as f64 * ratio;
        let idx = src_pos as usize;
        let frac = src_pos - idx as f64;
        let s0 = input.get(idx).copied().unwrap_or(0.0);
        let s1 = input.get(idx + 1).copied().unwrap_or(s0);
        output.push(s0 + (s1 - s0) * frac as f32);
    }
    output
}
