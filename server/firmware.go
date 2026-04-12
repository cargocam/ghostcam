package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/s3"
)

// piImageDevices is the fixed set of device slugs we build images for.
// Kept in sync with the `device` matrix in .github/workflows/release.yml.
var piImageDevices = []string{"zero2w", "pi4", "pi5"}

// maxPiImageSize caps the image body we'll buffer into memory during
// webhook ingestion. Images compressed with xz typically come in well
// under 1 GB per device; this upper bound protects the server against a
// malformed asset size on the release payload.
const maxPiImageSize = 2 << 30 // 2 GiB

// piImageAssetRe matches release asset names of the form
// `ghostcam-{device}-{version}.img.xz`, where device is one of the
// piImageDevices values and version is the release tag (e.g. v0.5.0).
// device and version are captured for routing + bookkeeping.
var piImageAssetRe = regexp.MustCompile(`^ghostcam-(zero2w|pi4|pi5)-([^/]+)\.img\.xz$`)

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
	BrowserDownloadURL string `json:"browser_download_url"`
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

// piImageIngestedSummary reports what a webhook ingestion moved into S3.
type piImageIngestedSummary struct {
	Device    string `json:"device"`
	Version   string `json:"version"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type githubWebhookResponse struct {
	Ingested []piImageIngestedSummary `json:"ingested"`
}

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

// GithubWebhook handles POST /api/v1/webhooks/github. Validates the
// X-Hub-Signature-256 HMAC, and for `release.published` events
// downloads each matching Pi image asset, uploads it to S3, and writes
// the metadata to Redis so PiImagesList can serve it.
//
// A missing GITHUB_WEBHOOK_SECRET fails closed (403) unless the server
// is running in a local dev config (no PublicURL), which matches the
// StripeWebhook handling pattern.
func (a *App) GithubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20)) // 10 MiB — webhook bodies are small JSON
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	if a.Config.GithubWebhookSecret != "" {
		if !verifyGithubSignature(r.Header.Get("X-Hub-Signature-256"), body, a.Config.GithubWebhookSecret) {
			slog.Warn("github webhook signature verification failed")
			http.Error(w, "", http.StatusUnauthorized)
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

	ingested, err := a.ingestPiImages(r.Context(), payload.Release.TagName, payload.Release.Assets)
	if err != nil {
		slog.Error("github webhook: pi image ingestion failed", "version", payload.Release.TagName, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("github webhook: pi image ingestion complete",
		"version", payload.Release.TagName, "ingested_count", len(ingested))
	writeJSON(w, http.StatusOK, githubWebhookResponse{Ingested: ingested})
}

// ingestPiImages walks the release assets, downloads the ones that match
// the ghostcam-{device}-{version}.img.xz pattern, streams them into S3,
// and stores per-device metadata in Redis. The version must appear in
// the asset name and match the release tag — this guards against a
// mismatched asset slipping into the wrong version namespace.
func (a *App) ingestPiImages(ctx context.Context, tag string, assets []githubReleaseAsset) ([]piImageIngestedSummary, error) {
	ingested := make([]piImageIngestedSummary, 0, len(piImageDevices))
	client := &http.Client{Timeout: 10 * time.Minute}

	for _, asset := range assets {
		m := piImageAssetRe.FindStringSubmatch(asset.Name)
		if m == nil {
			continue
		}
		device := m[1]
		version := m[2]
		if version != tag {
			slog.Warn("firmware: pi image asset version != release tag, skipping",
				"asset", asset.Name, "asset_version", version, "tag", tag)
			continue
		}

		data, sha256hex, err := downloadPiImage(ctx, client, asset.BrowserDownloadURL)
		if err != nil {
			return ingested, fmt.Errorf("download %s: %w", asset.Name, err)
		}

		key := s3.PiImageKey(version, device)
		if err := a.S3.Upload(ctx, key, data, "application/octet-stream"); err != nil {
			return ingested, fmt.Errorf("s3 upload %s: %w", key, err)
		}

		size := int64(len(data))
		meta, _ := json.Marshal(piImageMeta{
			Version:   version,
			SizeBytes: size,
			SHA256:    sha256hex,
		})
		if err := a.Redis.Set(ctx, redisPiImageKey(device), meta, 0).Err(); err != nil {
			return ingested, fmt.Errorf("redis set %s: %w", redisPiImageKey(device), err)
		}

		slog.Info("firmware: pi image ingested",
			"device", device, "version", version, "size_bytes", size, "sha256", sha256hex)
		ingested = append(ingested, piImageIngestedSummary{
			Device:    device,
			Version:   version,
			SizeBytes: size,
			SHA256:    sha256hex,
		})
	}

	return ingested, nil
}

// downloadPiImage GETs the asset, buffers it into memory (bounded by
// maxPiImageSize), and returns the bytes + hex-encoded SHA-256.
// GitHub public release assets do not require auth; for private repos a
// caller would need to inject an Authorization header here.
func downloadPiImage(ctx context.Context, client *http.Client, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("asset GET %s returned %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPiImageSize+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxPiImageSize {
		return nil, "", fmt.Errorf("asset exceeds %d bytes", maxPiImageSize)
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
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
