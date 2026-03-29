//! Real audio capture using cpal + opus.
//!
//! Captures audio from an ALSA input device, resamples/mixes to 48kHz mono,
//! encodes with Opus (VoIP mode), and sends as CaptureMessage::AudioFrame.
//!
//! Only compiled on Linux (cpal and opus are linux-only dependencies).

use anyhow::{Context, Result};
use bytes::Bytes;
use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use tokio::sync::mpsc as tokio_mpsc;
use tokio_util::sync::CancellationToken;

use super::{CaptureMessage, CaptureSender};

/// Start real audio capture on a dedicated std::thread.
///
/// Returns `Ok(())` immediately after spawning the thread. If device
/// initialization fails, returns `Err` so the caller can fall back gracefully.
pub fn start_real_audio(
    device_name: Option<&str>,
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()> {
    let host = cpal::default_host();

    let device = if let Some(name) = device_name {
        host.input_devices()
            .context("failed to enumerate audio input devices")?
            .find(|d| d.name().map(|n| n == name).unwrap_or(false))
            .with_context(|| format!("audio device '{}' not found", name))?
    } else {
        host.default_input_device()
            .context("no default audio input device")?
    };

    let device_display_name = device.name().unwrap_or_else(|_| "<unknown>".into());
    let supported_config = device
        .default_input_config()
        .context("failed to get default input config")?;

    tracing::info!(
        device = %device_display_name,
        sample_rate = supported_config.sample_rate().0,
        channels = supported_config.channels(),
        format = ?supported_config.sample_format(),
        "audio device selected"
    );

    std::thread::Builder::new()
        .name("audio-capture".into())
        .spawn(move || {
            run_audio_thread(device, supported_config, tx, cancel);
        })
        .context("failed to spawn audio capture thread")?;

    Ok(())
}

/// Audio capture + encoding loop running on a std::thread.
fn run_audio_thread(
    device: cpal::Device,
    supported_config: cpal::SupportedStreamConfig,
    tx: CaptureSender,
    cancel: CancellationToken,
) {
    let source_rate = supported_config.sample_rate().0;
    let channels = supported_config.channels();

    // Channel from cpal callback -> processing loop (std::sync::mpsc because
    // the cpal callback is a non-async closure on an OS audio thread).
    let (raw_tx, raw_rx) = std::sync::mpsc::channel::<Vec<f32>>();

    let stream = match supported_config.sample_format() {
        cpal::SampleFormat::F32 => {
            let sender = raw_tx.clone();
            device.build_input_stream(
                &supported_config.into(),
                move |data: &[f32], _: &cpal::InputCallbackInfo| {
                    let _ = sender.send(data.to_vec());
                },
                |err| tracing::error!("audio stream error: {}", err),
                None,
            )
        }
        cpal::SampleFormat::I16 => {
            let sender = raw_tx.clone();
            device.build_input_stream(
                &supported_config.into(),
                move |data: &[i16], _: &cpal::InputCallbackInfo| {
                    let samples: Vec<f32> = data.iter().map(|&s| s as f32 / 32768.0).collect();
                    let _ = sender.send(samples);
                },
                |err| tracing::error!("audio stream error: {}", err),
                None,
            )
        }
        cpal::SampleFormat::I32 => {
            let sender = raw_tx.clone();
            device.build_input_stream(
                &supported_config.into(),
                move |data: &[i32], _: &cpal::InputCallbackInfo| {
                    let samples: Vec<f32> = data.iter().map(|&s| s as f32 / 2147483648.0).collect();
                    let _ = sender.send(samples);
                },
                |err| tracing::error!("audio stream error: {}", err),
                None,
            )
        }
        fmt => {
            tracing::warn!(?fmt, "unsupported audio sample format");
            return;
        }
    };

    // Drop the extra sender so the channel closes when the stream stops.
    drop(raw_tx);

    let stream = match stream {
        Ok(s) => s,
        Err(e) => {
            tracing::error!("failed to build audio input stream: {e}");
            return;
        }
    };

    if let Err(e) = stream.play() {
        tracing::error!("failed to start audio stream: {e}");
        return;
    }

    // Set up Opus encoder: 48kHz mono, VoIP mode for speech/surveillance.
    let mut encoder = match opus::Encoder::new(48000, opus::Channels::Mono, opus::Application::Voip)
    {
        Ok(enc) => enc,
        Err(e) => {
            tracing::error!("failed to create opus encoder: {e}");
            return;
        }
    };

    // Set a reasonable bitrate for surveillance audio.
    let _ = encoder.set_bitrate(opus::Bitrate::Bits(32000));

    tracing::info!(
        source_rate,
        channels,
        "audio capture started (-> 48kHz mono Opus)"
    );

    const FRAME_SAMPLES: usize = 960; // 20ms at 48kHz
    let mut accumulator: Vec<f32> = Vec::with_capacity(FRAME_SAMPLES * 2);
    let mut opus_buf = vec![0u8; 4000]; // max Opus frame size
    let mut frame_count: u64 = 0;

    loop {
        if cancel.is_cancelled() {
            tracing::info!("audio capture cancelled");
            break;
        }

        // Use recv_timeout so we periodically check cancellation.
        let samples = match raw_rx.recv_timeout(std::time::Duration::from_millis(100)) {
            Ok(s) => s,
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => continue,
            Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                tracing::info!("audio stream ended (device disconnected)");
                break;
            }
        };

        // Stereo -> mono
        let mono = stereo_to_mono(&samples, channels);
        // Resample to 48kHz if needed
        let resampled = resample(&mono, source_rate, 48000);

        accumulator.extend_from_slice(&resampled);

        // Emit complete 20ms frames
        while accumulator.len() >= FRAME_SAMPLES {
            let frame: Vec<f32> = accumulator.drain(..FRAME_SAMPLES).collect();

            match encoder.encode_float(&frame, &mut opus_buf) {
                Ok(encoded_len) => {
                    let msg = CaptureMessage::AudioFrame(Bytes::copy_from_slice(
                        &opus_buf[..encoded_len],
                    ));

                    if tx.blocking_send(msg).is_err() {
                        tracing::info!("audio receiver dropped, stopping capture");
                        return;
                    }

                    frame_count += 1;
                    if frame_count % 2500 == 0 {
                        // Log every ~50 seconds
                        tracing::debug!(
                            frames = frame_count,
                            seconds = frame_count / 50,
                            "audio capture running"
                        );
                    }
                }
                Err(e) => {
                    tracing::warn!("opus encode error (skipping frame): {e}");
                }
            }
        }
    }

    // Keep the stream alive until we exit this scope (dropping it stops capture).
    drop(stream);
    tracing::info!(frames = frame_count, "audio capture finished");
}

/// Convert interleaved multi-channel samples to mono by averaging channels.
fn stereo_to_mono(samples: &[f32], channels: u16) -> Vec<f32> {
    if channels == 1 {
        return samples.to_vec();
    }

    samples
        .chunks(channels as usize)
        .map(|chunk| chunk.iter().sum::<f32>() / channels as f32)
        .collect()
}

/// Linear interpolation resampler.
fn resample(samples: &[f32], source_rate: u32, target_rate: u32) -> Vec<f32> {
    if source_rate == target_rate || samples.is_empty() {
        return samples.to_vec();
    }

    let ratio = source_rate as f64 / target_rate as f64;
    let output_len = ((samples.len() as f64) / ratio).ceil() as usize;
    let mut output = Vec::with_capacity(output_len);

    for i in 0..output_len {
        let src_idx = i as f64 * ratio;
        let idx0 = src_idx.floor() as usize;
        let idx1 = (idx0 + 1).min(samples.len().saturating_sub(1));
        let frac = (src_idx - idx0 as f64) as f32;

        if idx0 < samples.len() {
            let sample = samples[idx0] * (1.0 - frac) + samples[idx1] * frac;
            output.push(sample);
        }
    }

    output
}
