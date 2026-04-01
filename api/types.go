// Package api defines shared request/response types for the camera-server HTTP API.
package api

// ProvisionRequest is sent by the camera after scanning a provisioning QR code.
type ProvisionRequest struct {
	Token        string `json:"token"`
	DeviceSerial string `json:"device_serial"`
	FwVersion    string `json:"fw_version,omitempty"`
}

// ProvisionResponse is returned by the server after successful provisioning.
type ProvisionResponse struct {
	APIKey   string `json:"api_key"`
	DeviceID string `json:"device_id"`
}

// TelemetryPollRequest is sent by the camera every 10s with current sensor readings.
type TelemetryPollRequest struct {
	Telemetry TelemetryDatagram `json:"telemetry"`
	FwVersion string            `json:"fw_version,omitempty"`
}

// TelemetryPollResponse contains any pending commands for the camera.
type TelemetryPollResponse struct {
	Commands []CameraCommand `json:"commands,omitempty"`
}

// CameraCommand is a tagged union of commands the server can send to a camera.
// The Type field determines which other fields are populated.
type CameraCommand struct {
	Type       string `json:"type"`
	Mode       string `json:"mode,omitempty"`       // set_recording_mode
	Resolution string `json:"resolution,omitempty"` // set_resolution
	SSID       string `json:"ssid,omitempty"`       // network_config, remove_network
	PSK        string `json:"psk,omitempty"`        // network_config
}

// RecordingMode represents the camera's recording mode.
type RecordingMode string

const (
	RecordingModeConstant RecordingMode = "constant"
	RecordingModeMotion   RecordingMode = "motion"
)

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
	URLs    []PresignedUrl `json:"urls"`
	InitURL *PresignedUrl  `json:"init_url,omitempty"`
}

// QrPayload is the JSON payload encoded in provisioning QR codes.
type QrPayload struct {
	Server   string `json:"s"`
	Token    string `json:"t"`
	WifiSSID string `json:"w,omitempty"`
	WifiPSK  string `json:"p,omitempty"`
}
