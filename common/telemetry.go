package common

// TelemetryDatagram contains sensor readings from the camera.
// Pointer fields implement omitempty — only non-nil fields are serialized.
type TelemetryDatagram struct {
	// Unix milliseconds, camera clock.
	TS uint64 `json:"ts"`
	// WiFi signal strength (dBm).
	Sig *int8 `json:"sig,omitempty"`
	// SoC temperature (°C).
	Temp *uint32 `json:"temp,omitempty"`
	// Capture frame rate.
	FPS *float32 `json:"fps,omitempty"`
	// Video bitrate (kbps).
	Kbps *uint32 `json:"kbps,omitempty"`
	// CPU usage (%).
	CPU *uint32 `json:"cpu,omitempty"`
	// Memory usage (MB).
	Mem *uint32 `json:"mem,omitempty"`
	// Uptime (seconds).
	Uptime *uint32 `json:"uptime,omitempty"`
	// GPS latitude.
	Lat *float64 `json:"lat,omitempty"`
	// GPS longitude.
	Lon *float64 `json:"lon,omitempty"`
	// GPS altitude (metres).
	Alt *float32 `json:"alt,omitempty"`
	// GPS fix quality: 0=none, 1=2D, 2=3D.
	GPSFix *uint8 `json:"gps_fix,omitempty"`
	// Currently effective power mode after schedule + battery rule
	// resolution: "live" | "standby" | "sleep". Lets the server (and UI)
	// see what the camera is actually doing right now, which can differ
	// from the manually-set mode when a schedule or battery rule is
	// overriding it.
	PowerMode *string `json:"power_mode,omitempty"`
	// Currently effective upload mode: "proactive" | "lazy". Same
	// reasoning as PowerMode.
	UploadMode *string `json:"upload_mode,omitempty"`
	// Battery state-of-charge (0-100). Only set when a battery-sensing
	// HAT (PiSugar / generic UPS) is wired up via
	// platform/battery.py; absent on grid-powered cameras.
	BatteryPct *uint8 `json:"battery_pct,omitempty"`
	// Motion-gated upload counters since boot. Lets the field measure
	// bandwidth savings on cameras running recording_mode='motion':
	// uploaded counts segments that went to S3, skipped counts segments
	// that were posted to local-manifest instead. Absent on cameras
	// running recording_mode='constant' (always zero, no signal).
	MotionSegmentsUploaded *uint32 `json:"motion_segments_uploaded,omitempty"`
	MotionSegmentsSkipped  *uint32 `json:"motion_segments_skipped,omitempty"`
	// Performance / health metrics. All optional; absent fields stay
	// out of the JSON envelope so a missing capability (no gpsd, no
	// modem) doesn't surface as zero.
	//
	// SegmentUploadP95Ms: 95th percentile of the time from segment file
	// close to S3 PUT 200, sampled over the most recent ~50 uploads.
	// Leading indicator of network or presign-path slowdown.
	SegmentUploadP95Ms *uint32 `json:"segment_upload_p95_ms,omitempty"`
	// SegmentUploadRetries: cumulative retry count since boot. Differs
	// from a "failed uploads" counter: a single eventually-successful
	// upload that hits 2 retries adds 2 here.
	SegmentUploadRetries *uint32 `json:"segment_upload_retries,omitempty"`
	// SegmentQueueDepth: instantaneous depth of the asyncio segment
	// queue feeding the upload loop. Saturation means the camera is
	// producing segments faster than it can upload them — leading
	// indicator before storage_capped fires.
	SegmentQueueDepth *uint8 `json:"segment_queue_depth,omitempty"`
	// LiveWSBytesPerSec: bytes pushed over the live WebSocket since the
	// last telemetry tick, divided by the tick interval. Tracks WebRTC
	// viewer egress independently of S3 upload bandwidth.
	LiveWSBytesPerSec *uint32 `json:"live_ws_bytes_per_sec,omitempty"`
	// LiveWSDroppedFrames: cumulative count of frames dropped by the
	// LiveRelay ring buffer (drop-oldest under back-pressure). Any
	// non-zero number means the consumer (server) is slower than the
	// camera's encode rate.
	LiveWSDroppedFrames *uint32 `json:"live_ws_dropped_frames,omitempty"`
	// GpsdQueryMs: wall-time of the most recent gpsd query in
	// milliseconds. Catches gpsd hiccups (slow socket, parse errors)
	// that don't surface as fix-quality changes.
	GpsdQueryMs *uint16 `json:"gpsd_query_ms,omitempty"`
	// EventLoopLagMs: scheduling latency for asyncio.sleep(0) measured
	// at the schedule-ticker task. A sustained non-zero here = some
	// task is blocking the loop synchronously.
	EventLoopLagMs *uint16 `json:"event_loop_lag_ms,omitempty"`
	// DiskUsedPct: percent disk used at segment_dir's filesystem.
	// Complements local_storage_cap_bytes for visibility into
	// whether the cap is doing its job.
	DiskUsedPct *uint8 `json:"disk_used_pct,omitempty"`
	// ModemRAT: cellular Radio Access Technology in use (e.g. "LTE",
	// "5G_NSA", "WCDMA"). Pairs with the existing Sig dBm so the UI
	// can show "−95 dBm LTE" vs "−95 dBm 3G". Absent when wired.
	ModemRAT *string `json:"modem_rat,omitempty"`
	// NetworkRecoveryAttempts: cumulative count of times the daemon
	// detected an extended telemetry-POST silence (consecutive failures
	// past a threshold) and forced a network re-association via nmcli
	// or mmcli. Non-zero means the camera *did* lose its uplink at
	// some point and recovered itself; pair with the older
	// `presign_fail_count` style counters to distinguish "flaky network
	// but daemon kept up" from "uplink went black-hole and got
	// reset". See GH #82.
	NetworkRecoveryAttempts *uint32 `json:"network_recovery_attempts,omitempty"`
	// Actually-in-use rpicam-vid parameters (#113). Reflect what's
	// running RIGHT NOW: ABR-selected tier override > env-var profile >
	// stored UI preference. The UI dashboard renders these instead of
	// the stored Quality preference so an operator sees what their
	// camera is actually doing — not what they last clicked. nil when
	// no capture pipeline is active (e.g. sleep mode, between restarts).
	CurrentWidth       *uint32 `json:"current_width,omitempty"`
	CurrentHeight      *uint32 `json:"current_height,omitempty"`
	CurrentBitrateKbps *uint32 `json:"current_bitrate_kbps,omitempty"`
	// LocalSegmentBacklog is the count of .m4s segment files sitting in
	// the camera's segmentDir at telemetry-poll time — the disk-side
	// view of "how far behind the uploader is". The server edge-detects
	// crossings of an upload-stall threshold and emits an
	// `upload_stalled` SSE event (#115 bug 2). Distinct from the older
	// SegmentQueueDepth (uint8 in-memory metric, Python-era, never
	// populated by Go); the disk backlog routinely exceeds 255 during a
	// stall — the 2026-05-16 #107 soak peaked at 759.
	LocalSegmentBacklog *uint32 `json:"local_segment_backlog,omitempty"`
	// AudioRMSDBFS is the most recent mean-volume sample of the camera's
	// audio track, in dBFS (decibels relative to full scale, ≤ 0). The
	// camera measures this every ~5 min by running ffmpeg's volumedetect
	// filter against the second-most-recent finished segment. Typical
	// values: speech in a quiet room ≈ -25 to -35 dBFS; quiet ambient
	// ≈ -55 dBFS; broken/disconnected mic ≈ -90 dBFS. Server-side edge
	// detection in audio_silence.go fires an `audio_silent` SSE event
	// when this drops below the threshold (GH #114-adjacent). nil when
	// audio is disabled or no segment has been measured yet.
	AudioRMSDBFS *float32 `json:"audio_rms_dbfs,omitempty"`
}

// Ptr helpers for building TelemetryDatagram literals.
func PtrStr(v string) *string         { return &v }
func PtrInt8(v int8) *int8            { return &v }
func PtrUint8(v uint8) *uint8         { return &v }
func PtrUint32(v uint32) *uint32      { return &v }
func PtrFloat32(v float32) *float32   { return &v }
func PtrFloat64(v float64) *float64   { return &v }
