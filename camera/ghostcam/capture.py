"""Capture pipeline: rpicam-vid + ffmpeg orchestration.

Mirrors camera/capture.go. Three modes, mirroring the Go camera:

  * test_source with pre-encoded loop file (`{data_dir}/test-loop.mp4`)
  * test_source with ffmpeg testsrc2 + sine audio
  * real rpicam-vid + ffmpeg

In all modes the capture process produces:

  * MPEG-TS segments in segment_dir (when recording_mode != "never")
  * Raw H.264 to live_writer.write() — drives the WebSocket relay
  * OGG/Opus on a side-channel pipe → live_writer.push_audio()

The OGG/Opus side-channel is the Spike 2 result: instead of pinning to
fd 3 (which Python silently breaks during interpreter startup), we tell
ffmpeg `pipe:{wfd}` for whatever fd `os.pipe()` returned and wire it in
via `pass_fds`. ffmpeg's pipe protocol accepts arbitrary fd numbers.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
from pathlib import Path

from ghostcam.config import CameraConfig
from ghostcam.live_relay import LiveWriter
from ghostcam.ogg_reader import read_ogg_opus_packets

logger = logging.getLogger(__name__)

SEGMENT_DURATION_SECS = 6


def _records_segments(cfg: CameraConfig) -> bool:
    return cfg.recording_mode != "never"


def _next_segment_number(directory: Path) -> int:
    try:
        return sum(1 for e in directory.iterdir() if e.suffix == ".ts")
    except OSError:
        return 0


def _segment_pattern(segment_dir: Path) -> str:
    return str(segment_dir / "seg%05d.ts")


async def start_capture_pipeline(
    cfg: CameraConfig,
    live_writer: LiveWriter,
) -> None:
    """Spawn the capture pipeline. Returns when the pipeline exits or
    the surrounding task is cancelled. Raises on subprocess failure.
    """
    cfg.segment_dir.mkdir(parents=True, exist_ok=True)
    start_num = _next_segment_number(cfg.segment_dir)
    pattern = _segment_pattern(cfg.segment_dir)

    if cfg.test_source:
        loop_file = cfg.data_dir / "test-loop.mp4"
        if loop_file.is_file():
            await _run_test_file_loop(cfg, loop_file, pattern, start_num, live_writer)
        else:
            await _run_test_pipeline(cfg, pattern, start_num, live_writer)
        return
    await _run_real_pipeline(cfg, pattern, start_num, live_writer)


# --- helpers ---


async def _spawn_ffmpeg_with_audio_pipe(
    args: list[str],
    has_audio: bool,
) -> tuple[asyncio.subprocess.Process, asyncio.StreamReader | None]:
    """Spawn ffmpeg and, when has_audio, set up the side-channel pipe.

    Returns (proc, audio_reader). audio_reader is None when has_audio is
    False. Caller must ensure args contain the matching `pipe:{wfd}`
    output spec (the wfd is allocated here and we substitute the literal
    placeholder `{audio_fd}` in args).
    """
    audio_pipe_r: asyncio.StreamReader | None = None
    pass_fds: tuple[int, ...] = ()

    if has_audio:
        rfd, wfd = os.pipe()
        os.set_inheritable(wfd, True)
        # Substitute placeholder in args.
        args = [a.replace("{audio_fd}", str(wfd)) for a in args]
        pass_fds = (wfd,)

        proc = await asyncio.create_subprocess_exec(
            *args,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            pass_fds=pass_fds,
        )
        os.close(wfd)

        loop = asyncio.get_running_loop()
        audio_pipe_r = asyncio.StreamReader()
        protocol = asyncio.StreamReaderProtocol(audio_pipe_r)
        await loop.connect_read_pipe(lambda: protocol, os.fdopen(rfd, "rb"))
        return proc, audio_pipe_r

    proc = await asyncio.create_subprocess_exec(
        *args,
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    return proc, None


async def _drain_stderr(proc: asyncio.subprocess.Process, label: str) -> None:
    if proc.stderr is None:
        return
    while True:
        line = await proc.stderr.readline()
        if not line:
            return
        logger.debug("%s: %s", label, line.decode(errors="replace").rstrip())


async def _copy_to_live(
    src: asyncio.StreamReader,
    live_writer: LiveWriter,
    *,
    chunk: int = 65536,
) -> None:
    while True:
        data = await src.read(chunk)
        if not data:
            return
        live_writer.write(data)


async def _copy_to_two(
    src: asyncio.StreamReader,
    sinks: list[asyncio.StreamWriter | None],
    live_writer: LiveWriter,
    *,
    chunk: int = 65536,
) -> None:
    """Copy src bytes to each non-None StreamWriter sink AND to the live
    writer. Closes the StreamWriter sinks on EOF.
    """
    try:
        while True:
            data = await src.read(chunk)
            if not data:
                return
            live_writer.write(data)
            for w in sinks:
                if w is None:
                    continue
                w.write(data)
                try:
                    await w.drain()
                except (BrokenPipeError, ConnectionResetError):
                    return
    finally:
        for w in sinks:
            if w is None:
                continue
            with contextlib.suppress(Exception):
                w.close()


async def _audio_reader_task(
    audio_pipe: asyncio.StreamReader,
    live_writer: LiveWriter,
) -> None:
    try:
        await read_ogg_opus_packets(audio_pipe, live_writer.push_audio)
    except Exception as e:  # noqa: BLE001
        logger.debug("opus reader finished: %s", e)


# --- test-source pipelines ---


async def _run_test_pipeline(
    cfg: CameraConfig,
    pattern: str,
    start_num: int,
    live_writer: LiveWriter,
) -> None:
    logger.info("starting test capture pipeline (testsrc2 + sine)")

    size = f"{cfg.video_width}x{cfg.video_height}"
    video_input = (
        f"testsrc2=size={size}:rate={cfg.video_fps},"
        "drawtext=fontfile=/usr/share/fonts/dejavu/DejaVuSansMono.ttf:"
        "text='%{localtime\\:%T}':fontsize=48:fontcolor=white:x=10:y=10"
    )
    audio_input = "sine=frequency=440:sample_rate=48000"
    kf = f"keyint={cfg.video_keyframe_interval}:min-keyint={cfg.video_keyframe_interval}"

    args = [
        "ffmpeg",
        "-re",
        "-f", "lavfi", "-i", video_input,
        "-f", "lavfi", "-i", audio_input,
    ]
    if _records_segments(cfg):
        args += [
            "-map", "0:v", "-map", "1:a",
            "-c:v", "libx264", "-preset", "ultrafast",
            "-x264-params", kf,
            "-c:a", "aac", "-b:a", "64k",
            "-f", "segment",
            "-segment_time", str(SEGMENT_DURATION_SECS),
            "-segment_format", "mpegts",
            "-segment_start_number", str(start_num),
            "-reset_timestamps", "1",
            pattern,
        ]
    args += [
        "-map", "0:v",
        "-c:v", "libx264", "-preset", "ultrafast",
        "-x264-params", kf,
        "-f", "h264", "pipe:1",
        "-map", "1:a",
        "-c:a", "libopus", "-b:a", "32k",
        "-application", "lowdelay",
        "-frame_duration", "20",
        "-f", "ogg", "pipe:{audio_fd}",
    ]

    proc, audio_pipe = await _spawn_ffmpeg_with_audio_pipe(args, has_audio=True)
    logger.info("ffmpeg test pipeline started (pid=%d)", proc.pid)

    tasks = [
        asyncio.create_task(_copy_to_live(proc.stdout, live_writer), name="ffmpeg-stdout"),  # type: ignore[arg-type]
        asyncio.create_task(_drain_stderr(proc, "ffmpeg"), name="ffmpeg-stderr"),
    ]
    if audio_pipe is not None:
        tasks.append(asyncio.create_task(
            _audio_reader_task(audio_pipe, live_writer), name="opus-reader"
        ))

    try:
        rc = await proc.wait()
    finally:
        for t in tasks:
            t.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)

    if rc != 0:
        raise RuntimeError(f"ffmpeg exited with code {rc}")


async def _run_test_file_loop(
    cfg: CameraConfig,
    test_file: Path,
    pattern: str,
    start_num: int,
    live_writer: LiveWriter,
) -> None:
    logger.info("starting test file loop (-c copy): %s", test_file)

    args = [
        "ffmpeg",
        "-re",
        "-stream_loop", "-1",
        "-i", str(test_file),
    ]
    if _records_segments(cfg):
        args += [
            "-map", "0",
            "-c", "copy",
            "-f", "segment",
            "-segment_time", str(SEGMENT_DURATION_SECS),
            "-segment_format", "mpegts",
            "-segment_start_number", str(start_num),
            pattern,
        ]
    args += [
        "-map", "0:v", "-c:v", "copy",
        "-f", "h264", "pipe:1",
        "-map", "0:a",
        "-c:a", "libopus", "-b:a", "32k",
        "-application", "lowdelay",
        "-frame_duration", "20",
        "-f", "ogg", "pipe:{audio_fd}",
    ]

    proc, audio_pipe = await _spawn_ffmpeg_with_audio_pipe(args, has_audio=True)
    logger.info("ffmpeg test file loop started (pid=%d)", proc.pid)

    tasks = [
        asyncio.create_task(_copy_to_live(proc.stdout, live_writer)),  # type: ignore[arg-type]
        asyncio.create_task(_drain_stderr(proc, "ffmpeg")),
    ]
    if audio_pipe is not None:
        tasks.append(asyncio.create_task(_audio_reader_task(audio_pipe, live_writer)))

    try:
        rc = await proc.wait()
    finally:
        for t in tasks:
            t.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)

    if rc != 0:
        raise RuntimeError(f"ffmpeg exited with code {rc}")


# --- real rpicam-vid + ffmpeg ---


async def _run_real_pipeline(
    cfg: CameraConfig,
    pattern: str,
    start_num: int,
    live_writer: LiveWriter,
) -> None:
    has_audio = not cfg.no_audio
    record = _records_segments(cfg)
    logger.info(
        "starting real capture (record=%s, has_audio=%s, segment_start=%d)",
        record, has_audio, start_num,
    )

    rpicam = await asyncio.create_subprocess_exec(
        "rpicam-vid",
        "--codec", "h264",
        "--inline",
        "--width", str(cfg.video_width),
        "--height", str(cfg.video_height),
        "--framerate", str(cfg.video_fps),
        "--bitrate", str(cfg.video_bitrate),
        "-t", "0",
        "-o", "-",
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        preexec_fn=os.setsid,
    )
    logger.info("rpicam-vid started (pid=%d)", rpicam.pid)

    rpicam_tasks = [asyncio.create_task(_drain_stderr(rpicam, "rpicam-vid"))]

    # Streaming-only with no audio: pipe rpicam straight to live writer.
    if not record and not has_audio:
        try:
            await _copy_to_live(rpicam.stdout, live_writer)  # type: ignore[arg-type]
            rc = await rpicam.wait()
        finally:
            for t in rpicam_tasks:
                t.cancel()
            await asyncio.gather(*rpicam_tasks, return_exceptions=True)
        if rc != 0:
            raise RuntimeError(f"rpicam-vid exited with code {rc}")
        return

    feed_video_to_ffmpeg = record
    alsa_idx = 1 if feed_video_to_ffmpeg else 0

    args = [
        "ffmpeg",
        "-nostdin", "-loglevel", "warning",
        "-probesize", "5M", "-analyzeduration", "5M",
    ]
    if feed_video_to_ffmpeg:
        args += ["-f", "h264", "-framerate", str(cfg.video_fps), "-i", "pipe:0"]
    if has_audio:
        device = cfg.audio_device or "default"
        args += ["-f", "alsa", "-i", device]

    if record:
        if has_audio:
            args += ["-map", "0:v", "-map", "1:a",
                     "-c:v", "copy", "-c:a", "aac", "-b:a", "64k"]
        else:
            args += ["-map", "0:v", "-c:v", "copy"]
        args += [
            "-f", "segment",
            "-segment_time", str(SEGMENT_DURATION_SECS),
            "-segment_format", "mpegts",
            "-segment_start_number", str(start_num),
            "-reset_timestamps", "1",
            pattern,
        ]

    if has_audio:
        args += [
            "-map", f"{alsa_idx}:a",
            "-c:a", "libopus", "-b:a", "32k",
            "-application", "lowdelay",
            "-frame_duration", "20",
            "-f", "ogg", "pipe:{audio_fd}",
        ]

    ffmpeg, audio_pipe = await _spawn_ffmpeg_with_audio_pipe(args, has_audio=has_audio)
    logger.info("ffmpeg started (pid=%d)", ffmpeg.pid)

    aux_tasks: list[asyncio.Task[None]] = [
        asyncio.create_task(_drain_stderr(ffmpeg, "ffmpeg")),
    ]
    if audio_pipe is not None:
        aux_tasks.append(asyncio.create_task(_audio_reader_task(audio_pipe, live_writer)))

    # Tee rpicam stdout: ffmpeg stdin + live writer (or just live writer
    # when not recording). Streaming-only mode + audio uses the live
    # writer alone for video; ffmpeg only sees ALSA.
    sinks: list[asyncio.StreamWriter | None] = []
    if feed_video_to_ffmpeg and ffmpeg.stdin is not None:
        sinks.append(ffmpeg.stdin)
    aux_tasks.append(asyncio.create_task(
        _copy_to_two(rpicam.stdout, sinks, live_writer)  # type: ignore[arg-type]
    ))

    try:
        rpicam_rc, ffmpeg_rc = await asyncio.gather(rpicam.wait(), ffmpeg.wait())
    finally:
        for t in [*rpicam_tasks, *aux_tasks]:
            t.cancel()
        await asyncio.gather(*rpicam_tasks, *aux_tasks, return_exceptions=True)

    if rpicam_rc != 0:
        raise RuntimeError(f"rpicam-vid exited with code {rpicam_rc}")
    if ffmpeg_rc != 0:
        raise RuntimeError(f"ffmpeg exited with code {ffmpeg_rc}")
