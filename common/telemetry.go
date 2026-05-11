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
}

// Ptr helpers for building TelemetryDatagram literals.
func PtrStr(v string) *string         { return &v }
func PtrInt8(v int8) *int8            { return &v }
func PtrUint8(v uint8) *uint8         { return &v }
func PtrUint32(v uint32) *uint32      { return &v }
func PtrFloat32(v float32) *float32   { return &v }
func PtrFloat64(v float64) *float64   { return &v }
