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
}

// Ptr helpers for building TelemetryDatagram literals.
func PtrInt8(v int8) *int8       { return &v }
func PtrUint8(v uint8) *uint8    { return &v }
func PtrUint32(v uint32) *uint32 { return &v }
func PtrFloat32(v float32) *float32 { return &v }
func PtrFloat64(v float64) *float64 { return &v }
