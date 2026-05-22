package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/common"
)

// Version is set at build time via -ldflags "-X main.Version=vX.Y.Z".
// Dev builds keep "dev" which skips firmware update checks.
var Version = "dev"

// Client is the camera's HTTP client for server communication.
type Client struct {
	http      *http.Client
	serverURL string
	identity  *Identity
	deviceID  string
}

// NewClient creates a new camera HTTP client. Requests are authenticated
// via ed25519 signature using the camera's permanent identity keypair.
func NewClient(serverURL, deviceID string, identity *Identity) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
		serverURL: strings.TrimRight(serverURL, "/"),
		identity:  identity,
		deviceID:  deviceID,
	}
}

// PostTelemetry sends telemetry and returns the full poll response
// (commands plus side-band signals like WHIPSessionMissing). Callers that
// only need commands can read resp.Commands.
//
// rollbackEventJSON, if non-empty, is the raw contents of
// /var/ghostcam/rollback_event.json — surfaced once to the server when
// ExecStartPre took the rollback branch. The caller is responsible for
// deleting the on-disk marker only after this call succeeds.
// POST /api/v1/cameras/:id/telemetry
func (c *Client) PostTelemetry(ctx context.Context, telemetry common.TelemetryDatagram, rollbackEventJSON string) (common.TelemetryPollResponse, error) {
	// Drain any DiagBundles captured since the previous poll. The drain
	// clears the pending slice; if the post fails we accept the loss
	// (#119 design note: bundles are explicit operator requests, easy
	// to reissue).
	bundles := drainPendingDiagBundles()

	body := common.TelemetryPollRequest{
		Telemetry:     telemetry,
		FwVersion:     Version,
		RollbackEvent: rollbackEventJSON,
		DiagBundles:   bundles,
	}

	respBody, err := c.postJSON(ctx, fmt.Sprintf("/api/v1/cameras/%s/telemetry", c.deviceID), body)
	if err != nil {
		return common.TelemetryPollResponse{}, err
	}
	defer respBody.Close()

	var resp common.TelemetryPollResponse
	if err := json.NewDecoder(respBody).Decode(&resp); err != nil {
		return common.TelemetryPollResponse{}, fmt.Errorf("decoding telemetry response: %w", err)
	}
	return resp, nil
}

// RequestPresignedURLs requests presigned PUT URLs and confirms previously uploaded segments.
// POST /api/v1/cameras/:id/presign
func (c *Client) RequestPresignedURLs(ctx context.Context, count uint32, uploaded []common.UploadedSegment) (*common.PresignResponse, error) {
	body := common.PresignRequest{
		Count:    count,
		Uploaded: uploaded,
	}

	respBody, err := c.postJSON(ctx, fmt.Sprintf("/api/v1/cameras/%s/presign", c.deviceID), body)
	if err != nil {
		return nil, err
	}
	defer respBody.Close()

	var resp common.PresignResponse
	if err := json.NewDecoder(respBody).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decoding presign response: %w", err)
	}
	return &resp, nil
}

// S3UploadError is returned by UploadFile when S3 returns a non-2xx status.
type S3UploadError struct {
	StatusCode int
}

func (e *S3UploadError) Error() string {
	return fmt.Sprintf("S3 PUT returned %d", e.StatusCode)
}

// IsClientError returns true for 4xx errors (expired URL, auth failure, etc.).
func (e *S3UploadError) IsClientError() bool {
	return e.StatusCode/100 == 4
}

// UploadFile uploads segment data to a presigned S3 PUT URL.
func (c *Client) UploadFile(ctx context.Context, presignedURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating S3 PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "video/mp2t")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("S3 PUT failed: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode/100 != 2 {
		return &S3UploadError{StatusCode: resp.StatusCode}
	}
	return nil
}

// Provision calls POST /api/v1/cameras/provision with the camera's
// ed25519 public key. No secret is returned — the server just registers
// the public key (like adding to SSH authorized_keys).
func Provision(ctx context.Context, serverURL, token, deviceSerial string, identity *Identity) error {
	client := &http.Client{Timeout: 30 * time.Second}
	serverURL = strings.TrimRight(serverURL, "/")

	body := common.ProvisionRequest{
		Token:        token,
		DeviceSerial: deviceSerial,
		PublicKey:    identity.PublicKeyHex(),
		FwVersion:    Version,
		// Best-effort SIM IMSI lookup so the server can record which
		// cameras carry a Ghostcam-managed Soracom SIM (#74). Build-
		// tagged: real mmcli call on linux + !synthetic, no-op stub
		// elsewhere. ReadSIMImsi never blocks more than ~3 s and
		// returns "" on any failure, so provisioning isn't gated on
		// modem state.
		SIMImsi: ReadSIMImsi(ctx),
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling provision v2 request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/cameras/provision", serverURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating provision v2 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("provision v2 POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("provisioning v2 failed: %d — %s", resp.StatusCode, string(errBody))
	}

	slog.Info("provisioned", "device_id", identity.DeviceID)
	return nil
}

// UploadInit POSTs the fMP4 init segment (init.mp4) to the server, which
// stores it at s3://<bucket>/<deviceID>/init.mp4. The HLS manifest's
// #EXT-X-MAP tag points at that key; .m4s media segments cannot be played
// without a current init in S3. Re-uploads on every encoder restart (init
// data may change with codec params), but the body is ~1-2 KB so cost is
// trivial.
func (c *Client) UploadInit(ctx context.Context, data []byte) error {
	url := fmt.Sprintf("%s/api/v1/cameras/%s/init", c.serverURL, c.deviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating init upload request: %w", err)
	}
	req.Header.Set("Content-Type", "video/mp4")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("init upload POST failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("init upload returned %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}

// postJSON is a helper that POSTs JSON with bearer auth and returns the response body.
func (c *Client) postJSON(ctx context.Context, path string, body any) (io.ReadCloser, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	url := c.serverURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP POST %s failed: %w", path, err)
	}

	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP POST %s returned %d: %s", path, resp.StatusCode, string(errBody))
	}

	return resp.Body, nil
}

// setAuth sets the Authorization header using ed25519 signature.
func (c *Client) setAuth(req *http.Request) {
	SignRequest(req, c.deviceID, c.identity.PrivateKey)
}
