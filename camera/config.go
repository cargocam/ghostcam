package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

// CameraConfig is the fully resolved camera configuration. The struct
// itself lives in internal/state so every subpackage that needs a knob
// can import the type without pulling in package main; this alias keeps
// the main-package spelling unchanged at call sites here.
type CameraConfig = state.CameraConfig

// cameraConfigFile is the TOML-deserialized config file. All fields optional.
type cameraConfigFile struct {
	ServerURL            *string `toml:"server_url"`
	TestSource           *bool   `toml:"test_source"`
	SegmentDir           *string `toml:"segment_dir"`
	DataDir              *string `toml:"data_dir"`
	NoGPS                *bool   `toml:"no_gps"`
	VideoWidth           *uint32 `toml:"video_width"`
	VideoHeight          *uint32 `toml:"video_height"`
	VideoFPS             *uint32 `toml:"video_fps"`
	VideoBitrate         *uint32 `toml:"video_bitrate"`
	VideoKeyframeInterval *uint32 `toml:"video_keyframe_interval"`
	CellularAPN          *string `toml:"cellular_apn"`
	CellularUser         *string `toml:"cellular_user"`
	CellularPass         *string `toml:"cellular_pass"`
}

// LoadConfig parses flags, env vars, and an optional TOML config file.
// Layering: defaults -> config file -> env vars -> CLI flags.
func LoadConfig() (*CameraConfig, error) {
	var (
		configPath = flag.String("config", "", "path to TOML config file")
		serverURL      = flag.String("server-url", "", "server HTTPS URL")
		provisionToken = flag.String("provision-token", "", "one-time token for headless provisioning")
		provisionHTTPAddr = flag.String("provision-http-addr", "", "bind address (host:port) for the local offline provisioning HTTP server (USB gadget / SoftAP); empty disables it")
		testSource     = flag.Bool("test-source", false, "use ffmpeg test source instead of real capture")
		segmentDir = flag.String("segment-dir", "", "directory for segment ring buffer")
		dataDir    = flag.String("data-dir", "", "data directory")
		noGPS      = flag.Bool("no-gps", false, "disable GPS")
		noAudio    = flag.Bool("no-audio", false, "disable audio capture")
		audioDevice = flag.String("audio-device", "", "ALSA audio device name")
		audioGainDB = flag.Int("audio-gain-db", 0, "ffmpeg -af volume in dB; 20-30 dB typical for I2S MEMS mics (INMP441 etc.); 0 disables")
		abrEnabled  = flag.Bool("abr", false, "adaptive bitrate: ratchet resolution/bitrate based on observed packet loss")
		abrStartTier = flag.String("abr-start-tier", "", "starting ABR tier name (minimum/low/medium/high); default minimum")
		cellularAPN = flag.String("cellular-apn", "", "APN for the cellular data connection; when set, the daemon provisions a NetworkManager gsm connection with it at startup")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	file := findAndLoadConfig(*configPath)

	// Resolve data_dir: CLI -> env -> file -> default
	resolvedDataDir := coalesceStr(
		*dataDir,
		envOpt("GHOSTCAM_DATA_DIR"),
		ptrStr(file.DataDir),
		"/var/ghostcam",
	)

	// Resolve server_url: CLI -> env -> file -> stored file -> default
	resolvedServerURL := coalesceStr(
		*serverURL,
		envOpt("GHOSTCAM_SERVER_URL"),
		ptrStr(file.ServerURL),
		readStoredServerURL(resolvedDataDir),
		"",
	)

	resolvedProvisionToken := coalesceStr(
		*provisionToken,
		envOpt("GHOSTCAM_PROVISION_TOKEN"),
		"",
	)

	// Local provisioning HTTP server bind addr: CLI -> env -> disabled.
	// The gadget/SoftAP systemd layer sets GHOSTCAM_PROVISION_HTTP_ADDR
	// (e.g. 10.55.0.1:80) once its link interface is up; unset elsewhere
	// so the server never binds on a device without that link.
	resolvedProvisionHTTPAddr := coalesceStr(
		*provisionHTTPAddr,
		envOpt("GHOSTCAM_PROVISION_HTTP_ADDR"),
		"",
	)

	resolvedTestSource := *testSource || ptrBool(file.TestSource)

	resolvedSegmentDir := coalesceStr(
		*segmentDir,
		envOpt("GHOSTCAM_SEGMENT_DIR"),
		ptrStr(file.SegmentDir),
		filepath.Join(resolvedDataDir, "segments"),
	)

	resolvedNoGPS := *noGPS || ptrBool(file.NoGPS)

	// Video profile presets
	profileW, profileH, profileBR, profileKF := resolveVideoProfile(envOpt("GHOSTCAM_VIDEO_PROFILE"))

	videoWidth := coalesceUint32(
		envUint32("GHOSTCAM_VIDEO_WIDTH"),
		profileW,
		ptrUint32(file.VideoWidth),
		1280,
	)
	videoHeight := coalesceUint32(
		envUint32("GHOSTCAM_VIDEO_HEIGHT"),
		profileH,
		ptrUint32(file.VideoHeight),
		720,
	)
	videoFPS := coalesceUint32(
		envUint32("GHOSTCAM_VIDEO_FPS"),
		0,
		ptrUint32(file.VideoFPS),
		30,
	)
	videoBitrate := coalesceUint32(
		envUint32("GHOSTCAM_VIDEO_BITRATE"),
		profileBR,
		ptrUint32(file.VideoBitrate),
		2_000_000,
	)
	videoKeyframeInterval := coalesceUint32(
		envUint32("GHOSTCAM_VIDEO_KEYFRAME_INTERVAL"),
		profileKF,
		ptrUint32(file.VideoKeyframeInterval),
		30,
	)

	resolvedNoAudio := *noAudio
	resolvedAudioDevice := coalesceStr(*audioDevice, envOpt("GHOSTCAM_AUDIO_DEVICE"), "")
	resolvedAudioGainDB := *audioGainDB
	if resolvedAudioGainDB == 0 {
		if v := envOpt("GHOSTCAM_AUDIO_GAIN_DB"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				resolvedAudioGainDB = n
			}
		}
	}

	// Runtime overrides: resolution and recording_mode persisted by command handlers
	if stored := readStoredFile(resolvedDataDir, "resolution"); stored != "" {
		if sw, sh, sbr, skf := resolveVideoProfile(stored); sw > 0 {
			slog.Info("applying stored resolution override", "resolution", stored)
			videoWidth, videoHeight, videoBitrate, videoKeyframeInterval = sw, sh, sbr, skf
		}
	}

	// Default is "never" (streaming-only) — a freshly-flashed camera doesn't
	// start uploading segments until the user explicitly opts into recording.
	// This matches the DB column default so a brand-new enrollment behaves the
	// same whether or not the server has yet issued a set_recording_mode
	// command. Existing installs already have a value persisted on disk, so
	// this default only applies to fresh data dirs.
	//
	// Resolution order mirrors the other settings:
	//   1. persisted file at {dataDir}/recording_mode (written by the server
	//      via set_recording_mode) — primary source of truth after first use.
	//   2. GHOSTCAM_RECORDING_MODE env var — for fixtures (docker-compose
	//      test cameras) and anyone who wants to pin a mode at provisioning
	//      time before the server has had a chance to set it.
	//   3. built-in "never" default.
	recordingMode := "never"
	if env := envOpt("GHOSTCAM_RECORDING_MODE"); env != "" {
		recordingMode = env
	}
	if stored := readStoredFile(resolvedDataDir, "recording_mode"); stored != "" {
		recordingMode = stored
		slog.Info("applying stored recording mode override", "mode", stored)
	}

	// Local storage cap: env -> partition-aware default (#115).
	//
	// History: the default used to be a static 4 GB, which on a Pi Zero
	// 2W image (3.9 GB root partition) is *larger* than the partition.
	// The watcher's oldest-first eviction never fired before the
	// filesystem physically filled, surfacing as the disk-full incident
	// during the #107 24h soak. New default is whichever is smaller of
	// 4 GB or 50 % of the segment-dir partition's total size, floored at
	// 256 MB so a tiny test partition doesn't degenerate to zero. Env
	// override still wins unconditionally — operators with a separate
	// /var/ghostcam mount (Pi 4 + USB SSD) can pick any value.
	localStorageCap := defaultLocalStorageCapBytes(resolvedSegmentDir)
	if v := envOpt("GHOSTCAM_LOCAL_STORAGE_CAP_MB"); v != "" {
		if mb, err := strconv.ParseUint(v, 10, 64); err == nil && mb > 0 {
			localStorageCap = mb * 1024 * 1024
		}
	}

	resolvedABREnabled := *abrEnabled
	if !resolvedABREnabled {
		if v := envOpt("GHOSTCAM_ABR"); v == "1" || v == "true" {
			resolvedABREnabled = true
		}
	}
	resolvedABRStartTier := coalesceStr(*abrStartTier, envOpt("GHOSTCAM_ABR_START_TIER"), "minimum")

	// Power mode: persisted file at {dataDir}/power_mode (written by
	// HandleSetPowerMode), falling back to env, then to "live" so a
	// brand-new install behaves like the current always-on default
	// until the operator explicitly sets a mode.
	powerMode := "live"
	if env := envOpt("GHOSTCAM_POWER_MODE"); env != "" {
		powerMode = env
	}
	if stored := readStoredFile(resolvedDataDir, "power_mode"); stored != "" {
		powerMode = stored
		slog.Info("applying stored power mode override", "mode", stored)
	}
	if powerMode != "live" && powerMode != "standby" && powerMode != "sleep" {
		slog.Warn("invalid stored power mode, falling back to live", "mode", powerMode)
		powerMode = "live"
	}

	// Battery HAT (#73). No persistence on disk — driver selection is a
	// provisioning-time decision (which HAT the operator wired up) and
	// is set via env var or /boot/ghostcam.conf, not via a server
	// command. Empty string ⇒ no HAT registered ⇒ telemetry's battery
	// section stays nil, which is the right answer for grid-powered
	// cameras and any dev build.
	batteryHAT := envOpt("GHOSTCAM_BATTERY_HAT")
	batteryI2CBus := coalesceStr(envOpt("GHOSTCAM_BATTERY_I2C_BUS"), "/dev/i2c-1")

	// Cellular APN: CLI -> env -> file -> empty (auto). User/Pass: env ->
	// file. Empty APN leaves cellular to ModemManager/NM auto-config.
	resolvedCellularAPN := coalesceStr(*cellularAPN, envOpt("GHOSTCAM_CELLULAR_APN"), ptrStr(file.CellularAPN), "")
	resolvedCellularUser := coalesceStr(envOpt("GHOSTCAM_CELLULAR_USER"), ptrStr(file.CellularUser), "")
	resolvedCellularPass := coalesceStr(envOpt("GHOSTCAM_CELLULAR_PASS"), ptrStr(file.CellularPass), "")

	cfg := &CameraConfig{
		ServerURL:             resolvedServerURL,
		ProvisionToken:        resolvedProvisionToken,
		ProvisionHTTPAddr:     resolvedProvisionHTTPAddr,
		TestSource:            resolvedTestSource,
		SegmentDir:            resolvedSegmentDir,
		DataDir:               resolvedDataDir,
		NoGPS:                 resolvedNoGPS,
		NoAudio:               resolvedNoAudio,
		AudioDevice:           resolvedAudioDevice,
		AudioGainDB:           resolvedAudioGainDB,
		VideoWidth:            videoWidth,
		VideoHeight:           videoHeight,
		VideoFPS:              videoFPS,
		VideoBitrate:          videoBitrate,
		VideoKeyframeInterval: videoKeyframeInterval,
		RecordingMode:         recordingMode,
		LocalStorageCapBytes:  localStorageCap,
		ABREnabled:            resolvedABREnabled,
		ABRStartTier:          resolvedABRStartTier,
		PowerMode:             powerMode,
		BatteryHAT:            batteryHAT,
		BatteryI2CBus:         batteryI2CBus,
		CellularAPN:           resolvedCellularAPN,
		CellularUser:          resolvedCellularUser,
		CellularPass:          resolvedCellularPass,
	}

	if cfg.DataDir == "" {
		return nil, fmt.Errorf("data directory must not be empty")
	}

	return cfg, nil
}

func findAndLoadConfig(cliPath string) cameraConfigFile {
	candidates := []string{
		cliPath,
		envOpt("GHOSTCAM_CONFIG_FILE"),
	}
	if dd := envOpt("GHOSTCAM_DATA_DIR"); dd != "" {
		candidates = append(candidates, filepath.Join(dd, "camera.toml"))
	}
	candidates = append(candidates, "/boot/ghostcam.conf")

	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		slog.Info("loading config file", "path", p)
		var f cameraConfigFile
		if _, err := toml.DecodeFile(p, &f); err != nil {
			slog.Warn("failed to parse config file", "path", p, "err", err)
			continue
		}
		return f
	}
	return cameraConfigFile{}
}

func readStoredServerURL(dataDir string) string {
	return readStoredFile(dataDir, "server_url")
}

func readStoredFile(dataDir, name string) string {
	data, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		return ""
	}
	return trimString(string(data))
}

// WriteStoredFile persists a runtime config override to dataDir.
func WriteStoredFile(dataDir, name, value string) error {
	return os.WriteFile(filepath.Join(dataDir, name), []byte(value), 0644)
}

func resolveVideoProfile(profile string) (w, h, br, kf uint32) {
	switch profile {
	case "zero2w", "480p":
		slog.Info("applying video profile", "profile", "zero2w/480p")
		return 854, 480, 750_000, 30
	case "pi4", "720p":
		slog.Info("applying video profile", "profile", "pi4/720p")
		return 1280, 720, 2_000_000, 30
	case "pi5", "1080p":
		slog.Info("applying video profile", "profile", "pi5/1080p")
		return 1920, 1080, 4_000_000, 30
	case "":
		return 0, 0, 0, 0
	default:
		slog.Warn("unknown video profile, ignoring", "profile", profile)
		return 0, 0, 0, 0
	}
}

// --- helpers ---

func envOpt(key string) string {
	v := os.Getenv(key)
	return v
}

func envUint32(key string) uint32 {
	s := os.Getenv(key)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		slog.Warn("could not parse env var, ignoring", "var", key, "value", s)
		return 0
	}
	return uint32(v)
}

func coalesceStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func coalesceUint32(vals ...uint32) uint32 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func ptrUint32(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}

func trimString(s string) string {
	// Trim whitespace and newlines
	b := []byte(s)
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\n' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	return string(b[start:end])
}

// defaultLocalStorageCapBytes picks a sensible default for the segment
// ring-buffer cap based on the partition that hosts dir. Returns
// whichever is smaller of 4 GB or 50 % of the partition's total size,
// floored at 256 MB. Falls back to 4 GB if statfs fails.
//
// Background (#115): the prior static-4-GB default was larger than the
// Pi Zero 2W image's 3.9 GB root partition. The segment watcher's
// oldest-first eviction never fired before the filesystem physically
// filled, surfacing during the #107 soak as a 3-hour disk-full
// incident. Sizing relative to the partition keeps eviction useful on
// the Zero 2W's tight root and still respects bigger mounts (Pi 4 +
// USB SSD) when the env override is left unset.
func defaultLocalStorageCapBytes(dir string) uint64 {
	const (
		ceiling = uint64(4 * 1024 * 1024 * 1024) // 4 GB
		floor   = uint64(256 * 1024 * 1024)      // 256 MB
	)
	total := partitionTotalBytes(dir)
	if total == 0 {
		slog.Warn("could not size partition for storage cap, using 4 GB ceiling", "dir", dir)
		return ceiling
	}
	half := total / 2
	switch {
	case half > ceiling:
		slog.Info("local storage cap set to 4 GB ceiling", "partition_bytes", total)
		return ceiling
	case half < floor:
		slog.Info("local storage cap set to 256 MB floor", "partition_bytes", total)
		return floor
	default:
		slog.Info("local storage cap derived from partition size",
			"partition_bytes", total, "cap_bytes", half)
		return half
	}
}

// partitionTotalBytes returns the total byte size of the partition
// hosting dir, or 0 on error. Walks up the path if dir doesn't yet
// exist (LoadConfig runs before MkdirAll in main).
func partitionTotalBytes(dir string) uint64 {
	for dir != "" {
		var s syscall.Statfs_t
		if err := syscall.Statfs(dir, &s); err == nil {
			return uint64(s.Bsize) * s.Blocks
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0
		}
		dir = parent
	}
	return 0
}
