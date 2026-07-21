// Package common defines shared request/response types for the camera-server HTTP API.
//
// The camera is now Go (camera/, see go-camera-rewrite cutover) and uses
// these types directly. The UI also consumes them via tygo, which emits
// TypeScript to ui/src/lib/api-types/ — directive lives in
// server/apitypes/apitypes.go and runs as part of `go generate ./...`.
package common

// ProvisionRequest is sent by the camera during provisioning. The camera
// sends its ed25519 public key for registration (like adding to SSH
// authorized_keys). The server derives device_id from the public key.
type ProvisionRequest struct {
	Token        string `json:"token"`
	DeviceSerial string `json:"device_serial"`
	PublicKey    string `json:"public_key"` // hex-encoded ed25519 public key (64 chars)
	FwVersion    string `json:"fw_version,omitempty"`
	// SIMImsi is the cellular SIM's IMSI, when the camera has a modem
	// reachable via ModemManager (mmcli -m 0). Empty on grid-powered /
	// WiFi-only units. Recorded on the cameras row so DELETE camera
	// can deactivate the SIM in Soracom (#74), and so the future
	// per-camera cellular billing add-on (#117) knows which cameras
	// carry an active Ghostcam SIM. The camera reads this with a
	// short-timeout best-effort call — never blocks provisioning on
	// modem state.
	SIMImsi string `json:"sim_imsi,omitempty"`
}

// ProvisionResponse acknowledges key registration. No secret is returned.
type ProvisionResponse struct {
	DeviceID string `json:"device_id"` // echoed back for confirmation
	Status   string `json:"status"`    // "registered"
}

// TelemetryPollRequest is sent by the camera every 10s with current sensor readings.
//
// RollbackEvent carries the contents of /var/ghostcam/rollback_event.json
// when ExecStartPre took the rollback branch (a newly-installed firmware
// failed to write boot_ok before its next exit). The camera consumes the
// marker on the first telemetry tick after recovery — single-shot
// delivery, the file is deleted after read. Server surfaces this as a
// `firmware_rollback` event so a fleet-wide bad-firmware regression is
// visible from the dashboard.
type TelemetryPollRequest struct {
	Telemetry     TelemetryDatagram `json:"telemetry"`
	FwVersion     string            `json:"fw_version,omitempty"`
	RollbackEvent string            `json:"rollback_event,omitempty"` // raw JSON from rollback_event.json
	// DiagBundles drains any diag_bundle command results the camera
	// captured since the previous poll. Server persists each by DiagID
	// and clears the camera's pending slice on a 2xx response. Empty in
	// the common case (no diag_bundle command was issued).
	DiagBundles []DiagBundle `json:"diag_bundles,omitempty"`
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
	// WHIPSessionMissing is set when the server has *no* active pion
	// session for this device. The camera reacts by force-closing its
	// current publisher so the outer reconnect loop negotiates a fresh
	// WHIP — this catches the "server restarted while camera ICE
	// keepalive still thinks the connection is healthy" zombie state.
	// Polarity is "missing" rather than "alive" so an older server
	// build (no field) defaults to the safe value (no spurious
	// reconnects).
	WHIPSessionMissing bool `json:"whip_session_missing,omitempty"`
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
//
// Diagnostic / rescue commands (see ghostcam#119):
//   - diag_bundle              → DiagID field; camera returns a DiagBundle
//                                via TelemetryPollRequest.DiagBundles
//   - restart_service          → no payload; systemctl restart ghostcam-camera
//   - restart_modem_manager    → no payload; systemctl restart ModemManager
//   - restart_network_manager  → no payload; systemctl restart NetworkManager
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

	// set_cellular: provision/update the cellular data APN on a deployed
	// camera without SSH. The daemon persists these and applies them via
	// network.EnsureCellular. This is how the field-durable APN fix reaches
	// already-provisioned cameras (the provisioning payload only covers
	// onboarding). Empty CellularAPN with type set_cellular is a no-op.
	CellularAPN  string `json:"cellular_apn,omitempty"`
	CellularUser string `json:"cellular_user,omitempty"`
	CellularPass string `json:"cellular_pass,omitempty"`

	// Diagnostic correlation id. Only set for diag_bundle; copied
	// verbatim into the resulting DiagBundle so the server can match
	// asynchronous responses to its issuance audit row.
	DiagID string `json:"diag_id,omitempty"`
}

// DiagBundle is the read-only inspection payload the camera returns in
// response to a diag_bundle command. Every field is captured by running
// a fixed argv with no operator-supplied input; missing subcommands or
// non-zero exits leave the field empty rather than failing the whole
// bundle. Each field is independently truncated to ~32 KB so journalctl
// / dmesg can't blow up the total response.
//
// JSON kept narrow and consistent so the server can store the body as
// JSONB and add new fields later without a migration.
type DiagBundle struct {
	DiagID        string `json:"diag_id"`
	CapturedAt    int64  `json:"captured_at"` // epoch ms
	ModemList     string `json:"modem_list,omitempty"`     // mmcli -L
	ModemDetail   string `json:"modem_detail,omitempty"`   // mmcli -m 0 (omitted if no modem)
	NMConnections string `json:"nm_connections,omitempty"` // nmcli -t -f NAME,UUID,TYPE,DEVICE con show
	NMDevices     string `json:"nm_devices,omitempty"`     // nmcli -t -f DEVICE,TYPE,STATE,CONNECTION dev status
	IPAddr        string `json:"ip_addr,omitempty"`        // ip -4 -o addr
	IPRoute       string `json:"ip_route,omitempty"`       // ip route
	ServiceStatus string `json:"service_status,omitempty"` // systemctl --no-pager status ghostcam-camera
	ServiceLogs   string `json:"service_logs,omitempty"`   // journalctl --no-pager -u ghostcam-camera --since=1h
	Dmesg         string `json:"dmesg,omitempty"`          // journalctl --no-pager -k --since=1h
	Disk          string `json:"disk,omitempty"`           // df -h
	Mem           string `json:"mem,omitempty"`            // free -m
	Uptime        string `json:"uptime,omitempty"`         // uptime
	PkgVersion    string `json:"pkg_version,omitempty"`    // dpkg-query -W ghostcam-camera
}

// PresignRequest requests presigned PUT URLs and confirms previously
// uploaded segments. The `pending` field declares segments the camera
// is *about* to upload but hasn't yet — server uses it to surface a
// "blue ghost" placeholder in the viewer timeline before the actual
// S3 PUT lands. Lags upload by at most one presign cycle, which is
// still way faster than waiting for the confirm-on-next-cycle round
// trip the timeline currently uses.
type PresignRequest struct {
	Count    uint32            `json:"count"`
	Uploaded []UploadedSegment `json:"uploaded,omitempty"`
	Pending  []UploadedSegment `json:"pending,omitempty"`
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

// LocalManifestRequest is the body of
// POST /api/v1/cameras/{deviceID}/local-manifest. A lazy-mode camera
// posts a manifest of segments it has on disk but has NOT yet uploaded
// to S3, so the server's timeline / coverage bar can show the user
// that footage exists even if it's not fetchable until they scrub to
// it. On scrub, the server queues an `upload_segments` command which
// pulls the bytes on demand.
//
// Manifest entries are inserted with `uploaded_to_s3 = FALSE`. When
// the camera later uploads a segment (because of a scrub-triggered
// command), the presign-confirm path flips it to TRUE.
type LocalManifestRequest struct {
	Segments []UploadedSegment `json:"segments"`
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
	// Optional cellular APN provisioning. Lets a cellular-only camera be
	// handed its SIM's APN at onboarding time (nothing in the stack
	// auto-creates a mobile-broadband connection, so a SIM whose APN
	// isn't in ModemManager's DB never connects). Short keys to keep the
	// QR dense. CellularUser/Pass are for the APNs that need PAP/CHAP.
	CellularAPN  string `json:"ca,omitempty"`
	CellularUser string `json:"cu,omitempty"`
	CellularPass string `json:"cp,omitempty"`
}
