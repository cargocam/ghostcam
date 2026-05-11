// Package apitypes defines every request, response, and event payload on
// the viewer-server HTTP/SSE surface. It exists so tygo can generate a
// matching TypeScript file for the UI — editing a struct here is
// automatically reflected in ui/src/lib/api-types/ on the next
// `go generate ./...` run, and CI refuses PRs whose generated file is
// stale.
//
//go:generate bash -c "cd $(git rev-parse --show-toplevel) && go tool tygo generate"
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

import "encoding/json"

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

// ForgotPasswordRequest is the body of POST /api/v1/auth/forgot-password.
type ForgotPasswordRequest struct {
	Email string `json:"email"`
}

// ResetPasswordRequest is the body of POST /api/v1/auth/reset-password.
type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// VerifyEmailRequest is the body of POST /api/v1/auth/verify-email.
type VerifyEmailRequest struct {
	Token string `json:"token"`
}

// ChangeEmailRequest is the body of PATCH /api/v1/auth/email.
type ChangeEmailRequest struct {
	NewEmail        string `json:"new_email"`
	CurrentPassword string `json:"current_password"`
}

// ConfirmEmailChangeRequest is the body of POST /api/v1/auth/email/confirm.
type ConfirmEmailChangeRequest struct {
	Token string `json:"token"`
}

// RequestLoginOTPRequest is the body of POST /api/v1/auth/otp/request.
type RequestLoginOTPRequest struct {
	Email string `json:"email"`
}

// VerifyLoginOTPRequest is the body of POST /api/v1/auth/otp/verify.
type VerifyLoginOTPRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
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
	FwVersion     string          `json:"fw_version,omitempty"`
	Telemetry     *TelemetryEntry `json:"telemetry,omitempty"`
	// PowerMode / UploadMode are the manually-set values from the
	// cameras row. The camera's actual effective mode (which may differ
	// when a schedule or battery rule is overriding) is reported back
	// via the telemetry datagram (TelemetryEntry.PowerMode etc).
	PowerMode    string          `json:"power_mode"`
	UploadMode   string          `json:"upload_mode"`
	Schedule     json.RawMessage `json:"schedule,omitempty"`
	BatteryRules json.RawMessage `json:"battery_rules,omitempty"`
}

// EnrollResponse is the body of POST /api/v1/cameras — a one-time
// provisioning token plus its expiry.
type EnrollResponse struct {
	Token     string `json:"token"`
	ExpiresAt uint64 `json:"expires_at"`
}

// UpdateCameraRequest is the body of PATCH /api/v1/cameras/{id}.
//
// Power-mode fields:
//
//   - PowerMode   — "live" | "standby" | "sleep"
//   - UploadMode  — "proactive" | "lazy"
//   - Schedule    — list of {start, end, days, power_mode, upload_mode}.
//                   Send an empty list to clear an existing schedule.
//   - BatteryRules — list of {threshold_pct, power_mode, upload_mode}.
//                    Send an empty list to clear.
//
// All four are optional; sending `null` leaves the existing value alone.
type UpdateCameraRequest struct {
	DisplayName   *string         `json:"display_name,omitempty"`
	Notes         *string         `json:"notes,omitempty"`
	Resolution    *string         `json:"resolution,omitempty"`
	RecordingMode *string         `json:"recording_mode,omitempty"`
	PowerMode     *string         `json:"power_mode,omitempty"`
	UploadMode    *string         `json:"upload_mode,omitempty"`
	Schedule      json.RawMessage `json:"schedule,omitempty"`
	BatteryRules  json.RawMessage `json:"battery_rules,omitempty"`
}

// DeleteFootageResponse is the body of DELETE
// /api/v1/cameras/{deviceID}/footage. The endpoint processes deletions
// in batches bounded by server-side limits; HasMore is true when the
// batch was full, signalling the UI to call again to continue the
// purge.
type DeleteFootageResponse struct {
	DeletedCount   int    `json:"deleted_count"`
	BytesFreed     uint64 `json:"bytes_freed"`
	HasMore        bool   `json:"has_more"`
	RemainingCount int    `json:"remaining_count"`
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
	// PowerMode / UploadMode are what the camera is CURRENTLY effective
	// at (after schedule + battery-rule resolution), which can differ
	// from the manually-set values on the cameras row. UI shows both
	// so the operator sees what their schedule is doing.
	PowerMode  *string `json:"power_mode,omitempty"`
	UploadMode *string `json:"upload_mode,omitempty"`
	// BatteryPct is 0–100 only when a battery-sensing HAT is wired up
	// (see GH issue #73). nil on grid-powered cameras.
	BatteryPct *uint8 `json:"battery_pct,omitempty"`
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
	// UploadedToS3 distinguishes uploaded vs lazy-mode local-only
	// segments so the UI can render them differently (hatched fill,
	// "fetching…" state on scrub).
	UploadedToS3 bool `json:"uploaded_to_s3"`
}

// CoverageResponse is the body of GET /hls/{id}/coverage.
type CoverageResponse struct {
	Segments []CoverageSegment `json:"segments"`
}

// ====================================================================
// Billing
// ====================================================================

// TierInfo is the per-tier entry returned by GET /api/v1/billing/tiers.
// For paid tiers the ID is a Stripe price ID (e.g. "price_1ABC..."); for
// the free tier the ID is the literal string "free". Nil limits mean
// unlimited. Price/currency/interval are zero for the free tier.
type TierInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CameraLimit *int   `json:"camera_limit"`
	StorageGB   *int   `json:"storage_gb"`
	PriceCents  int64  `json:"price_cents"`
	Currency    string `json:"currency"`
	Interval    string `json:"interval"` // "month" / "year" / ""
}

// ListTiersResponse is the body of GET /api/v1/billing/tiers.
type ListTiersResponse struct {
	Tiers []TierInfo `json:"tiers"`
}

// SubscriptionResponse is the body of GET /api/v1/billing/subscription.
// Tier carries the current tier identifier (Stripe price ID or "free");
// TierName is the human-readable display name from the Stripe product, or
// "Free" for the unpaid tier.
type SubscriptionResponse struct {
	Tier     string `json:"tier"`
	TierName string `json:"tier_name"`
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
// Admin billing
// ====================================================================

// AdminBillingTier is the per-price entry returned by GET
// /api/v1/admin/billing/tiers. Unlike the public TierInfo — which only
// surfaces tiers that already have valid ghostcam metadata — this struct
// describes every active Stripe price in the account, including those
// that have not yet been configured. The admin UI uses it to render an
// editable table so the operator can see all products and assign limits.
type AdminBillingTier struct {
	PriceID     string `json:"price_id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	PriceCents  int64  `json:"price_cents"`
	Currency    string `json:"currency"`
	Interval    string `json:"interval"`
	// Raw metadata strings as stored on the Stripe product. Empty string
	// means the key is unset. The admin UI converts these to numbers or
	// the "Unlimited" label for display.
	CameraLimitRaw string `json:"camera_limit_raw"`
	StorageGBRaw   string `json:"storage_gb_raw"`
	// Configured is true when both metadata keys are present — i.e. the
	// product is complete enough for the tier cache to accept it.
	Configured bool `json:"configured"`
}

// AdminListBillingTiersResponse is the body of GET
// /api/v1/admin/billing/tiers.
type AdminListBillingTiersResponse struct {
	Tiers []AdminBillingTier `json:"tiers"`
}

// AdminUpdateBillingTierRequest is the body of PATCH
// /api/v1/admin/billing/tiers/{priceID}. Either limit field set to null
// means "unlimited" for that dimension. Name is optional — omit or pass
// an empty string to leave the product name unchanged. The two limit
// fields are always applied together to prevent half-configured products.
type AdminUpdateBillingTierRequest struct {
	CameraLimit *int   `json:"camera_limit"`
	StorageGB   *int   `json:"storage_gb"`
	Name        string `json:"name,omitempty"`
}

// AdminCreateBillingTierRequest is the body of POST
// /api/v1/admin/billing/tiers. Creates a brand-new Stripe product and a
// single recurring price on it in one call. The server validates the
// inputs and, on success, refreshes the tier cache so the new tier
// appears immediately in the public settings dialog.
type AdminCreateBillingTierRequest struct {
	Name        string `json:"name"`
	CameraLimit *int   `json:"camera_limit"` // null = unlimited
	StorageGB   *int   `json:"storage_gb"`   // null = unlimited
	PriceCents  int64  `json:"price_cents"`  // non-negative; 0 rejected for paid tiers
	Currency    string `json:"currency"`     // 3-letter ISO, e.g. "usd"
	Interval    string `json:"interval"`     // "month" or "year"
}

// AdminArchiveBillingTierRequest is the body of POST
// /api/v1/admin/billing/tiers/{priceID}/archive. Archives the Stripe
// price (and the product if this was its last active price). If the
// price has live subscribers the server returns 409 with an
// ActiveSubscribers count unless Confirm is true — the UI uses this
// to gate a "yes, I know, archive anyway" dialog so CFOs don't
// accidentally orphan a paid customer.
type AdminArchiveBillingTierRequest struct {
	Confirm bool `json:"confirm"`
}

// AdminArchiveConflictResponse is returned with HTTP 409 when the
// archive target still has active subscribers and Confirm was false.
// The UI uses the count to phrase an informed confirmation prompt.
type AdminArchiveConflictResponse struct {
	Error             string `json:"error"`
	ActiveSubscribers int64  `json:"active_subscribers"`
}

// AdminBillingTierSubscribersResponse is the body of
// GET /api/v1/admin/billing/tiers/{priceID}/subscribers.
// A tiny probe used by the Reprice dialog to tell the admin how many
// people will be affected before they commit.
type AdminBillingTierSubscribersResponse struct {
	ActiveSubscribers int64 `json:"active_subscribers"`
}

// AdminRepriceBillingTierRequest is the body of POST
// /api/v1/admin/billing/tiers/{priceID}/reprice.
//
// Stripe prices are immutable — the only way to "change" a price is
// to create a new one on the same product and archive the old one.
// This endpoint wraps that dance in one atomic admin operation and,
// optionally, migrates existing subscribers off the old price.
//
//	PriceCents                  New amount, in the minor currency unit.
//	                            Currency and interval are copied from
//	                            the existing price — Stripe does not
//	                            allow changing those on a subscription.
//	MigrateSubscribers          If true, every active subscription on
//	                            the old price gets its item updated
//	                            to the new price ID in one call.
//	Prorate                     Only meaningful when MigrateSubscribers
//	                            is true. true = Stripe calculates
//	                            prorations for the switch; false =
//	                            no prorations, customer is on the new
//	                            price starting next invoice.
//	ConfirmDroppingSubscribers  Only meaningful when MigrateSubscribers
//	                            is false and the old price has >0
//	                            active subscribers. Admin is
//	                            explicitly accepting that those
//	                            subscribers will be dropped to free
//	                            on their next API call.
type AdminRepriceBillingTierRequest struct {
	PriceCents                 int64 `json:"price_cents"`
	MigrateSubscribers         bool  `json:"migrate_subscribers"`
	Prorate                    bool  `json:"prorate"`
	ConfirmDroppingSubscribers bool  `json:"confirm_dropping_subscribers"`
}

// AdminRepriceBillingTierResponse is the success body of reprice.
// Carries the fresh admin tier list plus the count of subscribers
// that were migrated so the UI can surface feedback.
type AdminRepriceBillingTierResponse struct {
	Tiers         []AdminBillingTier `json:"tiers"`
	MigratedCount int                `json:"migrated_count"`
}

// ====================================================================
// Admin users
// ====================================================================

// AdminUser is a platform-wide view of a user for the admin Users
// section. Joined with admin status, subscription tier, and camera
// count so rendering the list never requires a per-row fan-out.
//
// `Tier` is the raw tier identifier stored on the subscription row
// (either "free" or a Stripe price ID). `TierName` is the resolved
// display name from the billing cache — "Free", "Ghostcam Pro", etc.
// The UI should render TierName; Tier stays available for equality
// comparisons and debugging drift.
type AdminUser struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	CreatedAt   int64  `json:"created_at"`
	VerifiedAt  *int64 `json:"verified_at,omitempty"`
	DisabledAt  *int64 `json:"disabled_at,omitempty"`
	DeletedAt   *int64 `json:"deleted_at,omitempty"`
	IsAdmin     bool   `json:"is_admin"`
	Tier        string `json:"tier"`
	TierName    string `json:"tier_name"`
	CameraCount int64  `json:"camera_count"`
}

// AdminListUsersResponse is the body of GET /api/v1/admin/users.
type AdminListUsersResponse struct {
	Users []AdminUser `json:"users"`
}

// AdminCreateUserRequest is the body of POST /api/v1/admin/users.
// The admin supplies email + display name; the server generates a
// random initial password and returns it in the response exactly once.
type AdminCreateUserRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// AdminCreateUserResponse is the success body of POST /api/v1/admin/users.
// The server sends an invite email to the new user with a set-password
// link — the admin does not need to share credentials manually.
type AdminCreateUserResponse struct {
	User AdminUser `json:"user"`
}

// AdminUpdateUserRequest is the body of PATCH /api/v1/admin/users/{id}.
// Today the only supported mutation is toggling the disabled flag; the
// struct uses a pointer so "not sent" is distinct from "set to false".
type AdminUpdateUserRequest struct {
	Disabled *bool `json:"disabled,omitempty"`
}

// AdminResetPasswordResponse is the success body of POST
// /api/v1/admin/users/{id}/reset-password. Shape matches the create
// response so the UI can reuse its one-time-password reveal dialog.
type AdminResetPasswordResponse struct {
	GeneratedPassword string `json:"generated_password"`
}

// ====================================================================
// Admin cameras
// ====================================================================

// AdminCamera is a platform-wide view of a camera for the admin
// Cameras section. Joined with owner email so the UI doesn't have to
// secondary-fetch against the users list for each row.
type AdminCamera struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name"`
	UserID      string `json:"user_id"`
	OwnerEmail  string `json:"owner_email"`
	EnrolledAt  int64  `json:"enrolled_at"`
	LastSeenAt  *int64 `json:"last_seen_at,omitempty"`
}

// AdminListCamerasResponse is the body of GET /api/v1/admin/cameras.
type AdminListCamerasResponse struct {
	Cameras []AdminCamera `json:"cameras"`
}

// AdminReassignCameraRequest is the body of PATCH
// /api/v1/admin/cameras/{deviceID}. The server validates that the
// target user isn't already at their tier limit and rejects with 409
// if they are — reassignment never silently starves an existing camera.
type AdminReassignCameraRequest struct {
	UserID string `json:"user_id"`
}

// AdminReassignCameraConflictResponse is the HTTP 409 body returned
// when the target user's tier limit would be exceeded by the move.
// Shape mirrors AdminArchiveConflictResponse so the UI can handle
// 409 with a single pattern.
type AdminReassignCameraConflictResponse struct {
	Error       string `json:"error"`
	CameraLimit int    `json:"camera_limit"`
	CameraCount int64  `json:"camera_count"`
}

// ====================================================================
// Client diagnostics
// ====================================================================

// ClientLogEntry is the body of POST /api/v1/client-log. The endpoint is
// gated behind an authenticated session and the UI only fires it when
// the "Client error logging" developer setting is enabled, so there is no
// privacy surprise — the user deliberately opts in when they need remote
// debugging from a mobile browser.
type ClientLogEntry struct {
	// Level maps to slog levels: "debug", "info", "warn", "error".
	// Unknown values fall back to "info".
	Level string `json:"level"`
	// Source is a free-form tag for grepping (e.g. "hls", "billing").
	Source string `json:"source"`
	// Message is the main log line.
	Message string `json:"message"`
	// UserAgent and URL are optional context captured by the client.
	UserAgent string `json:"user_agent,omitempty"`
	URL       string `json:"url,omitempty"`
	// Context is an open-ended string map for structured extras
	// (e.g. HLS error type / status code / device ID).
	Context map[string]string `json:"context,omitempty"`
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

// PiImage is a single downloadable Pi device image served by
// GET /api/v1/firmware/images. One per device (zero2w / pi4 / pi5).
type PiImage struct {
	Device      string `json:"device"`
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
}

// PiImagesResponse is the body of GET /api/v1/firmware/images.
// Only populated devices are included — if the webhook has not ingested
// a given device yet, it is omitted.
type PiImagesResponse struct {
	Images []PiImage `json:"images"`
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
