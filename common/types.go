// Package common defines shared request/response types for the camera-server HTTP API.
//
// Both consumers of this package (the Go server and the Python camera) get
// their types via codegen from these struct definitions:
//   - tygo emits TypeScript to ui/src/lib/api-types/ (driven by the
//     directive in server/apitypes/apitypes.go).
//   - tools/pydanticgen emits pydantic v2 models to
//     camera/ghostcam/wire/ (driven by the directive below).
//
// Both run together as part of `go generate ./...`. CI fails if either
// generated tree is stale.
//
//go:generate bash -c "cd $(git rev-parse --show-toplevel) && go run ./tools/pydanticgen ./common ./camera/ghostcam/wire"
package common

// ProvisionRequest is sent by the camera during provisioning. The camera
// sends its ed25519 public key for registration (like adding to SSH
// authorized_keys). The server derives device_id from the public key.
type ProvisionRequest struct {
	Token        string `json:"token"`
	DeviceSerial string `json:"device_serial"`
	PublicKey    string `json:"public_key"` // hex-encoded ed25519 public key (64 chars)
	FwVersion    string `json:"fw_version,omitempty"`
}

// ProvisionResponse acknowledges key registration. No secret is returned.
type ProvisionResponse struct {
	DeviceID string `json:"device_id"` // echoed back for confirmation
	Status   string `json:"status"`    // "registered"
}

// TelemetryPollRequest is sent by the camera every 10s with current sensor readings.
type TelemetryPollRequest struct {
	Telemetry TelemetryDatagram `json:"telemetry"`
	FwVersion string            `json:"fw_version,omitempty"`
}

// TelemetryPollResponse contains any pending commands for the camera.
//
// WakeLive is set when a viewer is actively trying to watch a camera that
// is in standby mode (live WS not currently connected). The camera reads
// this flag and proactively opens its live WebSocket so WebRTC startup
// stays bounded by one telemetry-poll interval.
type TelemetryPollResponse struct {
	Commands []CameraCommand `json:"commands,omitempty"`
	WakeLive bool            `json:"wake_live,omitempty"`
}

// CameraCommand is a tagged union of commands the server can send to a camera.
// The Type field determines which other fields are populated.
//
// Power-mode commands:
//   - set_power_mode      → PowerMode field      (live | standby | sleep)
//   - set_upload_mode     → UploadMode field     (proactive | lazy)
//   - set_schedule        → Schedule field       (JSON-encoded windows)
//   - set_battery_rules   → BatteryRules field   (JSON-encoded thresholds)
//   - upload_segments     → SegmentIDs field     (lazy-mode on-demand fetch)
type CameraCommand struct {
	Type       string `json:"type"`
	Mode       string `json:"mode,omitempty"`       // set_recording_mode
	Resolution string `json:"resolution,omitempty"` // set_resolution
	SSID       string `json:"ssid,omitempty"`       // network_config, remove_network
	PSK        string `json:"psk,omitempty"`        // network_config

	// Power-mode commands.
	PowerMode    string   `json:"power_mode,omitempty"`    // set_power_mode: "live" | "standby" | "sleep"
	UploadMode   string   `json:"upload_mode,omitempty"`   // set_upload_mode: "proactive" | "lazy"
	Schedule     string   `json:"schedule,omitempty"`      // set_schedule: JSON list of {window, power_mode, upload_mode}
	BatteryRules string   `json:"battery_rules,omitempty"` // set_battery_rules: JSON list of {threshold_pct, power_mode, upload_mode}
	SegmentIDs   []string `json:"segment_ids,omitempty"`   // upload_segments: priority-fetch these segments now
}

// PresignRequest requests presigned PUT URLs and confirms previously uploaded segments.
type PresignRequest struct {
	Count    uint32           `json:"count"`
	Uploaded []UploadedSegment `json:"uploaded,omitempty"`
}

// UploadedSegment confirms a successfully uploaded segment.
type UploadedSegment struct {
	SegmentID string `json:"segment_id"`
	StartTS   uint64 `json:"start_ts"`
	EndTS     uint64 `json:"end_ts"`
	SizeBytes uint64 `json:"size_bytes"`
	HasMotion bool   `json:"has_motion,omitempty"`
}

// PresignedUrl is a presigned URL for uploading a segment to S3/Tigris.
type PresignedUrl struct {
	SegmentID string `json:"segment_id"`
	S3Key     string `json:"s3_key"`
	PutURL    string `json:"put_url"`
	ExpiresAt uint64 `json:"expires_at"`
}

// PresignResponse contains presigned URLs for segment uploads.
type PresignResponse struct {
	URLs          []PresignedUrl `json:"urls"`
	InitURL       *PresignedUrl  `json:"init_url,omitempty"`
	StorageCapped bool           `json:"storage_capped,omitempty"`
}

// QRPayload is the JSON shape encoded inside a provisioning QR code. The
// viewer UI builds it (via the server's EnrollmentQR handler), displays
// it as a QR image, and the camera parses it on first boot after scan.
// Field names are single letters to keep the QR code compact.
type QRPayload struct {
	Server       string `json:"s"`           // server base URL
	Token        string `json:"t"`           // one-time provision token
	WifiSSID     string `json:"w,omitempty"` // optional Wi-Fi SSID to join
	WifiPassword string `json:"p,omitempty"` // optional Wi-Fi password
}
