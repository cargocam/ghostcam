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

// Version is set at build time via -ldflags.
var Version = "dev"

// Client is the camera's HTTP client for server communication.
type Client struct {
	http      *http.Client
	serverURL string
	apiKey    string
	deviceID  string
}

// NewClient creates a new camera HTTP client.
func NewClient(serverURL, apiKey, deviceID string) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
		serverURL: strings.TrimRight(serverURL, "/"),
		apiKey:    apiKey,
		deviceID:  deviceID,
	}
}

// PostTelemetry sends telemetry and returns pending commands.
// POST /api/v1/cameras/:id/telemetry
func (c *Client) PostTelemetry(ctx context.Context, telemetry common.TelemetryDatagram) ([]common.CameraCommand, error) {
	body := common.TelemetryPollRequest{
		Telemetry: telemetry,
		FwVersion: Version,
	}

	respBody, err := c.postJSON(ctx, fmt.Sprintf("/api/v1/cameras/%s/telemetry", c.deviceID), body)
	if err != nil {
		return nil, err
	}
	defer respBody.Close()

	var resp common.TelemetryPollResponse
	if err := json.NewDecoder(respBody).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decoding telemetry response: %w", err)
	}
	return resp.Commands, nil
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

// Provision calls POST /api/v1/cameras/provision (no auth required).
// This is a standalone function since the camera doesn't have an API key yet.
func Provision(ctx context.Context, serverURL, token, deviceSerial string) (*common.ProvisionResponse, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	serverURL = strings.TrimRight(serverURL, "/")

	body := common.ProvisionRequest{
		Token:        token,
		DeviceSerial: deviceSerial,
		FwVersion:    Version,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling provision request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/cameras/provision", serverURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("creating provision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provision POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provisioning failed: %d — %s", resp.StatusCode, string(errBody))
	}

	var result common.ProvisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding provision response: %w", err)
	}

	slog.Info("provisioned", "device_id", result.DeviceID)
	return &result, nil
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

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
