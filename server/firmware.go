package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/s3"
)

// piImageDevices is the fixed set of device slugs we build images for.
// Kept in sync with the `device` matrix in .github/workflows/release.yml.
//
// IMPORTANT: piImageAssetRe below hard-codes the same three slugs in
// its first capture group. If you add a device here, update the regex
// in the same commit — both must list the same set.
var piImageDevices = []string{"zero2w", "pi4", "pi5"}

// piImageAssetRe matches release asset names of the form
// `ghostcam-{device}-{version}.img.xz`, where device is one of the
// piImageDevices values and version is the release tag (e.g. v0.5.0).
// device and version are captured for routing + bookkeeping.
var piImageAssetRe = regexp.MustCompile(`^ghostcam-(zero2w|pi4|pi5)-([^/]+)\.img\.xz$`)

// trustedAssetHostPrefixes is the allow-list of host prefixes we'll
// follow when pulling a release asset. The webhook payload is HMAC-
// authenticated so we already know it came from GitHub, but the asset
// URL is still attacker-controllable in principle (anyone with push
// access to the repo could craft a release asset pointing elsewhere).
// Restricting to GitHub's own domains closes the residual SSRF surface.
var trustedAssetHostPrefixes = []string{
	"https://github.com/",
	"https://objects.githubusercontent.com/",
	"https://release-assets.githubusercontent.com/",
	"https://api.github.com/",
}

// maxPiImageSize caps the image body we'll accept from a release asset.
// Enforced via io.LimitReader. Pi images today run a few hundred MB;
// 2 GiB is the safety ceiling.
const maxPiImageSize = 2 << 30 // 2 GiB

// piImageMeta is the JSON value stored at firmware:images:{device} in Redis.
// Mirrors apitypes.FirmwareMeta for device images.
type piImageMeta struct {
	Version   string `json:"version"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// githubReleaseAsset is the subset of the GitHub release asset schema we
// read from the webhook payload.
type githubReleaseAsset struct {
	Name               string `json:"name"`
	URL                string `json:"url"`                  // API URL (works with token auth for private repos)
	BrowserDownloadURL string `json:"browser_download_url"` // public download URL (only works for public repos)
	Size               int64  `json:"size"`
}

// githubReleasePayload is the subset of the `release` webhook payload we
// act on. See https://docs.github.com/webhooks/webhook-events-and-payloads#release.
type githubReleasePayload struct {
	Action  string `json:"action"`
	Release struct {
		TagName string               `json:"tag_name"`
		Assets  []githubReleaseAsset `json:"assets"`
	} `json:"release"`
}

// githubWebhookAccepted is the body we return when the webhook is
// queued for async ingestion. The actual upload happens in a background
// goroutine — see note on ingestion latency in GithubWebhook.
type githubWebhookAccepted struct {
	Status  string `json:"status"`  // always "accepted"
	Version string `json:"version"` // release tag we queued
	Queued  int    `json:"queued"`  // number of matching assets queued
}

// piIngestionInFlight is a process-wide guard against overlapping
// ingestion runs. If GitHub redelivers a webhook (e.g. after a 5xx
// spike elsewhere on the server) we don't want two goroutines racing
// the same S3 keys. A single in-flight counter is enough: ingestion is
// idempotent on S3, so worst-case we drop a duplicate redelivery.
var piIngestionInFlight atomic.Int32

// PiImagesList handles GET /api/v1/firmware/images (public, no auth).
// Iterates the known devices, reads metadata from Redis, presigns a GET
// URL per image, and returns the union. Devices without a published
// image are omitted.
func (a *App) PiImagesList(w http.ResponseWriter, r *http.Request) {
	if a.Redis == nil || a.S3 == nil {
		writeJSON(w, http.StatusOK, apitypes.PiImagesResponse{Images: []apitypes.PiImage{}})
		return
	}

	ctx := r.Context()
	images := make([]apitypes.PiImage, 0, len(piImageDevices))

	for _, device := range piImageDevices {
		raw, err := a.Redis.Get(ctx, redisPiImageKey(device)).Result()
		if err != nil || raw == "" {
			continue
		}
		var meta piImageMeta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			slog.Warn("firmware: pi image meta unmarshal failed", "device", device, "error", err)
			continue
		}

		key := s3.PiImageKey(meta.Version, device)
		url, err := a.S3.PresignGet(ctx, key)
		if err != nil {
			slog.Warn("firmware: presign GET failed", "device", device, "version", meta.Version, "error", err)
			continue
		}

		images = append(images, apitypes.PiImage{
			Device:      device,
			Version:     meta.Version,
			DownloadURL: url,
			SizeBytes:   meta.SizeBytes,
			SHA256:      meta.SHA256,
		})
	}

	writeJSON(w, http.StatusOK, apitypes.PiImagesResponse{Images: images})
}

// GithubWebhook handles POST /api/v1/webhooks/github.
//
// Design constraints that shape this handler:
//
//   - The HTTP server's WriteTimeout is 60s (see server/main.go). A
//     single Pi image can be hundreds of MB, and three of them sequentially
//     blow past that ceiling on any residential-grade upstream.
//   - GitHub retries non-2xx webhook deliveries, which would cause us
//     to re-download + re-upload assets we've already persisted.
//
// So we validate the signature synchronously, parse + validate the
// payload synchronously, then kick off ingestion in a background
// goroutine and return 200 immediately. The goroutine uses
// context.Background() (not r.Context(), which is cancelled on return)
// and streams each asset directly from GitHub into S3 via a multipart
// uploader — no []byte buffering. Partial progress on error is
// acceptable: S3 writes are idempotent and a later redelivery just
// overwrites the same keys.
func (a *App) GithubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20)) // 10 MiB — webhook bodies are small JSON
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	if a.Config.GithubWebhookSecret != "" {
		if !verifyGithubSignature(r.Header.Get("X-Hub-Signature-256"), body, a.Config.GithubWebhookSecret) {
			slog.Warn("github webhook signature verification failed")
			// 403 (not 401): the request did present a signature, it
			// just didn't match. No challenge to reissue.
			http.Error(w, "", http.StatusForbidden)
			return
		}
	} else if a.Config.PublicURL != "" {
		slog.Error("github webhook rejected: GITHUB_WEBHOOK_SECRET not configured")
		http.Error(w, "", http.StatusForbidden)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"pong": "ghostcam"})
		return
	case "release":
		// handled below
	default:
		// Accept + ignore any other event type; GitHub retries on non-2xx.
		w.WriteHeader(http.StatusOK)
		return
	}

	var payload githubReleasePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	if payload.Action != "published" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.Release.TagName == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	if a.S3 == nil || a.Redis == nil {
		slog.Error("github webhook: S3 or Redis not configured, cannot ingest pi images")
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	// Count matching assets synchronously so the response can tell the
	// operator how many we queued. We do not download anything yet.
	matching := filterPiImageAssets(payload.Release.TagName, payload.Release.Assets)

	if piIngestionInFlight.Load() > 0 {
		slog.Warn("github webhook: ingestion already in flight, dropping redelivery",
			"version", payload.Release.TagName)
		writeJSON(w, http.StatusAccepted, githubWebhookAccepted{
			Status: "already_in_flight", Version: payload.Release.TagName,
		})
		return
	}

	// Find the camera binary asset for firmware auto-update.
	cameraAsset := findCameraAsset(payload.Release.Assets)

	// Launch ingestion in the background. We intentionally detach from
	// r.Context(): the HTTP handler is about to return and the ctx
	// would be cancelled. Use a generous timeout so a single stuck
	// download can't wedge the goroutine forever.
	piIngestionInFlight.Add(1)
	go func(piAssets []githubReleaseAsset, camAsset *githubReleaseAsset, version string) {
		defer piIngestionInFlight.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		// Ingest camera firmware binary (small, ~15 MB — do first).
		if camAsset != nil {
			if err := a.ingestCameraFirmware(ctx, version, *camAsset); err != nil {
				slog.Error("github webhook: camera firmware ingestion failed",
					"version", version, "error", err)
			}
		}

		// Ingest Pi images (large, ~300 MB each).
		if err := a.ingestPiImages(ctx, version, piAssets); err != nil {
			slog.Error("github webhook: pi image ingestion failed",
				"version", version, "error", err)
			return
		}
		slog.Info("github webhook: ingestion complete",
			"version", version, "pi_images", len(piAssets),
			"camera_firmware", camAsset != nil)
	}(matching, cameraAsset, payload.Release.TagName)

	writeJSON(w, http.StatusAccepted, githubWebhookAccepted{
		Status:  "accepted",
		Version: payload.Release.TagName,
		Queued:  len(matching),
	})
}

// filterPiImageAssets returns the subset of assets whose name matches
// the Pi image pattern for the given tag. Skips assets whose embedded
// version doesn't match the release tag — guards against a
// mistakenly-uploaded asset from a previous build slipping into a new
// release's namespace.
func filterPiImageAssets(tag string, assets []githubReleaseAsset) []githubReleaseAsset {
	out := make([]githubReleaseAsset, 0, len(piImageDevices))
	for _, asset := range assets {
		m := piImageAssetRe.FindStringSubmatch(asset.Name)
		if m == nil {
			continue
		}
		if m[2] != tag {
			slog.Warn("firmware: pi image asset version != release tag, skipping",
				"asset", asset.Name, "asset_version", m[2], "tag", tag)
			continue
		}
		out = append(out, asset)
	}
	return out
}

const cameraAssetName = "ghostcam-camera-aarch64"

// findCameraAsset returns the aarch64 camera binary asset, or nil if
// not found in the release.
func findCameraAsset(assets []githubReleaseAsset) *githubReleaseAsset {
	for i := range assets {
		if assets[i].Name == cameraAssetName {
			return &assets[i]
		}
	}
	return nil
}

// ingestCameraFirmware streams the camera binary from GitHub into S3
// and sets firmware:latest:version + firmware:latest:sha256 in Redis,
// enabling the telemetry-driven auto-update flow.
func (a *App) ingestCameraFirmware(ctx context.Context, version string, asset githubReleaseAsset) error {
	downloadURL := asset.BrowserDownloadURL
	if a.Config.GithubToken != "" && asset.URL != "" {
		downloadURL = asset.URL
	}

	if !isTrustedAssetURL(downloadURL) {
		return fmt.Errorf("untrusted camera asset URL: %s", downloadURL)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	key := s3.FirmwareKey(version)
	size, sha256hex, err := a.streamAssetToS3(ctx, client, downloadURL, key)
	if err != nil {
		return fmt.Errorf("stream camera binary to S3: %w", err)
	}

	pipe := a.Redis.Pipeline()
	pipe.Set(ctx, "firmware:latest:version", version, 0)
	pipe.Set(ctx, "firmware:latest:sha256", sha256hex, 0)
	meta, _ := json.Marshal(apitypes.FirmwareMeta{
		Version:   version,
		S3Key:     key,
		SizeBytes: int(size),
		SHA256:    sha256hex,
	})
	pipe.Set(ctx, "firmware:latest:meta", meta, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis set firmware:latest: %w", err)
	}

	slog.Info("firmware: camera binary ingested",
		"version", version, "size_bytes", size, "sha256", sha256hex)
	return nil
}

// ingestPiImages streams each matching asset directly from GitHub into
// S3, tees through a SHA-256 hasher so we record the digest without a
// second pass, and writes per-device metadata to Redis.
//
// Runs off-request in a goroutine (see GithubWebhook). Errors on an
// individual asset short-circuit ingestion — a later redelivery or
// retag will retry cleanly because S3 writes are idempotent on key.
func (a *App) ingestPiImages(ctx context.Context, tag string, assets []githubReleaseAsset) error {
	client := &http.Client{Timeout: 25 * time.Minute}

	for _, asset := range assets {
		m := piImageAssetRe.FindStringSubmatch(asset.Name)
		if m == nil {
			continue
		}
		device := m[1]
		version := m[2]

		// Prefer API URL with token auth (works for private repos).
		// Fall back to browser_download_url for public repos.
		downloadURL := asset.BrowserDownloadURL
		if a.Config.GithubToken != "" && asset.URL != "" {
			downloadURL = asset.URL
		}

		if !isTrustedAssetURL(downloadURL) {
			slog.Warn("firmware: skipping asset with untrusted URL",
				"asset", asset.Name, "url", downloadURL)
			continue
		}

		size, sha256hex, err := a.streamAssetToS3(ctx, client, downloadURL, s3.PiImageKey(version, device))
		if err != nil {
			return fmt.Errorf("%s: %w", asset.Name, err)
		}

		meta, _ := json.Marshal(piImageMeta{
			Version:   version,
			SizeBytes: size,
			SHA256:    sha256hex,
		})
		if err := a.Redis.Set(ctx, redisPiImageKey(device), meta, 0).Err(); err != nil {
			return fmt.Errorf("redis set %s: %w", redisPiImageKey(device), err)
		}

		slog.Info("firmware: pi image ingested",
			"device", device, "version", version, "size_bytes", size, "sha256", sha256hex)
	}
	_ = tag // tag is carried for log context in the caller; kept in signature for symmetry
	return nil
}

// streamAssetToS3 GETs the asset and streams it into S3 via multipart
// upload, hashing as it flows through. Never buffers the full body in
// memory — the s3 manager reads from r in chunks.
//
// Returns (size, hex sha256, nil) on success.
func (a *App) streamAssetToS3(ctx context.Context, client *http.Client, url, key string) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if a.Config.GithubToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.Config.GithubToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("asset GET %s returned %d", url, resp.StatusCode)
	}

	hasher := sha256.New()
	counter := &countingReader{}
	// Order: body → size cap → tee(hasher) → counter → uploader.
	// Hash must see the same bytes the uploader sees, so the tee sits
	// on the read path the uploader actually consumes.
	limited := io.LimitReader(resp.Body, maxPiImageSize+1)
	tee := &teeReader{src: limited, hasher: hasher, counter: counter}

	if err := a.S3.UploadStream(ctx, key, tee, "application/octet-stream"); err != nil {
		return 0, "", err
	}

	if counter.n > maxPiImageSize {
		return 0, "", fmt.Errorf("asset exceeds %d bytes", maxPiImageSize)
	}
	return counter.n, hex.EncodeToString(hasher.Sum(nil)), nil
}

// teeReader is a tiny io.Reader that updates a hasher and a byte
// counter on every successful read. Simpler than composing
// io.TeeReader + a wrapper because we need both side effects.
type teeReader struct {
	src     io.Reader
	hasher  hash.Hash
	counter *countingReader
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		// hash.Hash.Write never fails.
		_, _ = t.hasher.Write(p[:n])
		t.counter.n += int64(n)
	}
	return n, err
}

// countingReader tracks bytes that passed through a teeReader.
type countingReader struct{ n int64 }

// isTrustedAssetURL returns true if url points at one of GitHub's own
// hosts. The webhook is HMAC-authenticated so the URL itself is under
// GitHub admin control, but a lax check here would still be a
// gift-wrapped SSRF vector.
func isTrustedAssetURL(url string) bool {
	for _, p := range trustedAssetHostPrefixes {
		if strings.HasPrefix(url, p) {
			return true
		}
	}
	return false
}

// verifyGithubSignature validates the X-Hub-Signature-256 header against
// the raw request body using HMAC-SHA256. Header value is expected in
// the canonical `sha256=<hex>` form. Constant-time compare.
func verifyGithubSignature(header string, body []byte, secret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// redisPiImageKey is the Redis key holding the JSON-encoded piImageMeta
// for a given device.
func redisPiImageKey(device string) string {
	return "firmware:images:" + device
}
