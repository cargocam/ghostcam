package camera

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

// CameraConfig is the fully resolved camera configuration.
type CameraConfig struct {
	ServerURL            string
	TestSource           bool
	SegmentDir           string
	DataDir              string
	NoGPS                bool
	NoAudio              bool
	AudioDevice          string
	VideoWidth           uint32
	VideoHeight          uint32
	VideoFPS             uint32
	VideoBitrate         uint32
	VideoKeyframeInterval uint32
}

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
}

// LoadConfig parses flags, env vars, and an optional TOML config file.
// Layering: defaults -> config file -> env vars -> CLI flags.
func LoadConfig() (*CameraConfig, error) {
	var (
		configPath = flag.String("config", "", "path to TOML config file")
		serverURL  = flag.String("server-url", "", "server HTTPS URL")
		testSource = flag.Bool("test-source", false, "use ffmpeg test source instead of real capture")
		segmentDir = flag.String("segment-dir", "", "directory for segment ring buffer")
		dataDir    = flag.String("data-dir", "", "data directory")
		noGPS      = flag.Bool("no-gps", false, "disable GPS")
		noAudio    = flag.Bool("no-audio", false, "disable audio capture")
		audioDevice = flag.String("audio-device", "", "ALSA audio device name")
	)
	flag.Parse()

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

	cfg := &CameraConfig{
		ServerURL:             resolvedServerURL,
		TestSource:            resolvedTestSource,
		SegmentDir:            resolvedSegmentDir,
		DataDir:               resolvedDataDir,
		NoGPS:                 resolvedNoGPS,
		NoAudio:               resolvedNoAudio,
		AudioDevice:           resolvedAudioDevice,
		VideoWidth:            videoWidth,
		VideoHeight:           videoHeight,
		VideoFPS:              videoFPS,
		VideoBitrate:          videoBitrate,
		VideoKeyframeInterval: videoKeyframeInterval,
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
	data, err := os.ReadFile(filepath.Join(dataDir, "server_url"))
	if err != nil {
		return ""
	}
	return trimString(string(data))
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
