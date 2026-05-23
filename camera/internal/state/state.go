// Package state holds cross-package atomic state and shared types for
// the camera daemon's internal subpackages. It exists to break the
// cycles that arise from the capture supervisor in main coordinating
// abr, power, telemetry, and the capture pipeline — each of which
// would otherwise need to import another in a closed loop. Keeping
// these here also means the wire-coupling shape between subsystems is
// documented in one file rather than spread across globals.
package state

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// SegmentDurationSecs is the target HLS/fMP4 segment length. capture's
// ffmpeg flags use this directly; the upload-side watcher reads it to
// derive segment start timestamps from mtime.
const SegmentDurationSecs = 6

// CameraConfig is the fully resolved camera configuration. Lives in
// state (rather than the main package alongside LoadConfig) so every
// internal subpackage that needs a knob can import the type without
// pulling in package main.
type CameraConfig struct {
	ServerURL      string
	ProvisionToken string // one-time token for headless provisioning
	TestSource     bool
	SegmentDir     string
	DataDir        string
	NoGPS          bool
	NoAudio        bool
	AudioDevice    string
	AudioGainDB    int // ffmpeg -af volume=NdB; 0 disables the filter

	VideoWidth            uint32
	VideoHeight           uint32
	VideoFPS              uint32
	VideoBitrate          uint32
	VideoKeyframeInterval uint32
	RecordingMode         string // "constant", "motion", or "never" (streaming-only)
	LocalStorageCapBytes  uint64 // max local segment storage before eviction
	ABREnabled            bool   // adaptive bitrate: when true, abr.go drives rpicam-vid resolution+bitrate
	ABRStartTier          string // tier name to start at; default "minimum"
	PowerMode             string // "live" | "standby" | "sleep"; loaded from {dataDir}/power_mode
	BatteryHAT            string // battery HAT driver name; "" = no HAT, "pisugar3" = PiSugar 3 over I²C
	BatteryI2CBus         string // I²C bus device path for the battery HAT; default "/dev/i2c-1"
}

// Power-mode strings. Mirrored from the server's CameraCommand.PowerMode
// values so a forged command can be validated before being applied; see
// IsValidPowerMode. These constants live here (rather than in
// internal/power) so internal/battery can build default rules that
// reference them without importing power (which would cycle).
const (
	PowerModeLive    = "live"
	PowerModeStandby = "standby"
	PowerModeSleep   = "sleep"
)

// IsValidPowerMode is the same validation set the server applies in
// cameras.go before enqueueing the command. Mirrors it here so a
// rogue / forged command can't put the daemon into an undefined
// state.
func IsValidPowerMode(mode string) bool {
	switch mode {
	case PowerModeLive, PowerModeStandby, PowerModeSleep:
		return true
	}
	return false
}

// ABRTier is one rung on the quality ladder. Bitrate is in bits per
// second (matches rpicam-vid --bitrate units). Defined here rather
// than in internal/abr so internal/capture can read the active tier
// without importing abr (which itself depends on capture's Publisher
// type).
type ABRTier struct {
	Name    string
	Width   uint32
	Height  uint32
	Bitrate uint32
}

// VideoSendSample is a snapshot of the publisher's outbound video
// state for the ABR controller. LossRatio is the receiver-reported
// fraction (0..1) from the most recent RTCP Receiver Report;
// SamplesSent is the cumulative count of access units the publisher
// has written. Fresh is true iff an RR arrived recently enough that
// LossRatio reflects the current network — the controller refuses
// to tune on stale data.
//
// Defined in state so both capture (which produces samples) and abr
// (which consumes them) can share the shape without depending on
// each other.
type VideoSendSample struct {
	LossRatio   float64
	SamplesSent uint64
	Fresh       bool
}

// PublisherHandle is the minimum surface capture's WHIP Publisher
// exposes to the rest of the daemon. Telemetry-poll closes a
// publisher on WHIPSessionMissing; abr samples loss; the standby
// watchdog tears it down when no wake signal is fresh. Each only
// needs Close + SampleVideoStats — the full Publisher type stays in
// internal/capture so this package doesn't pull in pion.
type PublisherHandle interface {
	Close() error
	SampleVideoStats() (VideoSendSample, bool)
}

// currentPublisher holds the active WHIP publisher so the telemetry-poll
// goroutine can force-close it when the server reports the WHIP session
// is missing (server restart / crash recovery). The capture supervisor
// in main.go writes the publisher pointer here on each pipeline
// iteration; this is the only cross-goroutine handle.
var currentPublisher atomic.Pointer[PublisherHandle]

// SetCurrentPublisher atomically stores the active publisher handle.
// Pass nil when no publisher is active (sleep mode, between restarts).
func SetCurrentPublisher(p PublisherHandle) {
	if p == nil {
		currentPublisher.Store(nil)
		return
	}
	currentPublisher.Store(&p)
}

// CurrentPublisher returns the active publisher handle, or nil when no
// publisher is currently attached.
func CurrentPublisher() PublisherHandle {
	p := currentPublisher.Load()
	if p == nil {
		return nil
	}
	return *p
}

// requestPipelineRestart is set by the telemetry-poll goroutine when it
// detects the WHIP session is missing AND the local publisher is already
// nil — meaning capture is running without a live publisher (the initial
// WHIP connect probably failed during a server restart). The capture
// supervisor watches this flag and tears down the running pipeline so
// the outer reconnect loop negotiates a fresh WHIP handshake. Without
// this, the system has no path back to LIVE: pc.Disconnected fired,
// the initial reconnect attempt timed out, and the pipeline runs
// happily forever with pub=nil because nothing else triggers a retry.
var requestPipelineRestart atomic.Bool

// RequestPipelineRestart sets the restart flag. Producers: telemetry-poll
// (WHIPSessionMissing), abr (tier shift), commands (set_power_mode that
// changes the effective mode), battery rules.
func RequestPipelineRestart() {
	requestPipelineRestart.Store(true)
}

// ConsumePipelineRestart atomically clears the restart flag and
// returns whether it was set. The capture supervisor's watcher
// goroutine uses this so a single request fires exactly one restart.
func ConsumePipelineRestart() bool {
	return requestPipelineRestart.CompareAndSwap(true, false)
}

// IsPipelineRestartRequested reports whether the restart flag is set
// without consuming it. Used by main.go's `Store(false)` reset before
// arming the watcher.
func IsPipelineRestartRequested() bool {
	return requestPipelineRestart.Load()
}

// ResetPipelineRestart clears the flag without checking — main.go calls
// this at the top of each capture iteration to drop any stale request
// that fired before the new publisher was minted.
func ResetPipelineRestart() {
	requestPipelineRestart.Store(false)
}

// activeTier is the tier the next capture-pipeline spawn should run
// with. nil = use the static cfg.Video* defaults (ABR disabled or not
// yet decided). internal/capture reads this at runRealPipeline entry;
// internal/abr writes it as part of every tier shift.
var activeTier atomic.Pointer[ABRTier]

// ActiveTier returns the ABR-selected video tier override, or nil when
// ABR is disabled / hasn't decided yet. Capture code uses the override
// when present; falls back to cfg.Video* defaults otherwise.
func ActiveTier() *ABRTier { return activeTier.Load() }

// SetActiveTier publishes a new tier. Called from internal/abr on
// every tier shift; also seeded by NewABRController.
func SetActiveTier(t *ABRTier) { activeTier.Store(t) }

// Identity holds the camera's permanent ed25519 keypair and derived
// device ID. Generated on first boot, never regenerated — survives
// server switches. Lives in state so internal/bt's provisioning code
// can read it without importing package main.
type Identity struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	DeviceID   string // SHA-256(public_key)[:16] hex, 32 chars
}

// PublicKeyHex returns the hex-encoded public key for transmission to
// the server.
func (id *Identity) PublicKeyHex() string {
	return hex.EncodeToString(id.PublicKey)
}

// DeriveDeviceID returns the first 16 bytes of SHA-256(publicKey) as
// hex (32 chars). Exported because identity.go and provisioning code
// both need to construct device IDs from raw public keys.
func DeriveDeviceID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:16])
}

// Credentials holds the persisted camera identity and server binding.
type Credentials struct {
	DeviceID  string
	ServerURL string
	Identity  *Identity
}

// WriteStoredFile persists a runtime config override to dataDir. The
// command dispatcher (internal/commands) calls this to write
// recording_mode / power_mode / resolution / etc — each is a flat
// file the next LoadConfig will pick up.
func WriteStoredFile(dataDir, name, value string) error {
	return os.WriteFile(filepath.Join(dataDir, name), []byte(value), 0644)
}

// ReadTrimmedFile reads path, trims surrounding whitespace, and returns
// the contents. Returns "" on any error. Mirrors the helper that lived
// in credentials.go pre-refactor; lifted here so subpackages that need
// to read flat config files (provisioning under internal/bt, etc.)
// don't have to reach back into package main.
func ReadTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ClearCredentials removes server binding but preserves the camera's
// permanent identity (keypair). The camera will re-enter provisioning
// mode on next startup.
func ClearCredentials(dataDir string) {
	_ = os.Remove(filepath.Join(dataDir, "server_url"))
	// identity_key and identity_key.pub are NEVER removed.
}
