// Package apitypes defines every request, response, and event payload on
// the viewer-server HTTP/SSE surface. It exists so tygo can generate a
// matching TypeScript file for the UI — editing a struct here is
// automatically reflected in ui/src/lib/api-types/ on the next
// `make generate-types` run, and CI refuses PRs whose generated file is
// stale.
//
// Rules for this package:
//
//  1. Every type is exported and has JSON tags that exactly match the
//     wire format. The struct field names are internal; the tags are
//     the contract.
//  2. No behavior, no methods that do work. Types only. This keeps the
//     package importable from anywhere without cycles.
//  3. Camera-server contract types (PresignRequest, TelemetryDatagram,
//     ProvisionRequest, etc.) stay in common/. This package is the
//     viewer-server surface. The two do not overlap by design.
package apitypes

// ====================================================================
// Auth
// ====================================================================

// LoginRequest is the body of POST /api/v1/auth/login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse is the body of POST /api/v1/auth/login on success.
type LoginResponse struct {
	UserID string `json:"user_id"`
}

// ChangePasswordRequest is the body of PATCH /api/v1/auth/password.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ====================================================================
// Cameras
// ====================================================================

// CameraResponse is the per-camera payload returned by GET /api/v1/cameras
// (as a list) and GET /api/v1/cameras/{id} (as a single object).
type CameraResponse struct {
	DeviceID      string          `json:"device_id"`
	DisplayName   string          `json:"display_name"`
	EnrolledAt    uint64          `json:"enrolled_at"`
	LastSeenAt    *int64          `json:"last_seen_at,omitempty"`
	Provisioned   bool            `json:"provisioned"`
	Notes         *string         `json:"notes,omitempty"`
	Resolution    string          `json:"resolution"`
	RecordingMode string          `json:"recording_mode"`
	Telemetry     *TelemetryEntry `json:"telemetry,omitempty"`
}

// EnrollResponse is the body of POST /api/v1/cameras — a one-time
// provisioning token plus its expiry.
type EnrollResponse struct {
	Token     string `json:"token"`
	ExpiresAt uint64 `json:"expires_at"`
}

// UpdateCameraRequest is the body of PATCH /api/v1/cameras/{id}.
type UpdateCameraRequest struct {
	DisplayName   *string `json:"display_name,omitempty"`
	Notes         *string `json:"notes,omitempty"`
	Resolution    *string `json:"resolution,omitempty"`
	RecordingMode *string `json:"recording_mode,omitempty"`
}

// ====================================================================
// Telemetry
// ====================================================================

// TelemetryEntry is a single camera telemetry reading stored in the Redis
// telemetry stream and returned on GET /api/v1/telemetry/{id}/latest,
// GET /api/v1/telemetry/{id} and inside CameraResponse.
type TelemetryEntry struct {
	TS       uint64   `json:"ts"`
	ServerTS uint64   `json:"server_ts"`
	Sig      *int8    `json:"sig,omitempty"`
	Temp     *uint32  `json:"temp,omitempty"`
	FPS      *float32 `json:"fps,omitempty"`
	Kbps     *uint32  `json:"kbps,omitempty"`
	CPU      *uint32  `json:"cpu,omitempty"`
	Mem      *uint32  `json:"mem,omitempty"`
	Uptime   *uint32  `json:"uptime,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	Alt      *float32 `json:"alt,omitempty"`
	GPSFix   *uint8   `json:"gps_fix,omitempty"`
}

// TelemetryRangeResponse is the body of GET /api/v1/telemetry/{id}?from=&to=.
type TelemetryRangeResponse struct {
	Entries []TelemetryEntry `json:"entries"`
}

// ====================================================================
// Clips / Export
// ====================================================================

// PrepareClipRequest is the body of POST /api/v1/clips/prepare.
type PrepareClipRequest struct {
	DeviceID string `json:"device_id"`
	FromMs   uint64 `json:"from_ms"`
	ToMs     uint64 `json:"to_ms"`
}

// ClipSegment is one presigned segment URL inside PrepareClipResponse.
type ClipSegment struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	StartMs   uint64 `json:"start_ms"`
	EndMs     uint64 `json:"end_ms"`
	SizeBytes uint64 `json:"size_bytes"`
}

// PrepareClipResponse is the body of POST /api/v1/clips/prepare.
type PrepareClipResponse struct {
	Segments   []ClipSegment `json:"segments"`
	TotalBytes uint64        `json:"total_bytes"`
	DurationMs uint64        `json:"duration_ms"`
}

// ====================================================================
// HLS Coverage
// ====================================================================

// CoverageSegment is one entry in CoverageResponse and in the coverage
// SSE payload published when a camera confirms an upload.
type CoverageSegment struct {
	ID        string `json:"id"`
	StartMs   uint64 `json:"start_ms"`
	EndMs     uint64 `json:"end_ms"`
	HasMotion bool   `json:"has_motion"`
}

// CoverageResponse is the body of GET /hls/{id}/coverage.
type CoverageResponse struct {
	Segments []CoverageSegment `json:"segments"`
}

// ====================================================================
// Billing
// ====================================================================

// TierInfo is the per-tier entry returned by GET /api/v1/billing/tiers.
// Nil limits mean unlimited.
type TierInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CameraLimit *int   `json:"camera_limit"`
	StorageGB   *int   `json:"storage_gb"`
}

// ListTiersResponse is the body of GET /api/v1/billing/tiers.
type ListTiersResponse struct {
	Tiers []TierInfo `json:"tiers"`
}

// SubscriptionResponse is the body of GET /api/v1/billing/subscription.
type SubscriptionResponse struct {
	BillingEnabled bool   `json:"billing_enabled"`
	Tier           string `json:"tier"`
}

// UsageResponse is the body of GET /api/v1/billing/usage.
type UsageResponse struct {
	CamerasCount   int64  `json:"cameras_count"`
	StorageBytes   uint64 `json:"storage_bytes"`
	CameraLimit    *int   `json:"camera_limit"`
	StorageLimitGB *int   `json:"storage_limit_gb"`
}

// CheckoutRequest is the body of POST /api/v1/billing/checkout.
type CheckoutRequest struct {
	Tier       string `json:"tier"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

// CheckoutResponse is the body of POST /api/v1/billing/checkout on success.
type CheckoutResponse struct {
	URL string `json:"url"`
}

// PortalRequest is the body of POST /api/v1/billing/portal.
type PortalRequest struct {
	ReturnURL string `json:"return_url"`
}

// PortalResponse is the body of POST /api/v1/billing/portal on success.
type PortalResponse struct {
	URL string `json:"url"`
}

// ====================================================================
// API Tokens
// ====================================================================

// TokenInfo is the per-token entry returned by GET /api/v1/tokens.
type TokenInfo struct {
	TokenID    string `json:"token_id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
}

// CreateTokenRequest is the body of POST /api/v1/tokens.
type CreateTokenRequest struct {
	Label     string `json:"label"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

// CreateTokenResponse is the body of POST /api/v1/tokens — the raw token
// is returned exactly once.
type CreateTokenResponse struct {
	TokenID  string `json:"token_id"`
	RawToken string `json:"token"`
}

// ====================================================================
// QR / Enrollment
// ====================================================================

// QRRequest is the body of POST /api/v1/cameras/enroll/qr. Empty body is
// valid; TTLHours defaults to 24 (max 168).
type QRRequest struct {
	WifiSSID     string `json:"wifi_ssid,omitempty"`
	WifiPassword string `json:"wifi_password,omitempty"`
	TTLHours     uint64 `json:"ttl_hours,omitempty"`
}

// QRResponse is the body of GET/POST /api/v1/cameras/enroll/qr — the
// JSON payload that the UI encodes into a QR code for the camera to scan.
type QRResponse struct {
	Payload   string `json:"payload"`
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// ====================================================================
// Events (stored notifications)
// ====================================================================

// EventEntry is one stored notification. Returned by GET /api/v1/events.
type EventEntry struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	DeviceID  string `json:"device_id"`
	Data      string `json:"data"` // raw JSON string (shape depends on Type)
	CreatedAt uint64 `json:"created_at"`
	Read      bool   `json:"read"`
	Dismissed bool   `json:"dismissed"`
}

// ListEventsResponse is the body of GET /api/v1/events.
type ListEventsResponse struct {
	Events []EventEntry `json:"events"`
}

// UnreadCountResponse is the body of GET /api/v1/events/unread.
type UnreadCountResponse struct {
	Count int64 `json:"count"`
}

// EventsSyncPayload is the body published to the per-user `events_sync:<user_id>`
// Redis channel whenever a viewer reads / dismisses events. Other viewer tabs
// pick this up over SSE and update their local state.
type EventsSyncPayload struct {
	Action  string `json:"action"` // "read" | "read_all" | "dismiss"
	EventID string `json:"event_id,omitempty"`
}

// ====================================================================
// Firmware
// ====================================================================

// FirmwareRelease is the per-version metadata inside FirmwareLatestResponse.
type FirmwareRelease struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256,omitempty"`
}

// FirmwareLatestResponse is the body of GET /api/v1/firmware/latest.
// Release is nil when no firmware has been uploaded.
type FirmwareLatestResponse struct {
	Release *FirmwareRelease `json:"release"`
}

// FirmwareUploadResponse is the body of POST /api/v1/admin/firmware on success.
type FirmwareUploadResponse struct {
	Version   string `json:"version"`
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// FirmwareMeta is the shape stored at `firmware:latest:meta` in Redis as
// JSON. Not an HTTP response, but part of the admin-facing contract.
type FirmwareMeta struct {
	Version   string `json:"version"`
	S3Key     string `json:"s3_key"`
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// ====================================================================
// SSE / pub-sub payloads
// ====================================================================

// TelemetryStreamEvent is the payload of the SSE `telemetry` event, emitted
// for every camera telemetry update.
type TelemetryStreamEvent struct {
	DeviceID  string          `json:"device_id"`
	Telemetry *TelemetryEntry `json:"telemetry"`
}

// MotionEvent is the payload of the SSE `motion_detected` event and the
// corresponding `events:<user_id>` stream entry. EventID is the Redis
// stream ID; it is empty when the payload is persisted to the stream
// (the stream ID is assigned by Redis at XADD time) and non-empty when
// it is republished for SSE consumers.
type MotionEvent struct {
	EventID   string `json:"event_id,omitempty"`
	DeviceID  string `json:"device_id"`
	SegmentID string `json:"segment_id"`
	StartTS   uint64 `json:"start_ts"`
	EndTS     uint64 `json:"end_ts"`
}

// CoveragePayload is the payload of the SSE `coverage` event, published
// whenever a camera confirms a batch of uploaded segments.
type CoveragePayload struct {
	DeviceID string            `json:"device_id"`
	Segments []CoverageSegment `json:"segments"`
}

// StorageCappedEvent is the payload of the SSE `storage_capped` event,
// published when a camera's upload would exceed the user's tier limit.
type StorageCappedEvent struct {
	EventID      string `json:"event_id,omitempty"`
	UserID       string `json:"user_id"`
	DeviceID     string `json:"device_id"`
	StorageBytes uint64 `json:"storage_bytes"`
	LimitGB      int    `json:"limit_gb"`
}

// CameraLimitExceededEvent is the payload of the SSE event emitted on the
// `storage_capped:<user_id>` channel when a Stripe subscription change
// drops the user below their current camera count.
type CameraLimitExceededEvent struct {
	EventID     string `json:"event_id,omitempty"`
	UserID      string `json:"user_id"`
	CameraCount int64  `json:"camera_count"`
	CameraLimit int    `json:"camera_limit"`
	Tier        string `json:"tier"`
}
